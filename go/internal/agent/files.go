package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// The app's file provider (Android's DocumentsProvider) and share sheet
// reach the guest's files through these streams. The agent runs as root, so
// every operation goes through an os.Root on the configured directory (no
// path escapes it, symlinks included), and what it creates belongs to the
// session user.

func (s *Server) serveFile(c net.Conn, h protocol.Hello) {
	rootDir, err := s.filesRoot()
	if err != nil {
		reject(c, err.Error())
		return
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		reject(c, err.Error())
		return
	}
	defer root.Close()
	p, err := cleanRel(h.Path)
	if err != nil {
		reject(c, err.Error())
		return
	}
	owner := s.owner()
	reply := s.hello()
	switch h.Op {
	case protocol.FileStat:
		fi, err := stat(root, p)
		if err != nil {
			reject(c, err.Error())
			return
		}
		reply.File = &fi
		protocol.WriteJSON(c, reply)
	case protocol.FileList:
		entries, err := list(root, p)
		if err != nil {
			reject(c, err.Error())
			return
		}
		reply.Size = int64(len(entries))
		if protocol.WriteJSON(c, reply) != nil {
			return
		}
		for _, e := range entries {
			if protocol.WriteJSON(c, e) != nil {
				return
			}
		}
	case protocol.FileRead:
		f, err := root.Open(osPath(p))
		if err != nil {
			reject(c, err.Error())
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil || fi.IsDir() {
			reject(c, fmt.Sprintf("%s: not a file", p))
			return
		}
		reply.Size = fi.Size()
		if protocol.WriteJSON(c, reply) != nil {
			return
		}
		io.CopyN(c, f, fi.Size())
	case protocol.FileWrite:
		if p == "" {
			reject(c, "can't write the root")
			return
		}
		if protocol.WriteJSON(c, reply) != nil {
			return
		}
		if err := writeFile(root, p, c, h.Size, owner); err != nil {
			reject(c, err.Error())
			return
		}
		protocol.WriteJSON(c, protocol.Done{Type: protocol.TypeDone, Path: p})
	case protocol.FileCreate:
		fi, err := create(root, p, h.Name, h.Dir, owner)
		if err != nil {
			reject(c, err.Error())
			return
		}
		reply.File = &fi
		protocol.WriteJSON(c, reply)
	case protocol.FileDelete:
		if p == "" {
			reject(c, "can't delete the root")
			return
		}
		if err := root.RemoveAll(osPath(p)); err != nil {
			reject(c, err.Error())
			return
		}
		protocol.WriteJSON(c, reply)
	case protocol.FileRename:
		if err := validName(h.Name); err != nil || p == "" {
			reject(c, fmt.Sprintf("can't rename %q to %q", p, h.Name))
			return
		}
		to := path.Join(path.Dir(p), h.Name)
		if _, err := root.Lstat(osPath(to)); err == nil {
			reject(c, to+" exists")
			return
		}
		if err := root.Rename(osPath(p), osPath(to)); err != nil {
			reject(c, err.Error())
			return
		}
		fi, err := stat(root, to)
		if err != nil {
			reject(c, err.Error())
			return
		}
		reply.File = &fi
		protocol.WriteJSON(c, reply)
	default:
		reject(c, fmt.Sprintf("unknown file operation %q", h.Op))
	}
}

// cleanRel validates a provider path: slash-separated, relative, no "..".
func cleanRel(p string) (string, error) {
	if p == "" || p == "." {
		return "", nil
	}
	c := path.Clean(p)
	if path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("bad path %q", p)
	}
	return c, nil
}

func osPath(p string) string {
	if p == "" {
		return "."
	}
	return filepath.FromSlash(p)
}

func validName(n string) error {
	if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\x00") {
		return fmt.Errorf("bad name %q", n)
	}
	return nil
}

