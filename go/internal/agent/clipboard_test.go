package agent_test

import (
	"fmt"
	"io"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// clipboardEvents follows a clipboard subscription on sock.
func clipboardEvents(t *testing.T, sock string) func() protocol.ClipboardEvent {
	t.Helper()
	sub, err := client.SubscribeClipboard(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sub.Close() })
	events := make(chan protocol.ClipboardEvent, 1)
	go func() {
		defer close(events)
		for {
			ev, err := sub.Next()
			if err != nil {
				return
			}
			events <- ev
		}
	}()
	return func() protocol.ClipboardEvent {
		t.Helper()
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("the clipboard subscription ended")
			}
			return ev
		case <-time.After(5 * time.Second):
			t.Fatal("no clipboard event within 5s")
		}
		panic("unreachable")
	}
}

func TestClipboardSubscription(t *testing.T) {
	h, _, _, sock := startIntegrations(t, nil)
	ctl := connectApp(t, h)
	must(t, ctl.SetClipboard("before"))

	next := clipboardEvents(t, sock)
	if ev := next(); ev.Type != protocol.TypeClipboard || ev.Text != "before" || !slices.Equal(ev.Types, []string{"text/plain"}) {
		t.Errorf("first event = %+v, want the clipboard as it is", ev)
	}
	must(t, ctl.SetClipboardTypes("", []string{"image/png"}))
	if ev := next(); ev.Text != "" || !slices.Equal(ev.Types, []string{"image/png"}) {
		t.Errorf("after Android's image = %+v", ev)
	}

	// Guest copies are published too, text and typed alike.
	if _, err := client.Guest(sock, protocol.GuestRequest{Type: protocol.GuestCopy, Text: "from a tab"}); err != nil {
		t.Fatal(err)
	}
	if ev := next(); ev.Text != "from a tab" || !slices.Equal(ev.Types, []string{"text/plain"}) {
		t.Errorf("after a guest copy = %+v", ev)
	}
	eventOf(t, ctl, protocol.TypeGuestClipboard)
	in, w, _ := os.Pipe()
	defer in.Close()
	w.Write([]byte("JFIF"))
	w.Close()
	done := guestAsync(sock, protocol.GuestRequest{Type: protocol.GuestCopy, MIME: "image/jpeg"}, in)
	if ev := next(); ev.Text != "" || !slices.Equal(ev.Types, []string{"image/jpeg"}) {
		t.Errorf("after a typed guest copy = %+v", ev)
	}
	ev := eventOf(t, ctl, protocol.TypeGuestClipboardData)
	must(t, h.client.Fetch(ev.Ref, func(_ string, r io.Reader) error { _, err := io.Copy(io.Discard, r); return err }))
	if r := result(t, done); r.err != nil {
		t.Fatal(r.err)
	}
}

func TestClipboardSubscriberEndsOnTheLatest(t *testing.T) {
	h, _, _, sock := startIntegrations(t, nil)
	ctl := connectApp(t, h)
	next := clipboardEvents(t, sock)
	for i := range 100 {
		must(t, ctl.SetClipboard(fmt.Sprint(i)))
	}
	seen := -1
	for seen != 99 {
		var n int
		ev := next()
		if _, err := fmt.Sscan(ev.Text, &n); err != nil || n <= seen {
			t.Fatalf("got %q after %d", ev.Text, seen)
		}
		seen = n
	}
	// Nothing older waits behind the last value.
	must(t, ctl.SetClipboard("after"))
	if ev := next(); ev.Text != "after" {
		t.Errorf("next event = %+v", ev)
	}
}

func TestSensitiveGuestCopy(t *testing.T) {
	h, _, _, sock := startIntegrations(t, nil)
	ctl := connectApp(t, h)
	if _, err := client.Guest(sock, protocol.GuestRequest{Type: protocol.GuestCopy, Text: "hunter2", Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	if ev := eventOf(t, ctl, protocol.TypeGuestClipboard); !ev.Sensitive {
		t.Errorf("event = %+v, want sensitive", ev)
	}
	in, w, _ := os.Pipe()
	defer in.Close()
	w.Write([]byte("key"))
	w.Close()
	done := guestAsync(sock, protocol.GuestRequest{Type: protocol.GuestCopy, MIME: "application/x-key", Sensitive: true}, in)
	ev := eventOf(t, ctl, protocol.TypeGuestClipboardData)
	if !ev.Sensitive {
		t.Errorf("typed event = %+v, want sensitive", ev)
	}
	must(t, h.client.Fetch(ev.Ref, func(_ string, r io.Reader) error { _, err := io.Copy(io.Discard, r); return err }))
	if r := result(t, done); r.err != nil {
		t.Fatal(r.err)
	}
}

// A subscriber learns a copy was sensitive, from when it subscribes on.
func TestClipboardSubscriptionMarksSensitiveCopies(t *testing.T) {
	h, _, _, sock := startIntegrations(t, nil)
	ctl := connectApp(t, h)
	if _, err := client.Guest(sock, protocol.GuestRequest{Type: protocol.GuestCopy, Text: "hunter2", Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	next := clipboardEvents(t, sock)
	if ev := next(); ev.Text != "hunter2" || !ev.Sensitive {
		t.Errorf("first event = %+v, want the copy marked sensitive", ev)
	}
	must(t, ctl.SetClipboard("plain"))
	if ev := next(); ev.Sensitive {
		t.Errorf("after Android's clipboard = %+v, want it unmarked", ev)
	}
}

func TestNotificationSubscriberGetsNoClipboard(t *testing.T) {
	h, _, _, sock := startIntegrations(t, nil)
	ctl := connectApp(t, h)
	sub, err := client.Subscribe(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	must(t, ctl.SetClipboard("from-android"))
	if _, err := client.Guest(sock, protocol.GuestRequest{Type: protocol.GuestCopy, Text: "from-guest"}); err != nil {
		t.Fatal(err)
	}
	// The control stream is served in order: a clipboard event would come first.
	must(t, ctl.NotifyAction(7, "ok"))
	got := make(chan protocol.GuestEvent, 1)
	go func() {
		ev, _ := sub.Next()
		got <- ev
	}()
	select {
	case ev := <-got:
		if ev != (protocol.GuestEvent{Type: protocol.TypeNotifyAction, ID: 7, Key: "ok"}) {
			t.Errorf("notification subscriber got %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no guest event within 5s")
	}
}
