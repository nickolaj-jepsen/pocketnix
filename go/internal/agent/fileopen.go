package agent

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// openMode is an Android open mode (ParcelFileDescriptor.parseMode).
type openMode struct{ write, truncate bool }

func parseMode(m string) (openMode, error) {
	switch m {
	case "r":
		return openMode{}, nil
	case "w", "wt", "rwt":
		return openMode{write: true, truncate: true}, nil
	case "wa", "rw":
		return openMode{write: true}, nil
	}
	return openMode{}, fmt.Errorf("bad open mode %q", m)
}

// openFile is one FileOpen stream's file. Reads come from the original until
// the first write, which copies it (unless the mode truncates) into a
// temporary file next to it; every later read and write goes there.
type openFile struct {
	s     *Server
	root  *os.Root
	path  string
	mode  openMode
	owner *fileOwner

	src *os.File // nil when the file doesn't exist yet

	tmp     *os.File
	tmpPath string
	forget  func()
}

// serveOpen serves a FileOpen stream (protocol.FileOpen).
func (s *Server) serveOpen(c net.Conn, root *os.Root, p, mode string, owner *fileOwner) {
	m, err := parseMode(mode)
	if err != nil {
		reject(c, err.Error())
		return
	}
	if p == "" && m.write {
		reject(c, "can't write the root")
		return
	}
	o := &openFile{s: s, root: root, path: p, mode: m, owner: owner}
	defer o.discard()
	fi := protocol.FileInfo{Path: p, Name: path.Base(p), Writable: true}
	src, err := root.Open(osPath(p))
	switch {
	case err == nil:
		o.src = src
		st, err := src.Stat()
		if err != nil || st.IsDir() {
			reject(c, fmt.Sprintf("%s: not a file", p))
			return
		}
		fi = info(p, st)
	case errors.Is(err, fs.ErrNotExist) && m.write:
	default:
		reject(c, err.Error())
		return
	}
	if m.truncate {
		fi.Size = 0
	}
	reply := s.hello()
	reply.File = &fi
	if protocol.WriteJSON(c, reply) != nil {
		return
	}
	r := bufio.NewReaderSize(c, 64<<10)
	for {
		typ, payload, err := protocol.ReadFrame(r)
		if err != nil {
			// The app went away without a commit: its writes are discarded.
			return
		}
		var out []byte
		switch typ {
		case protocol.FileFrameRead:
			out, err = o.read(payload)
		case protocol.FileFrameWrite:
			err = o.write(payload)
		case protocol.FileFrameSync:
			if o.tmp != nil {
				err = o.tmp.Sync()
			}
		case protocol.FileFrameCommit:
			err = o.commit()
		default:
			err = fmt.Errorf("unknown file frame %#x", typ)
		}
		if err != nil {
			protocol.WriteFrame(c, protocol.FileFrameError, []byte(err.Error()))
			return
		}
		if protocol.WriteFrame(c, protocol.FileFrameOK, out) != nil || typ == protocol.FileFrameCommit {
			return
		}
	}
}

func offset(b []byte) (int64, error) {
	off := binary.BigEndian.Uint64(b)
	if off > math.MaxInt64 {
		return 0, fmt.Errorf("offset %d out of range", off)
	}
	return int64(off), nil
}

func (o *openFile) read(b []byte) ([]byte, error) {
	if len(b) != 12 {
		return nil, fmt.Errorf("read request is %d bytes, want 12", len(b))
	}
	off, err := offset(b)
	if err != nil {
		return nil, err
	}
	n := min(int(binary.BigEndian.Uint32(b[8:])), protocol.MaxFramePayload)
	f := o.tmp
	if f == nil && !o.mode.truncate {
		f = o.src
	}
	if f == nil {
		return nil, nil
	}
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, off)
	if err == io.EOF {
		err = nil
	}
	return buf[:got], err
}

func (o *openFile) write(b []byte) error {
	if !o.mode.write {
		return errors.New("opened read-only")
	}
	if len(b) < 8 {
		return errors.New("short write request")
	}
	off, err := offset(b)
	if err != nil {
		return err
	}
	if err := o.ensureTemp(); err != nil {
		return err
	}
	_, err = o.tmp.WriteAt(b[8:], off)
	return err
}

func (o *openFile) ensureTemp() error {
	if o.tmp != nil {
		return nil
	}
	f, tmp, forget, err := o.s.newTemp(o.root, o.path)
	if err != nil {
		return err
	}
	o.tmp, o.tmpPath, o.forget = f, tmp, forget
	if o.src != nil && !o.mode.truncate {
		// A copy within the guest's disk (copy_file_range), not over the stream.
		if _, err := io.Copy(f, io.NewSectionReader(o.src, 0, math.MaxInt64)); err != nil {
			return err
		}
	}
	return nil
}

