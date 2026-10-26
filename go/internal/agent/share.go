package agent

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// Android's share sheet delivers text and files into the share directory,
// then runs the share command on each.

// shareDir opens where shares land, made if missing, and returns it with its
// path: absolute, or relative to the session user's home.
func (s *Server) shareDir() (*os.Root, string, error) {
	cfg := s.config()
	dir := cfg.Share.Directory
	if dir == "" {
		dir = "Inbox"
	}
	home := userHome(cfg.User)
	if !filepath.IsAbs(dir) {
		if home == "" {
			return nil, "", fmt.Errorf("share directory %q: the session user has no home", dir)
		}
		dir = filepath.Join(home, dir)
	}
	r, err := configuredDir(home, dir, true, s.owner())
	return r, dir, err
}

// serveShare stores one shared file: Hello.Size bytes after the agent's Hello.
func (s *Server) serveShare(c net.Conn, h protocol.Hello) {
	name := filepath.Base(h.Name)
	if validName(name) != nil {
		name = "shared"
	}
	ready := func() error { return protocol.WriteJSON(c, s.hello()) }
	p, err := s.deliver(name, ready, io.LimitReader(c, h.Size), h.Size)
	if err != nil {
		reject(c, err.Error())
		return
	}
	protocol.WriteJSON(c, protocol.Done{Type: protocol.TypeDone, Path: p})
}

// shareText stores shared text as a file, like a shared file.
func (s *Server) shareText(text string) (string, error) {
	name := "shared-" + time.Now().Format("20060102-150405") + ".txt"
	return s.deliver(name, nil, strings.NewReader(text), int64(len(text)))
}

// deliver writes size bytes from r into the share directory under a free
// name like name, and runs the share command on it. ready, when set, runs
// once the file exists and before r is read.
func (s *Server) deliver(name string, ready func() error, r io.Reader, size int64) (string, error) {
	root, dir, err := s.shareDir()
	if err != nil {
		return "", err
	}
	defer root.Close()
	fi, err := create(root, "", name, false, s.owner())
	if err != nil {
		return "", err
	}
	if ready != nil {
		if err := ready(); err != nil {
			root.Remove(fi.Path)
			return "", err
		}
	}
	f, err := root.OpenFile(fi.Path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		root.Remove(fi.Path)
		return "", err
	}
	full := filepath.Join(dir, fi.Path)
	return full, s.runShareCommand(full)
}

// runShareCommand runs the share command through /bin/sh as the session
// user, the delivered file its last argument ("$1"), in the background: a
// share shouldn't wait on whatever it starts.
func (s *Server) runShareCommand(file string) error {
	cfg := s.config()
	if cfg.Share.Command == "" {
		return nil
	}
	u, err := sessionUser(cfg.User)
	if err != nil {
		return err
	}
	cmd := exec.Command("/bin/sh", "-c", cfg.Share.Command+` "$1"`, "sh", file)
	cmd.Dir = u.home
	cmd.Env = sessionEnv(u, "/bin/sh")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if u.uid != os.Getuid() {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(u.uid), Gid: uint32(u.gid), Groups: u.groups}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
