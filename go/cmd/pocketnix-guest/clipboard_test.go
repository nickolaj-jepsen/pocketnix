package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/agent"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/transport"
)

// fakeWayland is a Wayland selection: what the last copy offered, by type.
type fakeWayland struct {
	mu      sync.Mutex
	offered []string
	data    map[string][]byte
}

// copy takes the selection as an app does, offering data under each of types.
func (f *fakeWayland) copy(data []byte, types ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offered, f.data = types, map[string][]byte{}
	for _, t := range types {
		f.data[t] = data
	}
}

// selection returns the types offered and the data of the first.
func (f *fakeWayland) selection() ([]string, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.offered) == 0 {
		return nil, nil
	}
	return f.offered, f.data[f.offered[0]]
}

func (f *fakeWayland) types() ([]string, error) {
	types, _ := f.selection()
	if types == nil {
		return nil, errors.New("nothing is copied")
	}
	return types, nil
}

func (f *fakeWayland) read(t string, limit int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.data[t]
	if !ok {
		return nil, errors.New("not offered")
	}
	if int64(len(d)) > limit {
		return nil, errTooLarge
	}
	return d, nil
}

func (f *fakeWayland) offer(t string, data []byte) error {
	f.copy(data, t)
	return nil
}

// startAgent serves an agent's guest socket with the app's control stream connected.
func startAgent(t *testing.T) (string, *client.Client, *client.Control) {
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
	addr := "unix:" + filepath.Join(dir, "agent.sock")
	l, err := transport.Listen(addr)
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
	c := &client.Client{App: "test", Dial: func() (net.Conn, error) { return transport.Dial(addr) }}
	ctl, err := c.Control()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctl.Close() })
	// Events only reach a control stream that's registered, which the first push shows.
	if _, err := ctl.WaitUpdates(func(protocol.Updates) bool { return true }); err != nil {
		t.Fatal(err)
	}
	return sock, c, ctl
}

func newBridge(t *testing.T, sock string) (*bridge, *fakeWayland) {
	wl := &fakeWayland{}
	return &bridge{sock: sock, wl: wl, logf: t.Logf}, wl
}

// clipboardEvents follows the agent's clipboard as the bridge does.
func clipboardEvents(t *testing.T, sock string) func() protocol.ClipboardEvent {
	t.Helper()
	sub, err := client.SubscribeClipboard(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sub.Close() })
	return func() protocol.ClipboardEvent {
		t.Helper()
		ev := make(chan protocol.ClipboardEvent, 1)
		go func() {
			e, _ := sub.Next()
			ev <- e
		}()
		select {
		case e := <-ev:
			return e
		case <-time.After(5 * time.Second):
			t.Fatal("no clipboard event within 5s")
		}
		panic("unreachable")
	}
}

