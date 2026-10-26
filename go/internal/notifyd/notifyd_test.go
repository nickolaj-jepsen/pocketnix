package notifyd_test

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/agent"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/notifyd"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/transport"
)

const busConfig = `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:path=SOCKET</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>
`

// privateBus starts a session bus of its own and returns its address.
func privateBus(t *testing.T) string {
	t.Helper()
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("no dbus-daemon")
	}
	dir := t.TempDir()
	conf := filepath.Join(dir, "bus.conf")
	if err := os.WriteFile(conf, []byte(strings.Replace(busConfig, "SOCKET", filepath.Join(dir, "bus"), 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(daemon, "--config-file="+conf, "--nofork", "--print-address")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	addr, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(addr)
}

// startAgent serves an agent's guest socket and returns it, with the app's control stream.
func startAgent(t *testing.T) (string, *client.Control) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(cfg, []byte(`{"shell":"/bin/sh","forwards":[],"terminal":{"profiles":[{"name":"shell"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := transport.Listen("unix:" + filepath.Join(dir, "agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go srv.Serve(l)
	sock := filepath.Join(dir, "guest.sock")
	gl, err := agent.ListenGuest(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gl.Close() })
	go srv.ServeGuest(gl)
	c := &client.Client{App: "test", Dial: func() (net.Conn, error) { return transport.Dial("unix:" + filepath.Join(dir, "agent.sock")) }}
	ctl, err := c.Control()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctl.Close() })
	// Events only reach a control stream that's registered, which the first push shows.
	if _, err := ctl.WaitUpdates(func(protocol.Updates) bool { return true }); err != nil {
		t.Fatal(err)
	}
	return sock, ctl
}

func connect(t *testing.T, addr string) *dbus.Conn {
	t.Helper()
	conn, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func nextEvent(t *testing.T, ctl *client.Control) protocol.Event {
	t.Helper()
	select {
	case ev := <-ctl.Events():
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no app event within 5s")
	}
	panic("unreachable")
}

// nextSignal returns the next notifyd signal, skipping the bus's own
// (NameAcquired reaches the channel whatever the match rules say).
func nextSignal(t *testing.T, ch <-chan *dbus.Signal) *dbus.Signal {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case s := <-ch:
			if strings.HasPrefix(s.Name, notifyd.Name+".") {
				return s
			}
		case <-deadline:
			t.Fatal("no signal within 5s")
		}
	}
}

func TestNotifications(t *testing.T) {
	bus := privateBus(t)
	sock, ctl := startAgent(t)
	d, err := notifyd.Serve(connect(t, bus), sock, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := notifyd.Serve(connect(t, bus), sock, "test"); err == nil {
		t.Error("a second server took the name")
	}

	app := connect(t, bus)
	signals := make(chan *dbus.Signal, 16)
	app.Signal(signals)
	if err := app.AddMatchSignal(dbus.WithMatchInterface(notifyd.Name)); err != nil {
		t.Fatal(err)
	}
	obj := app.Object(notifyd.Name, notifyd.Path)
	notify := func(replaces uint32, summary string, actions []string, hints map[string]dbus.Variant, timeout int32) uint32 {
		t.Helper()
		if hints == nil {
			hints = map[string]dbus.Variant{}
		}
		var id uint32
		if err := obj.Call(notifyd.Name+".Notify", 0, "make", replaces, "", summary, "body", actions, hints, timeout).Store(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	var caps []string
	if err := obj.Call(notifyd.Name+".GetCapabilities", 0).Store(&caps); err != nil || !strings.Contains(strings.Join(caps, " "), "actions") {
		t.Errorf("capabilities = %v, %v", caps, err)
	}

	// An action comes back as ActionInvoked, then the notification is closed.
	id := notify(0, "build", []string{"ok", "OK"}, map[string]dbus.Variant{"urgency": dbus.MakeVariant(byte(2))}, -1)
	ev := nextEvent(t, ctl)
	if ev.Notification != id || ev.Title != "build" || ev.AppName != "make" || ev.Urgency != protocol.UrgencyCritical ||
		len(ev.Actions) != 1 || ev.Actions[0] != (protocol.NotifyAction{Key: "ok", Label: "OK"}) {
		t.Errorf("event = %+v, id %d", ev, id)
	}
	if err := ctl.NotifyAction(id, "ok"); err != nil {
		t.Fatal(err)
	}
	if s := nextSignal(t, signals); s.Name != notifyd.Name+".ActionInvoked" || s.Body[0] != id || s.Body[1] != "ok" {
		t.Errorf("signal = %v %v", s.Name, s.Body)
	}
	if s := nextSignal(t, signals); s.Name != notifyd.Name+".NotificationClosed" || s.Body[0] != id || s.Body[1] != uint32(protocol.ClosedDismissed) {
		t.Errorf("signal = %v %v", s.Name, s.Body)
	}

	// A replacement keeps the id; CloseNotification withdraws it from the app.
	id = notify(0, "first", nil, nil, -1)
	nextEvent(t, ctl)
	if got := notify(id, "second", nil, nil, -1); got != id {
		t.Errorf("replaced id = %d, want %d", got, id)
	}
	if ev := nextEvent(t, ctl); ev.Notification != id || ev.Title != "second" {
		t.Errorf("event = %+v", ev)
	}
	if err := obj.Call(notifyd.Name+".CloseNotification", 0, id).Err; err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, ctl); ev.Type != protocol.TypeNotifyClose || ev.Notification != id {
		t.Errorf("event = %+v", ev)
	}
	if s := nextSignal(t, signals); s.Name != notifyd.Name+".NotificationClosed" || s.Body[0] != id || s.Body[1] != uint32(protocol.ClosedByCall) {
		t.Errorf("signal = %v %v", s.Name, s.Body)
	}

	// A dismissal on Android, and an expiry.
	id = notify(0, "swipe me", nil, nil, -1)
	nextEvent(t, ctl)
	if err := ctl.NotifyClosed(id, protocol.ClosedDismissed); err != nil {
		t.Fatal(err)
	}
	if s := nextSignal(t, signals); s.Name != notifyd.Name+".NotificationClosed" || s.Body[0] != id || s.Body[1] != uint32(protocol.ClosedDismissed) {
		t.Errorf("signal = %v %v", s.Name, s.Body)
	}
	id = notify(0, "brief", nil, nil, 100)
	nextEvent(t, ctl)
	if ev := nextEvent(t, ctl); ev.Type != protocol.TypeNotifyClose || ev.Notification != id {
		t.Errorf("event = %+v", ev)
	}
	if s := nextSignal(t, signals); s.Name != notifyd.Name+".NotificationClosed" || s.Body[0] != id || s.Body[1] != uint32(protocol.ClosedExpired) {
		t.Errorf("signal = %v %v", s.Name, s.Body)
	}

	// Without the app, Notify fails rather than queues.
	ctl.Close()
	time.Sleep(100 * time.Millisecond)
	if err := obj.Call(notifyd.Name+".Notify", 0, "make", uint32(0), "", "lost", "", []string{}, map[string]dbus.Variant{}, int32(-1)).Err; err == nil {
		t.Error("Notify succeeded without the app")
	}
}
