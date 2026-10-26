package agent_test

import (
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

func TestNotificationsRoundTrip(t *testing.T) {
	h, _, _, sock := startIntegrations(t, nil)
	ctl, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	if _, err := ctl.WaitUpdates(func(protocol.Updates) bool { return true }); err != nil {
		t.Fatal(err)
	}
	sub, err := client.Subscribe(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	events := make(chan protocol.GuestEvent, 8)
	go func() {
		for {
			ev, err := sub.Next()
			if err != nil {
				close(events)
				return
			}
			events <- ev
		}
	}()
	nextGuestEvent := func() protocol.GuestEvent {
		t.Helper()
		select {
		case ev := <-events:
			return ev
		case <-time.After(5 * time.Second):
			t.Fatal("no guest event within 5s")
		}
		panic("unreachable")
	}

	actions := []protocol.NotifyAction{{Key: "ok", Label: "OK"}, {Key: "default", Label: "Open"}}
	r, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestNotify, Title: "build", Body: "done", Urgency: protocol.UrgencyCritical, AppName: "make", Actions: actions})
	if err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, ctl)
	if ev.Type != protocol.TypeNotify || ev.Notification != r.ID || r.ID == 0 || ev.Urgency != protocol.UrgencyCritical || ev.AppName != "make" || !equalJSON(ev.Actions, actions) {
		t.Errorf("event = %+v, id %d", ev, r.ID)
	}

	// A replacement keeps its id; a new one gets another.
	r2, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestNotify, Title: "build", Body: "again", Replaces: r.ID})
	if err != nil || r2.ID != r.ID {
		t.Errorf("replace = %d, %v; want %d", r2.ID, err, r.ID)
	}
	if ev := nextEvent(t, ctl); ev.Notification != r.ID || ev.Body != "again" {
		t.Errorf("event = %+v", ev)
	}
	r3, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestNotify, Body: "other"})
	if err != nil || r3.ID == r.ID {
		t.Errorf("new = %d, %v", r3.ID, err)
	}
	nextEvent(t, ctl)

	// The app's answers reach the subscriber.
	if err := ctl.NotifyAction(r.ID, "ok"); err != nil {
		t.Fatal(err)
	}
	if ev := nextGuestEvent(); ev != (protocol.GuestEvent{Type: protocol.TypeNotifyAction, ID: r.ID, Key: "ok"}) {
		t.Errorf("guest event = %+v", ev)
	}
	if err := ctl.NotifyClosed(r3.ID, protocol.ClosedDismissed); err != nil {
		t.Fatal(err)
	}
	if ev := nextGuestEvent(); ev != (protocol.GuestEvent{Type: protocol.TypeNotifyClosed, ID: r3.ID, Reason: protocol.ClosedDismissed}) {
		t.Errorf("guest event = %+v", ev)
	}

	// A guest close withdraws the app's notification.
	if _, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestNotifyClose, ID: r3.ID}); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, ctl); ev.Type != protocol.TypeNotifyClose || ev.Notification != r3.ID {
		t.Errorf("event = %+v", ev)
	}

	if _, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestNotify, Body: "x", Urgency: "loud"}); err == nil {
		t.Error("an unknown urgency was accepted")
	}
	// Pushes and subscriptions don't disturb request pairing.
	if err := ctl.Ping(); err != nil {
		t.Errorf("ping: %v", err)
	}
}
