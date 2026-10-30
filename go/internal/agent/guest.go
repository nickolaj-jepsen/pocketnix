package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// ServeGuest serves guest programs' requests to the app on l, a unix socket
// inside the guest. Only root and the session user may use it.
func (s *Server) ServeGuest(l net.Listener) error {
	slots := make(chan struct{}, maxGuestConns)
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// A failure such as EMFILE mustn't end the agent, and every tab with it.
			log.Printf("guest socket: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			protocol.WriteJSON(c, protocol.GuestReply{Error: "too many guest connections"})
			c.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			s.handleGuest(c)
		}()
	}
}

const (
	// maxGuestConns bounds the guest socket's open connections, so a leaking
	// program can't run the agent out of descriptors.
	maxGuestConns = 64
	// guestIdle is how long a guest connection may wait between requests.
	guestIdle = 30 * time.Second
)

func (s *Server) handleGuest(c net.Conn) {
	defer c.Close()
	if err := s.guestAllowed(c); err != nil {
		protocol.WriteJSON(c, protocol.GuestReply{Error: err.Error()})
		return
	}
	conn := &fdConn{Conn: c}
	r := bufio.NewReader(conn)
	for {
		c.SetReadDeadline(time.Now().Add(guestIdle))
		line, err := protocol.ReadLine(r)
		if err != nil {
			closeFiles(conn.take())
			return
		}
		fds := conn.take()
		var req protocol.GuestRequest
		var reply protocol.GuestReply
		if err := json.Unmarshal(line, &req); err != nil {
			reply.Error = "bad request: " + err.Error()
		} else if req.Type == protocol.GuestSubscribe {
			closeFiles(fds)
			s.subscribe(c, r)
			return
		} else if req.Type == protocol.GuestSubscribeClipboard {
			closeFiles(fds)
			s.subscribeClipboard(c, r)
			return
		} else if req.Type == protocol.GuestFrameBuffers {
			closeFiles(fds)
			s.frameBuffers(c, r, req)
			return
		} else if req.Type == protocol.GuestNotify || req.Type == protocol.GuestNotifyClose {
			var err error
			if reply, err = s.guestNotify(req); err != nil {
				reply.Error = err.Error()
			}
		} else if isTransfer(req) {
			reply = s.runTransfer(c, r, req, fds)
		} else if text, err := s.guest(req); err != nil {
			reply.Error = err.Error()
		} else {
			reply.Text = text
		}
		closeFiles(fds)
		if protocol.WriteJSON(c, reply) != nil {
			return
		}
	}
}

// guestAllowed admits root and the session user, by the socket's peer credentials.
func (s *Server) guestAllowed(c net.Conn) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return nil // tests over other transports
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var credErr error
	raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if credErr != nil {
		return credErr
	}
	if cred.Uid == 0 || int(cred.Uid) == os.Getuid() {
		return nil
	}
	if u, err := sessionUser(s.config().User); err == nil && int(cred.Uid) == u.uid {
		return nil
	}
	return fmt.Errorf("uid %d may not use the pocketnix agent", cred.Uid)
}

func (s *Server) guest(req protocol.GuestRequest) (string, error) {
	switch req.Type {
	case protocol.GuestOpen:
		if req.URL != "" {
			u, err := url.Parse(req.URL)
			if err != nil || u.Scheme == "" {
				return "", fmt.Errorf("%q is not a URL", req.URL)
			}
			return "", s.event(protocol.Event{Type: protocol.TypeOpen, URL: req.URL})
		}
		rel, err := s.underFilesRoot(req.Path)
		if err != nil {
			return "", err
		}
		return "", s.event(protocol.Event{Type: protocol.TypeOpen, Path: rel})
	case protocol.GuestCopy:
		if len(req.Text) > protocol.MaxClipboardText {
			return "", fmt.Errorf("clipboard text over %d KiB", protocol.MaxClipboardText>>10)
		}
		s.setClipboard(req.Text)
		s.setClipboardTypes(nil, req.Text)
		s.publishClipboard()
		return "", s.event(protocol.Event{Type: protocol.TypeGuestClipboard, Text: req.Text, Sensitive: req.Sensitive})
	case protocol.GuestPaste:
		return s.clipboardText(), nil
	case protocol.GuestHost:
		b, err := json.Marshal(s.hostState())
		return string(b), err
	default:
		return "", fmt.Errorf("unknown request %q", req.Type)
	}
}

// event pushes ev to every control stream, failing when the app isn't connected.
func (s *Server) event(ev protocol.Event) error {
	s.mu.Lock()
	n := len(s.controls)
	s.mu.Unlock()
	if n == 0 {
		return errors.New("the pocketnix app isn't connected")
	}
	s.push(ev)
	return nil
}

func (s *Server) setClipboard(text string) {
	s.mu.Lock()
	s.clipboard = text
	s.mu.Unlock()
}

func (s *Server) clipboardText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clipboard
}

// filesRoot is the file provider's root directory.
func (s *Server) filesRoot() (string, error) {
	cfg := s.config()
	if cfg.FilesRoot != "" {
		return cfg.FilesRoot, nil
	}
	u, err := sessionUser(cfg.User)
	if err != nil {
		return "", err
	}
	return u.home, nil
}

// openFilesRoot opens the file provider's root.
func (s *Server) openFilesRoot() (*os.Root, error) {
	cfg := s.config()
	if cfg.FilesRoot == "" {
		root, err := s.filesRoot()
		if err != nil {
			return nil, err
		}
		return os.OpenRoot(root)
	}
	return configuredDir(userHome(cfg.User), cfg.FilesRoot, false, nil)
}

// userHome is the session user's home, or "" when there's none to find.
func userHome(name string) string {
	u, err := sessionUser(name)
	if err != nil {
		return ""
	}
	return u.home
}

// underFilesRoot turns an absolute guest path into one relative to the file
// provider's root, which is how the app names files.
func (s *Server) underFilesRoot(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%q is not an absolute path", path)
	}
	root, err := s.filesRoot()
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realRoot, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s is outside %s, which is all the app can open (pocketnix.guest.files.root)", path, root)
	}
	if rel == "." {
		rel = ""
	}
	return filepath.ToSlash(rel), nil
}

// ListenGuest opens the guest socket at path, usable by everyone: the agent
// checks each peer's credentials itself.
func ListenGuest(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		l.Close()
		return nil, err
	}
	log.Printf("guest socket on %s", path)
	return l, nil
}