// commit puts the written file in place; an open that changed nothing leaves
// the file alone.
func (o *openFile) commit() error {
	if o.tmp == nil {
		if o.src != nil && !o.mode.truncate {
			return nil
		}
		if err := o.ensureTemp(); err != nil {
			return err
		}
	}
	f := o.tmp
	o.tmp = nil
	err := install(o.root, f, o.tmpPath, o.path, o.owner)
	if err != nil {
		o.root.Remove(osPath(o.tmpPath))
	}
	o.forget()
	return err
}

// discard drops uncommitted writes.
func (o *openFile) discard() {
	if o.tmp != nil {
		o.tmp.Close()
		o.root.Remove(osPath(o.tmpPath))
		o.forget()
	}
	if o.src != nil {
		o.src.Close()
	}
}

// install puts the temporary file f (at tmp) in place of p, synced and
// closed: a replaced file keeps its mode and owner, a new one is 0644 and
// owner's. f is closed either way.
func install(root *os.Root, f *os.File, tmp, p string, owner *fileOwner) error {
	mode := fs.FileMode(0o644)
	keep := owner
	if fi, err := root.Stat(osPath(p)); err == nil {
		if fi.IsDir() {
			f.Close()
			return fmt.Errorf("%s is a directory", p)
		}
		mode = fi.Mode().Perm()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && os.Getuid() == 0 {
			keep = &fileOwner{int(st.Uid), int(st.Gid)}
		}
	}
	var err error
	// On the descriptor: the path could be swapped under a chown by name.
	if keep != nil {
		err = f.Chown(keep.uid, keep.gid)
	}
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(osPath(tmp), osPath(p))
	}
	return err
}

// Temporary files under the file provider's root are recorded in the state
// directory while they exist, so the next start removes the ones a crash left
// (an app crash powers the guest off mid-write) without walking the tree.

// tempPattern matches tempName's names: the sweep removes nothing else.
var tempPattern = regexp.MustCompile(`^\..+\.pocketnix-[0-9]+$`)

func tempName(p string) string {
	return path.Join(path.Dir(p), fmt.Sprintf(".%s.pocketnix-%d", path.Base(p), time.Now().UnixNano()))
}

type tempRecord struct {
	Root string `json:"root"` // the root's Name()
	Path string `json:"path"`
}

func (s *Server) tempsDir() string { return filepath.Join(s.StateDir, "file-temps") }

// newTemp creates an empty temporary file next to p; forget drops its record
// once it's renamed or removed.
func (s *Server) newTemp(root *os.Root, p string) (f *os.File, tmp string, forget func(), err error) {
	tmp = tempName(p)
	forget = func() {}
	if s.StateDir != "" {
		rec, err := json.Marshal(tempRecord{Root: root.Name(), Path: tmp})
		if err != nil {
			return nil, "", nil, err
		}
		if err := os.MkdirAll(s.tempsDir(), 0o700); err != nil {
			return nil, "", nil, err
		}
		r, err := os.CreateTemp(s.tempsDir(), "*.json")
		if err != nil {
			return nil, "", nil, err
		}
		_, err = r.Write(rec)
		if cerr := r.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(r.Name())
			return nil, "", nil, err
		}
		forget = func() { os.Remove(r.Name()) }
	}
	f, err = root.OpenFile(osPath(tmp), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		forget()
		return nil, "", nil, err
	}
	return f, tmp, forget, nil
}

// SweepTemps removes the temporary files an earlier run recorded and never
// finished. Records for another file root (the config changed) are dropped.
func (s *Server) SweepTemps() {
	if s.StateDir == "" {
		return
	}
	entries, err := os.ReadDir(s.tempsDir())
	if err != nil || len(entries) == 0 {
		return
	}
	root, err := s.openFilesRoot()
	if err != nil {
		log.Printf("sweep temporary files: %v (keeping the records)", err)
		return
	}
	defer root.Close()
	for _, e := range entries {
		name := filepath.Join(s.tempsDir(), e.Name())
		var rec tempRecord
		if b, err := os.ReadFile(name); err == nil && json.Unmarshal(b, &rec) == nil &&
			rec.Root == root.Name() && tempPattern.MatchString(path.Base(rec.Path)) {
			if err := root.Remove(osPath(rec.Path)); err == nil {
				log.Printf("removed %s, left by an unfinished write", rec.Path)
			}
		}
		os.Remove(name)
	}
}