func appEvent(t *testing.T, ctl *client.Control) protocol.Event {
	t.Helper()
	select {
	case ev := <-ctl.Events():
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no app event within 5s")
	}
	panic("unreachable")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestPickType(t *testing.T) {
	for _, c := range []struct {
		offer []string
		want  string
	}{
		{[]string{"text/html", "image/png", "text/plain", "text/plain;charset=utf-8"}, "text/plain"},
		{[]string{"text/plain;charset=iso-8859-1", "text/plain;charset=utf-8"}, "text/plain;charset=utf-8"},
		{[]string{"text/plain;charset=iso-8859-1", "image/png"}, "text/plain;charset=iso-8859-1"},
		{[]string{"text/html", "image/png"}, "image/png"},
		{[]string{"application/x-thing", "text/uri-list"}, "text/uri-list"},
		{[]string{"application/x-thing", "application/pdf"}, "application/x-thing"},
		{[]string{"x-kde-passwordManagerHint", "UTF8_STRING", "text/plain"}, "text/plain"},
		{[]string{"SAVE_TARGETS"}, ""},
	} {
		if got := pickType(c.offer); got != c.want {
			t.Errorf("pickType(%q) = %q, want %q", c.offer, got, c.want)
		}
	}
}

func TestBridgeDropsItsOwnEcho(t *testing.T) {
	sock, _, ctl := startAgent(t)
	b, wl := newBridge(t, sock)
	next := clipboardEvents(t, sock)

	must(t, ctl.SetClipboard("from-android"))
	must(t, b.fromAgent(next()))
	if _, data := wl.selection(); string(data) != "from-android" {
		t.Fatalf("Wayland holds %q", data)
	}
	// wl-paste --watch reports the bridge's own copy: it mustn't go back.
	must(t, b.fromWayland("data"))
	if got, err := client.Guest(sock, protocol.GuestRequest{Type: protocol.GuestPaste}); err != nil || got != "from-android" {
		t.Fatalf("agent's clipboard = %q, %v", got, err)
	}

	wl.copy([]byte("from-wayland"), "text/html", "text/plain;charset=utf-8")
	must(t, b.fromWayland("data"))
	if ev := appEvent(t, ctl); ev.Type != protocol.TypeGuestClipboard || ev.Text != "from-wayland" {
		t.Fatalf("app event = %+v, want from-wayland and nothing before it", ev)
	}
	// The agent publishes that copy; taking it back would replace the app's offer.
	must(t, b.fromAgent(next()))
	if types, _ := wl.selection(); !slices.Equal(types, []string{"text/html", "text/plain;charset=utf-8"}) {
		t.Errorf("Wayland offers %q, want the app's offer", types)
	}

	// A copy from a tab reaches Wayland.
	if _, err := client.Guest(sock, protocol.GuestRequest{Type: protocol.GuestCopy, Text: "from-a-tab"}); err != nil {
		t.Fatal(err)
	}
	must(t, b.fromAgent(next()))
	if _, data := wl.selection(); string(data) != "from-a-tab" {
		t.Errorf("Wayland holds %q", data)
	}
}

func TestBridgeLeavesAndroidAloneOnNil(t *testing.T) {
	sock, _, ctl := startAgent(t)
	b, wl := newBridge(t, sock)
	next := clipboardEvents(t, sock)
	must(t, ctl.SetClipboard("kept"))
	must(t, b.fromAgent(next()))

	// The bridge's wl-copy exited: the selection is empty.
	wl.copy(nil)
	must(t, b.fromWayland("nil"))
	if got, err := client.Guest(sock, protocol.GuestRequest{Type: protocol.GuestPaste}); err != nil || got != "kept" {
		t.Errorf("agent's clipboard = %q, %v", got, err)
	}
	// Android's same clipboard again is no echo now: Wayland lost it.
	must(t, ctl.SetClipboard("kept"))
	must(t, b.fromAgent(next()))
	if _, data := wl.selection(); string(data) != "kept" {
		t.Errorf("Wayland holds %q", data)
	}
}

func TestBridgeMarksSensitiveCopies(t *testing.T) {
	sock, _, ctl := startAgent(t)
	b, wl := newBridge(t, sock)
	wl.copy([]byte("hunter2"), "text/plain;charset=utf-8", sensitiveHint)
	must(t, b.fromWayland("sensitive"))
	if ev := appEvent(t, ctl); ev.Text != "hunter2" || !ev.Sensitive {
		t.Errorf("app event = %+v, want sensitive", ev)
	}
}

func TestBridgeTypedData(t *testing.T) {
	sock, c, ctl := startAgent(t)
	b, wl := newBridge(t, sock)
	next := clipboardEvents(t, sock)

	// Android's image: the bridge fetches it from the app.
	jpeg := []byte("JFIF from android")
	must(t, ctl.SetClipboardTypes("", []string{"image/jpeg"}))
	done := make(chan error, 1)
	go func() { done <- b.fromAgent(next()) }()
	ev := appEvent(t, ctl)
	if ev.Type != protocol.TypeClipboardRequest || ev.MIME != "image/jpeg" {
		t.Fatalf("app event = %+v", ev)
	}
	if _, err := c.ShareFor(ev.Ref, "", jpeg); err != nil {
		t.Fatal(err)
	}
	must(t, <-done)
	if types, data := wl.selection(); !slices.Equal(types, []string{"image/jpeg"}) || !bytes.Equal(data, jpeg) {
		t.Errorf("Wayland holds %q %q", types, data)
	}

	// A Wayland app's image: the app fetches it, and its echo leaves the app's offer.
	png := []byte("\x89PNG from wayland")
	wl.copy(png, "image/png", "text/html")
	go func() { done <- b.fromWayland("data") }()
	ev = appEvent(t, ctl)
	if ev.Type != protocol.TypeGuestClipboardData || ev.MIME != "image/png" {
		t.Fatalf("app event = %+v", ev)
	}
	var got []byte
	must(t, c.Fetch(ev.Ref, func(_ string, r io.Reader) (err error) { got, err = io.ReadAll(r); return err }))
	must(t, <-done)
	if !bytes.Equal(got, png) {
		t.Errorf("the app got %q", got)
	}
	must(t, b.fromAgent(next()))
	if types, _ := wl.selection(); !slices.Equal(types, []string{"image/png", "text/html"}) {
		t.Errorf("Wayland offers %q, want the app's offer", types)
	}
}

func TestBridgeSkipsOversizedText(t *testing.T) {
	sock, _, ctl := startAgent(t)
	b, wl := newBridge(t, sock)
	logged := make(chan error, 4)
	b.logf = func(format string, args ...any) {
		t.Logf(format, args...)
		if err, ok := args[0].(error); ok {
			logged <- err
		}
	}
	changes, events := newSlot[string](), newSlot[protocol.ClipboardEvent]()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.run(ctx, changes, events)

	wl.copy([]byte(strings.Repeat("x", protocol.MaxClipboardText+1)), "text/plain")
	changes.put("data")
	select {
	case err := <-logged:
		if !errors.Is(err, errTooLarge) {
			t.Errorf("logged %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing logged within 5s")
	}
	// It keeps running: the next copy crosses, and the large one never did.
	wl.copy([]byte("small"), "text/plain")
	changes.put("data")
	if ev := appEvent(t, ctl); ev.Text != "small" {
		t.Errorf("app event = %+v", ev)
	}
}
