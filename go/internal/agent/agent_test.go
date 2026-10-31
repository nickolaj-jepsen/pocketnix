package agent_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/agent"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/transport"
)

type harness struct {
	t      *testing.T
	srv    *agent.Server
	cfg    string
	client *client.Client
	addr   net.Addr // the agent's listener
}

// start serves cfg on a fresh listener at the address made by addr.
func start(t *testing.T, cfg map[string]any, addr func(dir string) string) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, cfg: filepath.Join(dir, "agent.json")}
	h.write(cfg)
	srv, err := agent.New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	l, err := transport.Listen(addr(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	h.addr = l.Addr()
	go srv.Serve(l)
	dial := l.Addr().Network() + ":" + l.Addr().String()
	h.client = &client.Client{App: "test", Dial: func() (net.Conn, error) { return transport.Dial(dial) }}
	return h
}

func unixAddr(dir string) string { return "unix:" + filepath.Join(dir, "agent.sock") }
func tcpAddr(string) string      { return "tcp:127.0.0.1:0" }

func (h *harness) write(cfg map[string]any) {
	h.t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.cfg, b, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func baseConfig() map[string]any {
	return map[string]any{
		// The sandbox's passwd shell may not exist; any POSIX sh will do.
		"shell": "/bin/sh",
		"forwards": []map[string]any{
			{"name": "ssh", "guestPort": 22, "hostPort": 2222},
			{"name": "web", "guestPort": 8080, "lan": true},
			{"name": "mosh", "guestPort": 60001, "protocol": "udp"},
		},
		"terminal": map[string]any{
			"profiles": []map[string]any{
				{"name": "shell"},
				{"name": "echo", "command": "echo hello from $USER"},
			},
			"extraKeys": [][]map[string]any{{{"key": "ESC"}, {"key": "-", "popup": "|"}}},
			"theme":     map[string]any{"colors": map[string]string{"background": "#000000"}},
		},
	}
}

func recvConfig(t *testing.T, ctl *client.Control) protocol.Config {
	t.Helper()
	select {
	case cfg, ok := <-ctl.Configs():
		if !ok {
			t.Fatal("control stream closed")
		}
		return cfg
	case <-time.After(5 * time.Second):
		t.Fatal("no config within 5s")
	}
	panic("unreachable")
}

func TestControlConfig(t *testing.T) {
	for name, addr := range map[string]func(string) string{"unix": unixAddr, "tcp": tcpAddr} {
		t.Run(name, func(t *testing.T) {
			h := start(t, baseConfig(), addr)
			ctl, err := h.client.Control()
			if err != nil {
				t.Fatal(err)
			}
			defer ctl.Close()
			cfg := recvConfig(t, ctl)
			want := []protocol.Forward{
				{Name: "ssh", GuestPort: 22, HostPort: 2222, Protocol: "tcp"},
				{Name: "web", GuestPort: 8080, HostPort: 8080, LAN: true, Protocol: "tcp"},
				{Name: "mosh", GuestPort: 60001, HostPort: 60001, Protocol: "udp"},
			}
			if !equalJSON(cfg.Forwards, want) {
				t.Errorf("forwards = %+v, want %+v", cfg.Forwards, want)
			}
			if cfg.Terminal.DefaultProfile != "shell" {
				t.Errorf("default profile = %q, want the first profile", cfg.Terminal.DefaultProfile)
			}
			if got := cfg.Terminal.ExtraKeys[0][1]; got.Key != "-" || got.Popup != "|" {
				t.Errorf("extra key = %+v", got)
			}
			if cfg.Hostname == "" {
				t.Error("no hostname")
			}
			if err := ctl.Ping(); err != nil {
				t.Errorf("ping: %v", err)
			}
		})
	}
}

func TestReloadPushesConfig(t *testing.T) {
	h := start(t, baseConfig(), unixAddr)
	ctl, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	recvConfig(t, ctl)

	cfg := baseConfig()
	cfg["terminal"].(map[string]any)["theme"] = map[string]any{"colors": map[string]string{"background": "#123456"}}
	h.write(cfg)
	if err := h.srv.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := recvConfig(t, ctl).Terminal.Theme.Colors["background"]; got != "#123456" {
		t.Errorf("background after reload = %q", got)
	}

	// A broken file keeps the last good config for new connections.
	os.WriteFile(h.cfg, []byte("{"), 0o644)
	if err := h.srv.Reload(); err == nil {
		t.Error("reload of a broken file succeeded")
	}
	ctl2, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl2.Close()
	if got := recvConfig(t, ctl2).Terminal.Theme.Colors["background"]; got != "#123456" {
		t.Errorf("background after a failed reload = %q", got)
	}
}

// Streams opening while pushes fly still get the Hello first, and the
// config they end up with is the latest one.
func TestControlOpensDuringPushes(t *testing.T) {
	h := start(t, baseConfig(), unixAddr)
	stop := make(chan struct{})
	pushing := make(chan struct{})
	go func() {
		defer close(pushing)
		for {
			select {
			case <-stop:
				return
			default:
				h.srv.Reload()
				h.srv.Updates.Configure(agent.UpdatesConfig{})
			}
		}
	}()
	var ctls []*client.Control
	for range 20 {
		ctl, err := h.client.Control()
		if err != nil {
			close(stop)
			t.Fatalf("open during pushes: %v", err)
		}
		defer ctl.Close()
		ctls = append(ctls, ctl)
	}
	close(stop)
	<-pushing
	cfg := baseConfig()
	cfg["terminal"].(map[string]any)["theme"] = map[string]any{"colors": map[string]string{"background": "#123456"}}
	h.write(cfg)
	if err := h.srv.Reload(); err != nil {
		t.Fatal(err)
	}
	for i, ctl := range ctls {
		// The reader queues each config before it answers the ping behind it.
		if err := ctl.Ping(); err != nil {
			t.Fatalf("stream %d: ping: %v", i, err)
		}
		var last protocol.Config
		for drained := false; !drained; {
			select {
			case last = <-ctl.Configs():
			default:
				drained = true
			}
		}
		if got := last.Terminal.Theme.Colors["background"]; got != "#123456" {
			t.Errorf("stream %d ended on background %q", i, got)
		}
	}
}

func TestRejectsBadHello(t *testing.T) {
	h := start(t, baseConfig(), unixAddr)
	for name, hello := range map[string]protocol.Hello{
		"version": {Type: "hello", Version: protocol.Version + 1, Stream: "control"},
		"stream":  {Type: "hello", Version: protocol.Version, Stream: "nope"},
		"profile": {Type: "hello", Version: protocol.Version, Stream: "pty", Profile: "nope"},
		"port":    {Type: "hello", Version: protocol.Version, Stream: "forward", Port: 70000},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := h.client.Dial()
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			protocol.WriteJSON(c, hello)
			_, err = protocol.ReadHello(c)
			var re *protocol.RemoteError
			if !errors.As(err, &re) {
				t.Errorf("got %v, want an agent error", err)
			}
		})
	}
}

