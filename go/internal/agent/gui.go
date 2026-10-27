package agent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// serveGUI splices the stream to the compositor's GUI socket. It never reads
// the GUI protocol: the compositor and the app speak it end to end.
func (s *Server) serveGUI(c net.Conn) {
	cfg := s.config()
	if cfg.GUI == nil || !cfg.GUI.Enable {
		reject(c, "GUI is not enabled")
		return
	}
	g, err := dialGUI(cfg)
	if err != nil {
		reject(c, err.Error())
		return
	}
	defer g.Close()
	if protocol.WriteJSON(c, s.hello()) != nil {
		return
	}
	splice(c, g)
}

// guiSocket returns the session user's runtime directory and the GUI
// socket's path.
func guiSocket(cfg *File) (runtime, path string, err error) {
	u, err := sessionUser(cfg.User)
	if err != nil {
		return "", "", err
	}
	runtime = runtimeDir(u.uid)
	path = cfg.GUISocket
	if path == "" {
		path = "pocketnix-gui.sock"
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(runtime, path)
	}
	return runtime, filepath.Clean(path), nil
}

// dialGUI connects to the GUI socket. The runtime directory is the user's,
// so the socket is opened through its os.Root and connected to by that
// descriptor: nothing the user puts there can lead root's connection to
// another socket.
func dialGUI(cfg *File) (net.Conn, error) {
	runtime, p, err := guiSocket(cfg)
	if err != nil {
		return nil, err
	}
	fail := func(err error) error { return fmt.Errorf("GUI socket %s: %w", p, err) }
	dir, err := configuredDir(runtime, filepath.Dir(p), false, nil)
	if err != nil {
		return nil, fail(err)
	}
	defer dir.Close()
	// O_PATH opens a symlink as itself, which the type check then refuses.
	f, err := dir.OpenFile(filepath.Base(p), unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fail(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fail(err)
	}
	if fi.Mode().Type() != os.ModeSocket {
		return nil, fail(errors.New("not a socket"))
	}
	// connect(2) through the descriptor's /proc link reaches the inode opened
	// above; it also lifts sun_path's 108-byte limit.
	conn, err := net.DialTimeout("unix", "/proc/self/fd/"+strconv.Itoa(int(f.Fd())), 10*time.Second)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) {
			err = op.Err
		}
		return nil, fail(err)
	}
	return conn, nil
}
