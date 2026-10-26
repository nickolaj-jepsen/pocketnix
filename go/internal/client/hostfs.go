package client

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// HostFolders replaces the folders the agent mounts, as the app does on connect.
func (c *Control) HostFolders(folders []protocol.HostFolder) error {
	if folders == nil {
		folders = []protocol.HostFolder{}
	}
	_, err := c.request(protocol.Request{Type: protocol.TypeHostFolders, Folders: folders})
	return err
}

// HostFS opens a hostfs stream for the folder name, for a HostFSServer to serve.
func (c *Client) HostFS(name string) (net.Conn, error) {
	conn, _, err := c.open(protocol.Hello{Stream: protocol.StreamHostFS, Name: name})
	return conn, err
}

// HostFSServer answers a folder's hostfs streams from a plain directory, as
// the app answers them from an Android folder; any number of streams share
// its open files.
type HostFSServer struct {
	root     *os.Root
	writable bool

	mu      sync.Mutex
	next    int64
	handles map[int64]*os.File
}

// NewHostFSServer serves dir, refusing changes unless writable.
func NewHostFSServer(dir string, writable bool) (*HostFSServer, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &HostFSServer{root: root, writable: writable, handles: map[int64]*os.File{}}, nil
}

// Close closes the directory and every open file.
func (s *HostFSServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, f := range s.handles {
		f.Close()
		delete(s.handles, id)
	}
	return s.root.Close()
}

// Serve answers conn's requests, each as it comes, until the stream ends.
func (s *HostFSServer) Serve(conn net.Conn) error {
	var wmu sync.Mutex
	var wg sync.WaitGroup
	defer wg.Wait()
	r := bufio.NewReaderSize(conn, 64<<10)
	for {
		var req protocol.HostFSRequest
		typ, id, data, err := protocol.ReadHostFS(r, &req)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if typ != protocol.FrameHostFSRequest {
			return fmt.Errorf("unexpected frame %#x", typ)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			reply, out := s.handle(req, data)
			wmu.Lock()
			defer wmu.Unlock()
			if protocol.WriteHostFS(conn, protocol.FrameHostFSReply, id, reply, out) != nil {
				conn.Close()
			}
		}()
	}
}

func (s *HostFSServer) handle(req protocol.HostFSRequest, data []byte) (protocol.HostFSReply, []byte) {
	reply, out, err := s.do(req, data)
	if err != nil {
		reply = protocol.HostFSReply{Error: err.Error(), Errno: errnoName(err)}
	}
	return reply, out
}

func errnoName(err error) string {
	var e syscall.Errno
	switch {
	case errors.As(err, &e):
		return unix.ErrnoName(e)
	case errors.Is(err, fs.ErrNotExist):
		return "ENOENT"
	case errors.Is(err, fs.ErrExist):
		return "EEXIST"
	case errors.Is(err, fs.ErrPermission):
		return "EACCES"
	}
	return "EIO"
}

func rel(p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	c := path.Clean(p)
	if path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("bad path %q: %w", p, syscall.EINVAL)
	}
	return filepath.FromSlash(c), nil
}

func (s *HostFSServer) info(p string) (protocol.FileInfo, error) {
	rp, err := rel(p)
	if err != nil {
		return protocol.FileInfo{}, err
	}
	fi, err := s.root.Stat(rp)
	if err != nil {
		return protocol.FileInfo{}, err
	}
	name := path.Base(p)
	if p == "" {
		name = ""
	}
	return protocol.FileInfo{
		Path: p, Name: name, Dir: fi.IsDir(), Size: fi.Size(),
		MTime:    fi.ModTime().UnixMilli(),
		Writable: s.writable && fi.Mode().Perm()&0o200 != 0,
	}, nil
}

func (s *HostFSServer) file(id int64) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.handles[id]
	if f == nil {
		return nil, syscall.EBADF
	}
	return f, nil
}