func TestPoweroffWithoutPowerIsRefused(t *testing.T) {
	h := start(t, baseConfig(), unixAddr)
	ctl, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	var re *protocol.RemoteError
	if err := ctl.Poweroff(); !errors.As(err, &re) {
		t.Errorf("poweroff = %v, want an agent error", err)
	}
	// The stream survives a refused request.
	if err := ctl.Ping(); err != nil {
		t.Errorf("ping after refusal: %v", err)
	}
}

func TestPoweroffAcknowledged(t *testing.T) {
	h := start(t, baseConfig(), unixAddr)
	h.srv.Power = noopPower{}
	ctl, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	if err := ctl.Poweroff(); err != nil {
		t.Errorf("poweroff: %v", err)
	}
}

type noopPower struct{}

func (noopPower) Poweroff() error { return nil }
func (noopPower) Reboot() error   { return nil }

// readUntil reads session output until it contains want.
func readUntil(t *testing.T, r io.Reader, want string) string {
	t.Helper()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			out.Write(buf[:n])
			if strings.Contains(out.String(), want) {
				done <- nil
				return
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiting for %q: %v; output so far %q", want, err, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no %q within 10s", want)
	}
	return out.String()
}

func ptyConfig(command string) map[string]any {
	cfg := baseConfig()
	cfg["terminal"].(map[string]any)["profiles"] = []map[string]any{{"name": "t", "command": command}}
	return cfg
}

func drain(p *client.PTY) {
	io.Copy(io.Discard, p)
}

