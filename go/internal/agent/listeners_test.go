package agent_test

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/agent"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

func offersConfig() map[string]any {
	cfg := baseConfig()
	cfg["forwardOffers"] = map[string]any{"enable": true}
	return cfg
}

// startOffers serves cfg at addr with the port watcher polling procNet
// (empty: the real /proc), and opens a control stream.
func startOffers(t *testing.T, cfg map[string]any, addr func(string) string, procNet string) (*harness, *client.Control) {
	t.Helper()
	h := start(t, cfg, addr)
	// Answered, so Serve has counted its own port before the first scan.
	ctl := control(t, h)
	h.srv.PortPoll, h.srv.ProcNet = 10*time.Millisecond, procNet
	h.srv.WatchPorts(nil)
	t.Cleanup(func() {
		// Offers off stops the watcher.
		h.write(baseConfig())
		h.srv.Reload()
	})
	return h, ctl
}

func listen(t *testing.T) (net.Listener, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, l.Addr().(*net.TCPAddr).Port
}

// awaitPorts waits for a snapshot that ok accepts. Other tests' listeners
// come and go in it too, so ok looks only at the test's own ports.
func awaitPorts(t *testing.T, ctl *client.Control, what string, ok func(ports []int) bool) protocol.PortListeners {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case p, open := <-ctl.PortListeners():
			if !open {
				t.Fatal("control stream closed")
			}
			if ok(p.Ports) {
				return p
			}
		case <-timeout:
			t.Fatalf("no snapshot %s within 5s", what)
		}
	}
}

func control(t *testing.T, h *harness) *client.Control {
	t.Helper()
	ctl, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctl.Close() })
	return ctl
}

func TestPortListenersFollowAListener(t *testing.T) {
	h, ctl := startOffers(t, offersConfig(), unixAddr, "")
	l, port := listen(t)
	awaitPorts(t, ctl, fmt.Sprint("with ", port), func(ps []int) bool { return slices.Contains(ps, port) })

	// A stream that opens now starts from the current set.
	awaitPorts(t, control(t, h), fmt.Sprint("on connect with ", port), func(ps []int) bool { return slices.Contains(ps, port) })

	l.Close()
	awaitPorts(t, ctl, fmt.Sprint("without ", port), func(ps []int) bool { return !slices.Contains(ps, port) })
}

// rawControl opens a control stream and returns each message's type and line in turn.
func rawControl(t *testing.T, h *harness) (send func(any), next func() (string, []byte)) {
	t.Helper()
	c, err := h.client.Dial()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	protocol.WriteJSON(c, protocol.Hello{Type: protocol.TypeHello, Version: protocol.Version, Stream: protocol.StreamControl})
	if _, err := protocol.ReadHello(c); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	return func(msg any) { protocol.WriteJSON(c, msg) }, func() (string, []byte) {
		t.Helper()
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := protocol.ReadLine(r)
		if err != nil {
			t.Fatal(err)
		}
		var env protocol.Envelope
		json.Unmarshal(line, &env)
		return env.Type, line
	}
}

// nextPorts skips to the next port-listeners message.
func nextPorts(t *testing.T, next func() (string, []byte)) []int {
	t.Helper()
	for {
		if typ, line := next(); typ == protocol.TypePortListeners {
			var p protocol.PortListeners
			if err := json.Unmarshal(line, &p); err != nil {
				t.Fatal(err)
			}
			return p.Ports
		}
	}
}

