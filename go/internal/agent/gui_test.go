package agent_test

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/agent"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

func guiConfig(socket string) map[string]any {
	cfg := baseConfig()
	cfg["gui"] = map[string]any{
		"enable":      true,
		"defaults":    map[string]any{"size": "phone", "input": "touch"},
		"appDefaults": map[string]any{"org.gnome.Calculator": map[string]any{"size": "compact"}},
	}
	cfg["guiSocket"] = socket
	return cfg
}

// fakeCompositor accepts GUI connections on path and passes on the streams
// the agent hands over on them.
func fakeCompositor(t *testing.T, path string) <-chan net.Conn {
	t.Helper()
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	conns := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			conns <- handedOver(t, c)
		}
	}()
	return conns
}

// handedOver reads the agent's handover from c and returns the stream it
// carries; c itself on anything else, which the test then sees.
func handedOver(t *testing.T, c *net.UnixConn) net.Conn {
	buf, oob := make([]byte, 16), make([]byte, unix.CmsgSpace(4))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	c.SetReadDeadline(time.Time{})
	if err != nil || n != 1 || buf[0] != 0 {
		return c
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		return c
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		return c
	}
	c.Close()
	f := os.NewFile(uintptr(fds[0]), "handed over")
	defer f.Close()
	stream, err := net.FileConn(f)
	if err != nil {
		t.Errorf("the handed-over descriptor: %v", err)
		return c
	}
	return stream
}

func accepted(t *testing.T, conns <-chan net.Conn) net.Conn {
	t.Helper()
	select {
	case c := <-conns:
		t.Cleanup(func() { c.Close() })
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never connected to the GUI socket")
	}
	panic("unreachable")
}

func readN(t *testing.T, r net.Conn, n int) string {
	t.Helper()
	r.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		t.Fatalf("read %d bytes: %v (got %q)", n, err, b)
	}
	return string(b)
}

func TestGUIHandsTheStreamToTheCompositor(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "gui.sock")
	conns := fakeCompositor(t, sock)
	h := start(t, guiConfig(sock), unixAddr)

	app, err := h.client.GUI()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	comp := accepted(t, conns)

	// Bytes that are no valid frame: the agent mustn't parse them.
	app.Write([]byte("\x00\xff\xff\xffapp"))
	if got := readN(t, comp, 7); got != "\x00\xff\xff\xffapp" {
		t.Errorf("compositor got %q", got)
	}
	comp.Write([]byte("compositor"))
	if got := readN(t, app, 10); got != "compositor" {
		t.Errorf("app got %q", got)
	}

	// The compositor dropping the connection (a newer app won) ends the stream.
	comp.Close()
	app.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := app.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("after the compositor closed: %d bytes, %v; want EOF", n, err)
	}

	// The app going away reaches the compositor as EOF.
	app2, err := h.client.GUI()
	if err != nil {
		t.Fatal(err)
	}
	comp2 := accepted(t, conns)
	app2.Close()
	comp2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := comp2.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("after the app closed: %d bytes, %v; want EOF", n, err)
	}
}

func TestGUIRefused(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.sock")
	conns := fakeCompositor(t, real)
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "plain")
	os.WriteFile(plain, nil, 0o644)

	off := guiConfig(real)
	off["gui"].(map[string]any)["enable"] = false
	noGUI := baseConfig()
	noGUI["guiSocket"] = real

	for name, tc := range map[string]struct {
		cfg map[string]any
		// The contract's wording; the other refusals' text is the agent's own.
		want string
	}{
		"disabled":   {off, "GUI is not enabled"},
		"absent":     {noGUI, "GUI is not enabled"},
		"missing":    {guiConfig(filepath.Join(dir, "missing.sock")), ""},
		"symlink":    {guiConfig(link), ""},
		"not socket": {guiConfig(plain), ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := start(t, tc.cfg, unixAddr)
			c, err := h.client.GUI()
			var re *protocol.RemoteError
			if !errors.As(err, &re) {
				if c != nil {
					c.Close()
				}
				t.Fatalf("GUI() = %v, want an agent error", err)
			}
			if !strings.Contains(re.Message, tc.want) {
				t.Errorf("error %q, want it to mention %q", re.Message, tc.want)
			}
			// Above all, a symlink must not lead to the socket it points at.
			select {
			case c := <-conns:
				c.Close()
				t.Errorf("a refused GUI stream reached %s", real)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

func TestGUIConfigPushed(t *testing.T) {
	h := start(t, guiConfig(""), unixAddr)
	ctl, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	cfg := recvConfig(t, ctl)
	want := &protocol.GUI{
		Enable:      true,
		Defaults:    protocol.AppSettings{Size: "phone", Input: "touch"},
		AppDefaults: map[string]protocol.AppSettings{"org.gnome.Calculator": {Size: "compact"}},
	}
	if !equalJSON(cfg.GUI, want) {
		t.Errorf("gui = %+v, want %+v", cfg.GUI, want)
	}
}

func TestGUIConfigRejectsUnknownSettings(t *testing.T) {
	for _, set := range []func(map[string]any){
		func(g map[string]any) { g["defaults"] = map[string]any{"size": "huge"} },
		func(g map[string]any) { g["appDefaults"] = map[string]any{"foot": map[string]any{"input": "pen"}} },
	} {
		cfg := guiConfig("")
		set(cfg["gui"].(map[string]any))
		h := &harness{t: t, cfg: filepath.Join(t.TempDir(), "agent.json")}
		h.write(cfg)
		if _, err := agent.New(h.cfg); err == nil {
			t.Errorf("config %v loaded", cfg["gui"])
		}
	}
}

func TestPTYTabID(t *testing.T) {
	h := start(t, ptyConfig(`echo "tab:$POCKETNIX_TAB."`), unixAddr)
	seen := map[string]bool{}
	for range 2 {
		p, err := h.client.PTY("", 24, 80)
		if err != nil {
			t.Fatal(err)
		}
		if p.Tab == "" || seen[p.Tab] {
			t.Fatalf("tab id %q (seen %v)", p.Tab, seen)
		}
		seen[p.Tab] = true
		readUntil(t, p, "tab:"+p.Tab+".")
		p.Close()
	}
}
