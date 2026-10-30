// Package agent is the guest side of pocketnix: it serves the app's streams
// (see package protocol) on any listener.
package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
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
	// Updates runs the app's update and generation requests.
	Updates *Updater
	// StateDir keeps what must outlive a crash (the file provider's
	// unfinished temporary files); empty keeps nothing.
	StateDir string

	mu       sync.Mutex
	path     string
	cfg      *File
	controls map[*lockedWriter]struct{}
	// clipboard is the latest of Android's clipboard and a guest copy.
	clipboard string
	notes     notifications
	transfers transfers
	// clipWatchers are the guest connections following the clipboard.
	clipWatchers clipWatchers

	// HostFile is where the phone's state goes as JSON; empty writes none.
	HostFile string
	// Systemctl drives the module's pocketnix-*.target units from the
	// phone's state; empty leaves them alone.
	Systemctl string
	host      hostTracker
	wakes     wakeTracker
	hostfs    hostFolders

	// PortPoll is how often the port watcher polls /proc without hints;
	// zero is PortPollInterval.
	PortPoll time.Duration
	// ProcNet holds the tcp and tcp6 tables the port watcher reads; empty
	// is /proc/net.
	ProcNet string
	ports   portWatch
}

// New loads the config file at path.
func New(path string) (*Server, error) {
	cfg, err := LoadFile(path)
	if err != nil {
		return nil, err
	}
	s := &Server{Version: "dev", path: path, cfg: cfg, controls: map[*lockedWriter]struct{}{}, Updates: NewUpdater()}
	s.Updates.Configure(cfg.Updates)
	s.Updates.Changed = func() { s.pushLatest(func() any { return s.Updates.State() }) }
	s.Updates.Offline = s.knownOffline
	s.portsConfigured()
	return s, nil
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
	s.mu.Unlock()
	s.pushLatest(func() any { return configMessage(s.config()) })
	s.Updates.Configure(cfg.Updates)
	s.wakeChanged()
	s.portsConfigured()
	return nil
}

// push sends msg to every control stream.
func (s *Server) push(msg any) {
	s.pushLatest(func() any { return msg })
}

// pushLatest sends msg() to every control stream, called with the stream's
// writer held: a stream that just opened has its snapshot out first, and
// what follows it is never older. A nil msg() sends nothing.
func (s *Server) pushLatest(msg func() any) {
	s.mu.Lock()
	controls := make([]*lockedWriter, 0, len(s.controls))
	for w := range s.controls {
		controls = append(controls, w)
	}
	s.mu.Unlock()
	for _, w := range controls {
		w.mu.Lock()
		var err error
		if m := msg(); m != nil {
			err = protocol.WriteJSON(unlocked{w}, m)
		}
		w.mu.Unlock()
		if err != nil {
			log.Printf("push: %v", err)
		}
	}
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

// Serve accepts connections on l until it fails. A TCP l's port is never
// offered for forwarding.
func (s *Server) Serve(l net.Listener) error {
	if a, ok := l.Addr().(*net.TCPAddr); ok {
		s.ownPort(a.Port, 1)
		defer s.ownPort(a.Port, -1)
	}
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
	// While 1 is the only version, the replies' Version is the negotiated one;
	// a second version would hand v to the stream handlers.
	if _, ok := protocol.Negotiate(hello.MinVersion, hello.Version); !ok {
		reject(c, fmt.Sprintf("the app speaks protocol %d–%d, the agent %d–%d", max(hello.MinVersion, 1), hello.Version, protocol.MinVersion, protocol.Version))
		return
	}
	switch hello.Stream {
	case protocol.StreamControl:
		s.serveControl(c)
	case protocol.StreamPTY:
		s.servePTY(c, hello)
	case protocol.StreamForward:
		s.serveForward(c, hello.Port)
	case protocol.StreamUDP:
		s.serveUDP(c, hello.Port)
	case protocol.StreamFont:
		s.serveFont(c)
	case protocol.StreamGUI:
		s.serveGUI(c)
	case protocol.StreamFile:
		s.serveFile(c, hello)
	case protocol.StreamShare:
		s.serveShare(c, hello)
	case protocol.StreamSave:
		s.serveSave(c, hello)
	case protocol.StreamHostFS:
		s.serveHostFS(c, hello)
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
	// deadline bounds each write (control streams; PTY output may wait on a busy app).
	deadline bool
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.write(b)
}

func (l *lockedWriter) write(b []byte) (int, error) {
	c, ok := l.w.(net.Conn)
	if !ok || !l.deadline {
		return l.w.Write(b)
	}
	// A reader that stopped reading (a frozen app) mustn't stall pushes to
	// everyone else: give up on it, and the stream ends.
	c.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
	n, err := c.Write(b)
	if err != nil {
		c.Close()
	}
	return n, err
}

const controlWriteTimeout = 10 * time.Second

// unlocked writes through a lockedWriter whose lock the caller holds.
type unlocked struct{ l *lockedWriter }

func (u unlocked) Write(b []byte) (int, error) { return u.l.write(b) }

func (s *Server) serveControl(c net.Conn) {
	w := &lockedWriter{w: c, deadline: true}
	// Register before taking the snapshot, so a reload in between can't be
	// missed, and hold the writer meanwhile: a push waits until after the
	// Hello and the snapshot, which it then supersedes.
	w.mu.Lock()
	s.mu.Lock()
	s.controls[w] = struct{}{}
	cfg := s.cfg
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.controls, w)
		s.mu.Unlock()
		s.hostLost(w)
	}()
	err := protocol.WriteJSON(unlocked{w}, s.hello())
	if err == nil {
		err = protocol.WriteJSON(unlocked{w}, configMessage(cfg))
	}
	if err == nil {
		err = protocol.WriteJSON(unlocked{w}, s.Updates.State())
	}
	if err == nil {
		err = protocol.WriteJSON(unlocked{w}, s.wake())
	}
	if p, ok := s.portSnapshot(); ok && err == nil {
		err = protocol.WriteJSON(unlocked{w}, p)
	}
	w.mu.Unlock()
	if err != nil {
		return
	}
	// Only JSON lines follow the Hello, so the reader may read ahead.
	r := bufio.NewReader(c)
	for {
		line, err := protocol.ReadLine(r)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("control: %v", err)
			}
			return
		}
		var req protocol.Request
		if err := json.Unmarshal(line, &req); err != nil {
			reject(w, "bad message: "+err.Error())
			continue
		}
		if reply, err := s.controlRequest(w, req); err != nil {
			protocol.WriteJSON(w, protocol.Error{Type: protocol.TypeError, Message: err.Error(), ID: req.ID})
		} else {
			protocol.WriteJSON(w, withID(reply, req.ID))
		}
	}
}

