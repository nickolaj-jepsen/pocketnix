// Package agent is the guest side of pocketnix: it serves the app's control,
// PTY, forward and font streams (see package protocol) on any listener.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// Power performs the guest power actions the app may request.
type Power interface {
	Poweroff() error
	Reboot() error
}

// Server serves app connections. Create it with New.
type Server struct {
	Version string
	Power   Power // nil refuses power requests

	mu       sync.Mutex
	path     string
	cfg      *File
	controls map[*lockedWriter]struct{}
}

// New loads the config file at path.
func New(path string) (*Server, error) {
	cfg, err := LoadFile(path)
	if err != nil {
		return nil, err
	}
	return &Server{Version: "dev", path: path, cfg: cfg, controls: map[*lockedWriter]struct{}{}}, nil
}

// Reload re-reads the config file and pushes it to every control stream. On
// error the previous config stays.
func (s *Server) Reload() error {
	cfg, err := LoadFile(s.path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = cfg
	controls := make([]*lockedWriter, 0, len(s.controls))
	for w := range s.controls {
		controls = append(controls, w)
	}
	s.mu.Unlock()
	msg := configMessage(cfg)
	for _, w := range controls {
		if err := protocol.WriteJSON(w, msg); err != nil {
			log.Printf("push config: %v", err)
		}
	}
	return nil
}

func (s *Server) config() *File {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func configMessage(f *File) protocol.Config {
	c := f.Config
	c.Type = protocol.TypeConfig
	if c.Hostname == "" {
		c.Hostname, _ = os.Hostname()
	}
	return c
}

// Serve accepts connections on l until it fails.
func (s *Server) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Keep serving through transient failures such as EMFILE.
			log.Printf("accept on %v: %v", l.Addr(), err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	// A peer that connects and never says hello mustn't hold a goroutine forever.
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	hello, err := protocol.ReadHello(c)
	if err != nil {
		log.Printf("read hello from %v: %v", c.RemoteAddr(), err)
		return
	}
	c.SetReadDeadline(time.Time{})
	if hello.Version != protocol.Version {
		reject(c, fmt.Sprintf("protocol version %d, agent speaks %d", hello.Version, protocol.Version))
		return
	}
	switch hello.Stream {
	case protocol.StreamControl:
		s.serveControl(c)
	case protocol.StreamPTY:
		s.servePTY(c, hello)
	case protocol.StreamForward:
		s.serveForward(c, hello.Port)
	case protocol.StreamFont:
		s.serveFont(c)
	default:
		reject(c, fmt.Sprintf("unknown stream %q", hello.Stream))
	}
}

func (s *Server) hello() protocol.Hello {
	return protocol.Hello{Type: protocol.TypeHello, Version: protocol.Version, Agent: s.Version}
}

func reject(w io.Writer, msg string) {
	protocol.WriteJSON(w, protocol.Error{Type: protocol.TypeError, Message: msg})
}

// lockedWriter serializes whole-message writes from the reply path and from
// config pushes.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

func (s *Server) serveControl(c net.Conn) {
	w := &lockedWriter{w: c}
	// Register before sending the config, so a reload in between can't be missed.
	s.mu.Lock()
	s.controls[w] = struct{}{}
	cfg := s.cfg
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.controls, w)
		s.mu.Unlock()
	}()
	if protocol.WriteJSON(w, s.hello()) != nil || protocol.WriteJSON(w, configMessage(cfg)) != nil {
		return
	}
	for {
		line, err := protocol.ReadLine(c)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("control: %v", err)
			}
			return
		}
		var env protocol.Envelope
		if err := json.Unmarshal(line, &env); err != nil {
			reject(w, "bad message: "+err.Error())
			continue
		}
		switch env.Type {
		case protocol.TypePing:
			protocol.WriteJSON(w, protocol.Envelope{Type: protocol.TypePong})
		case protocol.TypePoweroff, protocol.TypeReboot:
			if err := s.power(env.Type); err != nil {
				reject(w, err.Error())
				continue
			}
			// The echo acknowledges; the VM stopping is the real answer.
			protocol.WriteJSON(w, protocol.Envelope{Type: env.Type})
		default:
			// Newer apps may send what this agent doesn't know yet.
			log.Printf("control: ignoring message type %q", env.Type)
		}
	}
}

func (s *Server) power(action string) error {
	if s.Power == nil {
		return errors.New(action + " is disabled on this agent")
	}
	log.Printf("%s requested by the app", action)
	if action == protocol.TypeReboot {
		return s.Power.Reboot()
	}
	return s.Power.Poweroff()
}

func (s *Server) serveForward(c net.Conn, port int) {
	if port < 1 || port > 65535 {
		reject(c, fmt.Sprintf("port %d out of range", port))
		return
	}
	t, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 10*time.Second)
	if err != nil {
		reject(c, err.Error())
		return
	}
	defer t.Close()
	if protocol.WriteJSON(c, s.hello()) != nil {
		return
	}
	splice(c, t)
}

// splice copies both ways, passing each side's EOF on as a half-close.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

func (s *Server) serveFont(c net.Conn) {
	path := s.config().Terminal.Theme.Font
	if path == "" {
		reject(c, "no font configured")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		reject(c, err.Error())
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		reject(c, err.Error())
		return
	}
	h := s.hello()
	h.Size = fi.Size()
	if protocol.WriteJSON(c, h) != nil {
		return
	}
	io.CopyN(c, f, fi.Size())
}