func TestUnchangedRescanPushesNothing(t *testing.T) {
	// FIFOs for the tables: a scan reads tcp to its end, then tcp6, so each
	// pair of writes the test makes is one scan.
	dir := t.TempDir()
	tables := []string{filepath.Join(dir, "tcp"), filepath.Join(dir, "tcp6")}
	for _, table := range tables {
		if err := syscall.Mkfifo(table, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := start(t, offersConfig(), unixAddr)
	h.srv.ProcNet, h.srv.PortPoll = dir, time.Millisecond
	h.srv.WatchPorts(nil)
	_, next := rawControl(t, h)
	t.Cleanup(func() {
		h.write(baseConfig())
		h.srv.Reload()
		// Let a scan blocked on a FIFO finish, so the stopped watcher returns.
		for _, table := range tables {
			if f, err := os.OpenFile(table, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				f.Close()
			}
		}
	})
	write := func(table string, f func(io.Writer)) {
		t.Helper()
		w, err := os.OpenFile(table, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		f(w)
	}
	scan := func(ports ...int) {
		t.Helper()
		write(tables[0], func(w io.Writer) {
			fmt.Fprintln(w, "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode")
			for i, p := range ports {
				fmt.Fprintf(w, "%4d: 0100007F:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000 %5d        0 %d\n", i, p, os.Getuid(), 1000+i)
			}
		})
		write(tables[1], func(io.Writer) {})
	}

	if got := nextPorts(t, next); len(got) != 0 {
		t.Fatalf("snapshot before the first scan = %v, want none", got)
	}
	scan(8123)
	for range 3 {
		scan(8123)
	}
	scan(8123, 8124)
	if got := nextPorts(t, next); !slices.Equal(got, []int{8123}) {
		t.Fatalf("after the first scan = %v, want [8123]", got)
	}
	if got := nextPorts(t, next); !slices.Equal(got, []int{8123, 8124}) {
		t.Errorf("after unchanged scans and a change = %v, want only the change, [8123 8124]", got)
	}
}

func TestPortOffersOffLoadsNothing(t *testing.T) {
	h := start(t, baseConfig(), unixAddr)
	h.srv.PortPoll = 10 * time.Millisecond
	var loads atomic.Int32
	h.srv.WatchPorts(func() (agent.PortHints, error) {
		loads.Add(1)
		return nil, errors.New("no hints in a test")
	})
	listen(t)
	send, next := rawControl(t, h)
	send(protocol.Request{Type: protocol.TypePing, ID: 1})
	for typ, _ := next(); typ != protocol.TypePong; typ, _ = next() {
		if typ == protocol.TypePortListeners {
			t.Fatal("a snapshot with offers off")
		}
	}
	if n := loads.Load(); n != 0 {
		t.Fatalf("offers off loaded the watcher %d times", n)
	}

	h.write(offersConfig())
	if err := h.srv.Reload(); err != nil {
		t.Fatal(err)
	}
	nextPorts(t, next)
	if n := loads.Load(); n != 1 {
		t.Errorf("offers on loaded the watcher %d times, want once", n)
	}
	h.write(baseConfig())
	h.srv.Reload()
}

func TestReloadedForwardLeavesTheSnapshot(t *testing.T) {
	h, ctl := startOffers(t, offersConfig(), unixAddr, "")
	_, port := listen(t)
	awaitPorts(t, ctl, fmt.Sprint("with ", port), func(ps []int) bool { return slices.Contains(ps, port) })

	cfg := offersConfig()
	cfg["forwards"] = []map[string]any{{"name": "dev", "guestPort": port}}
	h.write(cfg)
	if err := h.srv.Reload(); err != nil {
		t.Fatal(err)
	}
	awaitPorts(t, ctl, fmt.Sprint("without the forward's ", port), func(ps []int) bool { return !slices.Contains(ps, port) })
}

func TestReloadedUserIsResolvedAgain(t *testing.T) {
	other, err := user.Lookup("nobody")
	if err != nil || other.Uid == strconv.Itoa(os.Getuid()) {
		t.Skip("needs a user nobody other than the test's")
	}
	h, ctl := startOffers(t, offersConfig(), unixAddr, "")
	_, port := listen(t)
	awaitPorts(t, ctl, fmt.Sprint("with ", port), func(ps []int) bool { return slices.Contains(ps, port) })

	cfg := offersConfig()
	cfg["user"] = "nobody"
	h.write(cfg)
	if err := h.srv.Reload(); err != nil {
		t.Fatal(err)
	}
	awaitPorts(t, ctl, fmt.Sprint("without another user's ", port), func(ps []int) bool { return !slices.Contains(ps, port) })

	h.write(offersConfig())
	if err := h.srv.Reload(); err != nil {
		t.Fatal(err)
	}
	awaitPorts(t, ctl, fmt.Sprint("with ", port, " again"), func(ps []int) bool { return slices.Contains(ps, port) })
}

func TestAgentsOwnPortIsNeverOffered(t *testing.T) {
	h, ctl := startOffers(t, offersConfig(), tcpAddr, "")
	own := h.addr.(*net.TCPAddr).Port
	_, port := listen(t)
	timeout := time.After(5 * time.Second)
	for {
		select {
		case p := <-ctl.PortListeners():
			if slices.Contains(p.Ports, own) {
				t.Fatalf("the agent's own port %d offered: %v", own, p.Ports)
			}
			if slices.Contains(p.Ports, port) {
				return
			}
		case <-timeout:
			t.Fatalf("no snapshot with %d within 5s", port)
		}
	}
}

// procTable writes a /proc/net/tcp with a LISTEN line per uid and IPv4 address:port.
func procTable(t *testing.T, dir string, lines ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	for i := 0; i+1 < len(lines); i += 2 {
		uid, addr := lines[i], netip.MustParseAddrPort(lines[i+1])
		a := addr.Addr().As4()
		fmt.Fprintf(&b, "%4d: %08X:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000 %5s        0 %d\n",
			i/2, binary.NativeEndian.Uint32(a[:]), addr.Port(), uid, 1000+i)
	}
	if err := os.WriteFile(filepath.Join(dir, "tcp"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestForwardReachesOnlyTheSessionUsersListener(t *testing.T) {
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
			c.Write([]byte("hi"))
			c.Close()
		}
	}()
	port := echo.Addr().(*net.TCPAddr).Port
	me, other := strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getuid()+1)
	dir := t.TempDir()
	forward := func(h *harness) error {
		c, err := h.client.Forward(port)
		if err == nil {
			defer c.Close()
			if got, _ := io.ReadAll(c); string(got) != "hi" {
				t.Errorf("forward got %q", got)
			}
		}
		return err
	}

	h := start(t, baseConfig(), unixAddr)
	h.srv.ProcNet = dir
	// The table says another user also listens on 0.0.0.0, which a dial to 127.0.0.1 may reach.
	procTable(t, dir, me, fmt.Sprintf("127.0.0.1:%d", port), other, fmt.Sprintf("0.0.0.0:%d", port))
	var re *protocol.RemoteError
	if err := forward(h); !errors.As(err, &re) {
		t.Errorf("forward to a port another user listens on = %v, want an agent error", err)
	}
	procTable(t, dir, me, fmt.Sprintf("127.0.0.1:%d", port))
	if err := forward(h); err != nil {
		t.Errorf("forward to the session user's port: %v", err)
	}

	// A module forward reaches whoever listens.
	cfg := baseConfig()
	cfg["forwards"] = []map[string]any{{"name": "svc", "guestPort": port}}
	m := start(t, cfg, unixAddr)
	m.srv.ProcNet = dir
	procTable(t, dir, other, fmt.Sprintf("127.0.0.1:%d", port))
	if err := forward(m); err != nil {
		t.Errorf("module forward to another user's port: %v", err)
	}
}