// withID adds the request's id to its answer.
func withID(reply any, id int) any {
	if id == 0 {
		return reply
	}
	b, err := json.Marshal(reply)
	var m map[string]any
	if err != nil || json.Unmarshal(b, &m) != nil {
		return reply
	}
	m["id"] = id
	return m
}

// request runs one control request and returns its answer.
func (s *Server) request(req protocol.Request) (any, error) {
	ack := protocol.Envelope{Type: req.Type}
	switch req.Type {
	case protocol.TypePing:
		return protocol.Envelope{Type: protocol.TypePong}, nil
	case protocol.TypePoweroff, protocol.TypeReboot:
		// The echo acknowledges; the VM stopping is the real answer.
		return ack, s.power(req.Type)
	case protocol.TypeUpdates:
		return s.Updates.State(), nil
	case protocol.TypeUpdateCheck:
		return ack, s.Updates.Check()
	case protocol.TypeUpdateApply:
		return ack, s.Updates.Apply()
	case protocol.TypeUpdateCancel:
		return ack, s.Updates.Cancel()
	case protocol.TypeSwitchGeneration:
		return ack, s.Updates.SwitchGeneration(req.Generation)
	case protocol.TypeHistory:
		entries, err := history(s.config(), req.Limit)
		return protocol.History{Type: protocol.TypeHistory, Entries: entries}, err
	case protocol.TypeSetClipboard:
		if len(req.Text) > protocol.MaxClipboardText {
			return nil, fmt.Errorf("clipboard text over %d KiB", protocol.MaxClipboardText>>10)
		}
		s.setClipboard(req.Text)
		s.setClipboardTypes(req.Types, req.Text)
		s.publishClipboard()
		return ack, nil
	case protocol.TypeShareText:
		p, err := s.shareText(req.Text)
		return protocol.Done{Type: protocol.TypeShareText, Path: p}, err
	case protocol.TypeNotifyAction, protocol.TypeNotifyClosed:
		return ack, s.notifyFromApp(req)
	case protocol.TypeGuestDone:
		return ack, s.guestDone(req)
	case protocol.TypeHostFolders:
		return ack, s.applyHostFolders(req.Folders)
	case protocol.TypeResolvePath:
		return s.resolvePath(req.Path)
	default:
		// Answered, so a newer app's later requests still pair with their replies.
		return nil, fmt.Errorf("unknown request %q", req.Type)
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

// serveUDP relays datagrams between the stream and a UDP socket connected to
// port on the loopback, until the app closes the stream.
func (s *Server) serveUDP(c net.Conn, port int) {
	if port < 1 || port > 65535 {
		reject(c, fmt.Sprintf("port %d out of range", port))
		return
	}
	u, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		reject(c, err.Error())
		return
	}
	defer u.Close()
	if protocol.WriteJSON(c, s.hello()) != nil {
		return
	}
	go func() {
		defer c.Close()
		buf := make([]byte, protocol.MaxDatagram)
		for {
			n, err := u.Read(buf)
			// Nothing listening yet (ICMP port unreachable): the service may start later.
			if errors.Is(err, syscall.ECONNREFUSED) {
				continue
			}
			if err != nil || protocol.WriteDatagram(c, buf[:n]) != nil {
				return
			}
		}
	}()
	r := bufio.NewReader(c)
	buf := make([]byte, protocol.MaxDatagram)
	for {
		n, err := protocol.ReadDatagram(r, buf)
		if err != nil {
			return
		}
		u.Write(buf[:n])
	}
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
	f, err := configuredFile(userHome(s.config().User), path)
	if err != nil {
		reject(c, err.Error())
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("font %s is not a regular file", path)
	}
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