func TestPTYProfileAndExit(t *testing.T) {
	h := start(t, ptyConfig(`stty size; read line; echo "got:$line"; exit 3`), unixAddr)
	p, err := h.client.PTY("", 30, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	readUntil(t, p, "30 100")
	p.Write([]byte("typed\r"))
	readUntil(t, p, "got:typed")
	drain(p)
	if status, err := p.Exit(); err != nil || status != 3 {
		t.Errorf("exit = %d, %v; want 3", status, err)
	}
}

func TestPTYResize(t *testing.T) {
	h := start(t, ptyConfig(`while read line; do stty size; done`), unixAddr)
	p, err := h.client.PTY("t", 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Resize(50, 132); err != nil {
		t.Fatal(err)
	}
	p.Write([]byte("\r"))
	readUntil(t, p, "50 132")
}

func TestPTYRunsTheCommandItIsGiven(t *testing.T) {
	h := start(t, ptyConfig(`echo from-the-profile`), unixAddr)
	p, err := h.client.PTYCommand("t", "echo from-the-command; exit 4", 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	readUntil(t, p, "from-the-command")
	drain(p)
	if status, err := p.Exit(); err != nil || status != 4 {
		t.Errorf("exit = %d, %v; want the command's 4", status, err)
	}
}

func TestPTYSignalStatus(t *testing.T) {
	h := start(t, ptyConfig(`kill -TERM $$`), unixAddr)
	p, err := h.client.PTY("", 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	drain(p)
	if status, err := p.Exit(); err != nil || status != -int32(syscall.SIGTERM) {
		t.Errorf("exit = %d, %v; want -%d", status, err, syscall.SIGTERM)
	}
}

func TestPTYHangupEndsSession(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	h := start(t, ptyConfig(`echo $$ > `+pidFile+`; echo started; while :; do sleep 1; done`), unixAddr)
	p, err := h.client.PTY("", 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, p, "started")
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	p.Close()
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			st, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
			t.Fatalf("session process %d still alive after hangup: %s", pid, st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A session that ignores the hangup is killed once its grace is up.
func TestPTYIgnoringHangupIsKilled(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	h := start(t, ptyConfig(`trap '' HUP; echo $$ > `+pidFile+`; echo started; while :; do sleep 1; done`), unixAddr)
	p, err := h.client.PTY("", 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, p, "started")
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	p.Close()
	deadline := time.Now().Add(20 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("session process %d ignoring SIGHUP outlived its tab", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A background child that outlives the shell gets a hung-up tty, as on a
// closed terminal, instead of pinning the session's pty.
func TestPTYBackgroundChildIsHungUp(t *testing.T) {
	mark := filepath.Join(t.TempDir(), "hung-up")
	// Ignored before the fork: the child inherits it before the shell's exit hangs it up.
	h := start(t, ptyConfig(`trap '' HUP; (sleep 2; echo late || touch `+mark+`) & echo started`), unixAddr)
	p, err := h.client.PTY("", 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	readUntil(t, p, "started")
	drain(p)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(mark); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the background child could still write to the tty")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestForward(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				// Echo until the client half-closes, then answer and close.
				b, _ := io.ReadAll(c)
				c.Write(append([]byte("echo:"), b...))
				c.Close()
			}()
		}
	}()
	port := echo.Addr().(*net.TCPAddr).Port

	h := start(t, baseConfig(), unixAddr)
	c, err := h.client.Forward(port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("ping"))
	c.(interface{ CloseWrite() error }).CloseWrite()
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "echo:ping" {
		t.Errorf("forward got %q, %v", got, err)
	}

	echo.Close()
	var re *protocol.RemoteError
	if _, err := h.client.Forward(port); !errors.As(err, &re) {
		t.Errorf("forward to a closed port = %v, want an agent error", err)
	}
}

func TestForwardReachesIPv6Loopback(t *testing.T) {
	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback:", err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		c.Write([]byte("v6"))
		c.Close()
	}()
	h := start(t, baseConfig(), unixAddr)
	c, err := h.client.Forward(l.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got, err := io.ReadAll(c); err != nil || string(got) != "v6" {
		t.Errorf("forward to a ::1 listener got %q, %v", got, err)
	}
}

func TestUDP(t *testing.T) {
	echo, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, protocol.MaxDatagram)
		for {
			n, from, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			echo.WriteTo(append([]byte("echo:"), buf[:n]...), from)
		}
	}()
	port := echo.LocalAddr().(*net.UDPAddr).Port

	h := start(t, baseConfig(), unixAddr)
	d, err := h.client.UDP(port)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	buf := make([]byte, protocol.MaxDatagram)
	// Each datagram comes back whole and on its own, the empty one included.
	for _, msg := range []string{"one", "", strings.Repeat("x", 60000)} {
		if err := d.Send([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		d.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := d.Recv(buf)
		if err != nil || string(buf[:n]) != "echo:"+msg {
			t.Fatalf("reply to a %d-byte datagram = %d bytes, %v", len(msg), n, err)
		}
	}

	// A guest port with nothing listening drops datagrams but keeps the stream.
	echo.Close()
	d.Send([]byte("lost"))
	d.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := d.Recv(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("recv with nothing listening = %v, want a timeout", err)
	}

	var re *protocol.RemoteError
	if _, err := h.client.UDP(70000); !errors.As(err, &re) {
		t.Errorf("udp to port 70000 = %v, want an agent error", err)
	}
}

func TestFont(t *testing.T) {
	font := filepath.Join(t.TempDir(), "font.ttf")
	data := bytes.Repeat([]byte("not really a font "), 10000)
	os.WriteFile(font, data, 0o644)
	cfg := baseConfig()
	cfg["terminal"].(map[string]any)["theme"] = map[string]any{"font": font}
	h := start(t, cfg, unixAddr)

	ctl, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	sum := sha256.Sum256(data)
	if got := recvConfig(t, ctl).Terminal.Theme.FontSHA256; got != hex.EncodeToString(sum[:]) {
		t.Errorf("font sha256 = %q", got)
	}
	got, err := h.client.Font()
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("font: %d bytes, %v; want %d bytes", len(got), err, len(data))
	}
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