func (s *HostFSServer) do(req protocol.HostFSRequest, data []byte) (protocol.HostFSReply, []byte, error) {
	var reply protocol.HostFSReply
	switch req.Op {
	case protocol.FileStat, protocol.FileList, protocol.FileRead, protocol.FileOpen, protocol.FileClose, protocol.FileSync:
	default:
		if !s.writable {
			return reply, nil, syscall.EROFS
		}
	}
	switch req.Op {
	case protocol.FileStat:
		fi, err := s.info(req.Path)
		reply.File = &fi
		return reply, nil, err
	case protocol.FileList:
		rp, err := rel(req.Path)
		if err != nil {
			return reply, nil, err
		}
		d, err := s.root.Open(rp)
		if err != nil {
			return reply, nil, err
		}
		names, err := d.Readdirnames(-1)
		d.Close()
		if err != nil {
			return reply, nil, err
		}
		reply.Files = []protocol.FileInfo{}
		for _, n := range names {
			// What an Android folder holds: files and directories, no links.
			fi, err := s.info(path.Join(req.Path, n))
			if err == nil {
				reply.Files = append(reply.Files, fi)
			}
		}
		return reply, nil, nil
	case protocol.FileCreate:
		p := path.Join(req.Path, req.Name)
		rp, err := rel(p)
		if err != nil || strings.Contains(req.Name, "/") {
			return reply, nil, syscall.EINVAL
		}
		if req.Dir {
			err = s.root.Mkdir(rp, 0o755)
		} else {
			var f *os.File
			if f, err = s.root.OpenFile(rp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); err == nil {
				f.Close()
			}
		}
		if err != nil {
			return reply, nil, err
		}
		fi, err := s.info(p)
		reply.File = &fi
		return reply, nil, err
	case protocol.FileDelete:
		rp, err := rel(req.Path)
		if err == nil && rp == "." {
			err = syscall.EBUSY
		}
		if err != nil {
			return reply, nil, err
		}
		return reply, nil, s.root.RemoveAll(rp)
	case protocol.FileRename:
		from, err := rel(req.Path)
		if err != nil {
			return reply, nil, err
		}
		to, err := rel(req.To)
		if err != nil {
			return reply, nil, err
		}
		if _, err := s.root.Lstat(to); err == nil {
			return reply, nil, syscall.EEXIST
		}
		if err := s.root.Rename(from, to); err != nil {
			return reply, nil, err
		}
		fi, err := s.info(req.To)
		reply.File = &fi
		return reply, nil, err
	case protocol.FileOpen:
		rp, err := rel(req.Path)
		if err != nil {
			return reply, nil, err
		}
		flags := os.O_RDONLY
		switch req.Mode {
		case "r":
		case "rw":
			flags = os.O_RDWR
		case "rwt":
			flags = os.O_RDWR | os.O_TRUNC
		default:
			return reply, nil, fmt.Errorf("open mode %q: %w", req.Mode, syscall.EINVAL)
		}
		if flags != os.O_RDONLY && !s.writable {
			return reply, nil, syscall.EROFS
		}
		f, err := s.root.OpenFile(rp, flags, 0)
		if err != nil {
			return reply, nil, err
		}
		s.mu.Lock()
		s.next++
		reply.Handle = s.next
		s.handles[s.next] = f
		s.mu.Unlock()
		return reply, nil, nil
	case protocol.FileRead:
		f, err := s.file(req.Handle)
		if err != nil {
			return reply, nil, err
		}
		buf := make([]byte, min(max(req.Size, 0), protocol.MaxHostFSData))
		n, err := f.ReadAt(buf, req.Offset)
		if errors.Is(err, io.EOF) {
			err = nil
		}
		return reply, buf[:n], err
	case protocol.FileWrite:
		f, err := s.file(req.Handle)
		if err != nil {
			return reply, nil, err
		}
		n, err := f.WriteAt(data, req.Offset)
		reply.Size = int64(n)
		return reply, nil, err
	case protocol.FileTruncate:
		if req.Handle != 0 {
			f, err := s.file(req.Handle)
			if err != nil {
				return reply, nil, err
			}
			return reply, nil, f.Truncate(req.Size)
		}
		rp, err := rel(req.Path)
		if err != nil {
			return reply, nil, err
		}
		f, err := s.root.OpenFile(rp, os.O_WRONLY, 0)
		if err != nil {
			return reply, nil, err
		}
		defer f.Close()
		return reply, nil, f.Truncate(req.Size)
	case protocol.FileSync:
		f, err := s.file(req.Handle)
		if err != nil {
			return reply, nil, err
		}
		return reply, nil, f.Sync()
	case protocol.FileClose:
		s.mu.Lock()
		f := s.handles[req.Handle]
		delete(s.handles, req.Handle)
		s.mu.Unlock()
		if f == nil {
			return reply, nil, syscall.EBADF
		}
		return reply, nil, f.Close()
	}
	return reply, nil, fmt.Errorf("unknown operation %q: %w", req.Op, syscall.ENOSYS)
}