func info(p string, fi fs.FileInfo) protocol.FileInfo {
	name := path.Base(p)
	if p == "" {
		name = ""
	}
	return protocol.FileInfo{
		Path: p, Name: name, Dir: fi.IsDir(), Size: fi.Size(),
		MTime:    fi.ModTime().UnixMilli(),
		Writable: fi.Mode().Perm()&0o200 != 0,
	}
}

func stat(root *os.Root, p string) (protocol.FileInfo, error) {
	fi, err := root.Stat(osPath(p))
	if err != nil {
		return protocol.FileInfo{}, err
	}
	return info(p, fi), nil
}

func list(root *os.Root, p string) ([]protocol.FileInfo, error) {
	d, err := root.Open(osPath(p))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	des, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.FileInfo, 0, len(des))
	for _, de := range des {
		child := path.Join(p, de.Name())
		// Stat, not Lstat: a symlink shows as what it points to, or not at all
		// when that is outside the root or gone.
		fi, err := root.Stat(osPath(child))
		if err != nil {
			continue
		}
		out = append(out, info(child, fi))
	}
	return out, nil
}

// owner is who files the agent creates belong to; nil keeps them the agent's.
type fileOwner struct{ uid, gid int }

func (s *Server) owner() *fileOwner {
	if os.Getuid() != 0 {
		return nil
	}
	u, err := sessionUser(s.config().User)
	if err != nil || u.uid == 0 {
		return nil
	}
	return &fileOwner{u.uid, u.gid}
}

func (o *fileOwner) chown(root *os.Root, p string) error {
	if o == nil {
		return nil
	}
	return root.Lchown(osPath(p), o.uid, o.gid)
}

// writeFile replaces p with r's bytes through a temporary file, so a failed
// transfer leaves the old contents. A new file is 0644; an existing one keeps
// its mode.
func writeFile(root *os.Root, p string, r io.Reader, size int64, owner *fileOwner) error {
	mode := fs.FileMode(0o644)
	// A replaced file keeps its owner; a new one is the session user's.
	keep := owner
	if fi, err := root.Stat(osPath(p)); err == nil {
		if fi.IsDir() {
			return fmt.Errorf("%s is a directory", p)
		}
		mode = fi.Mode().Perm()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && os.Getuid() == 0 {
			keep = &fileOwner{int(st.Uid), int(st.Gid)}
		}
	}
	tmp := path.Join(path.Dir(p), fmt.Sprintf(".%s.pocketnix-%d", path.Base(p), time.Now().UnixNano()))
	f, err := root.OpenFile(osPath(tmp), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, size))
	if err == nil && n != size {
		// The app's upload failed midway: keep the old contents.
		err = fmt.Errorf("got %d of %d bytes", n, size)
	}
	// On the descriptor: the path could be swapped under a chown by name.
	if err == nil && keep != nil {
		err = f.Chown(keep.uid, keep.gid)
	}
	if err == nil {
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(osPath(tmp), osPath(p))
	}
	if err != nil {
		root.Remove(osPath(tmp))
	}
	return err
}

// create makes name (or "name (n)" when taken) in directory dir.
func create(root *os.Root, dir, name string, isDir bool, owner *fileOwner) (protocol.FileInfo, error) {
	if err := validName(name); err != nil {
		return protocol.FileInfo{}, err
	}
	stem, ext := name, ""
	if !isDir {
		if i := strings.LastIndexByte(name, '.'); i > 0 {
			stem, ext = name[:i], name[i:]
		}
	}
	for n := 0; n < 1000; n++ {
		candidate := name
		if n > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		p := path.Join(dir, candidate)
		var err error
		if isDir {
			err = root.Mkdir(osPath(p), 0o755)
		} else {
			var f *os.File
			if f, err = root.OpenFile(osPath(p), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); err == nil {
				f.Close()
			}
		}
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return protocol.FileInfo{}, err
		}
		if err := owner.chown(root, p); err != nil {
			return protocol.FileInfo{}, err
		}
		return stat(root, p)
	}
	return protocol.FileInfo{}, fmt.Errorf("no free name like %q in %q", name, dir)
}
