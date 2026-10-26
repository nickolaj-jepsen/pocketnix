// Package notifyd is the guest's org.freedesktop.Notifications server:
// notifications on the session bus become Android notifications through the
// agent's guest socket, and Android's actions and dismissals come back as
// the spec's signals.
package notifyd

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

const (
	Name  = "org.freedesktop.Notifications"
	Path  = dbus.ObjectPath("/org/freedesktop/Notifications")
	iface = Name

	// transientTimeout is how long a transient notification without its own
	// timeout stays; Android has no notification that skips the shade.
	transientTimeout = 10 * time.Second
	maxResubscribe   = 30 * time.Second
)

// Daemon serves the notifications interface on one bus connection.
type Daemon struct {
	conn    *dbus.Conn
	socket  string
	version string

	mu sync.Mutex
	// live holds the notifications this daemon posted and Android still
	// shows, with their expiry timers (nil for none). Only these get signals.
	live   map[uint32]*time.Timer
	sub    *client.Subscription
	closed bool
}

// Serve takes the notifications name on conn and serves it, posting through
// the agent's guest socket at socket. It fails when another server holds the name.
func Serve(conn *dbus.Conn, socket, version string) (*Daemon, error) {
	d := &Daemon{conn: conn, socket: socket, version: version, live: map[uint32]*time.Timer{}}
	srv := server{d}
	if err := conn.Export(srv, Path, iface); err != nil {
		return nil, err
	}
	node := &introspect.Node{
		Name: string(Path),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			{
				Name:    iface,
				Methods: introspect.Methods(srv),
				Signals: []introspect.Signal{
					{Name: "NotificationClosed", Args: []introspect.Arg{{Name: "id", Type: "u"}, {Name: "reason", Type: "u"}}},
					{Name: "ActionInvoked", Args: []introspect.Arg{{Name: "id", Type: "u"}, {Name: "action_key", Type: "s"}}},
				},
			},
		},
	}
	if err := conn.Export(introspect.NewIntrospectable(node), Path, "org.freedesktop.DBus.Introspectable"); err != nil {
		return nil, err
	}
	// Subscribed before the name is ours, so the first notification's answer can't be missed.
	sub, err := client.Subscribe(socket)
	if err != nil {
		log.Printf("subscribe: %v (retrying)", err)
	}
	go d.follow(sub)
	reply, err := conn.RequestName(Name, dbus.NameFlagDoNotQueue)
	if err == nil && reply != dbus.RequestNameReplyPrimaryOwner {
		err = errors.New(Name + " is taken by another notification server")
	}
	if err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// Close stops following the agent; the bus connection stays the caller's.
func (d *Daemon) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.sub != nil {
		d.sub.Close()
	}
	for id, t := range d.live {
		if t != nil {
			t.Stop()
		}
		delete(d.live, id)
	}
}

// following records sub as the current subscription, false once closed.
func (d *Daemon) following(sub *client.Subscription) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		if sub != nil {
			sub.Close()
		}
		return false
	}
	d.sub = sub
	return true
}

// follow reads the agent's actions and closes, resubscribing whenever the
// agent goes away (it restarts with the guest).
func (d *Daemon) follow(sub *client.Subscription) {
	wait := time.Second
	for d.following(sub) {
		if sub == nil {
			time.Sleep(wait)
			wait = min(wait*2, maxResubscribe)
			sub, _ = client.Subscribe(d.socket)
			continue
		}
		wait = time.Second
		for {
			ev, err := sub.Next()
			if err != nil {
				break
			}
			d.answer(ev)
		}
		sub.Close()
		sub = nil
	}
}

func (d *Daemon) answer(ev protocol.GuestEvent) {
	if !d.forget(ev.ID) {
		return
	}
	switch ev.Type {
	case protocol.TypeNotifyAction:
		d.emit("ActionInvoked", ev.ID, ev.Key)
		// The app removed it; we don't do resident notifications.
		d.emit("NotificationClosed", ev.ID, uint32(protocol.ClosedDismissed))
	case protocol.TypeNotifyClosed:
		d.emit("NotificationClosed", ev.ID, ev.Reason)
	}
}

// forget drops id from the live set, reporting whether it was there.
func (d *Daemon) forget(id uint32) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.live[id]
	if t != nil {
		t.Stop()
	}
	delete(d.live, id)
	return ok
}

func (d *Daemon) emit(signal string, args ...any) {
	if err := d.conn.Emit(Path, iface+"."+signal, args...); err != nil {
		log.Printf("emit %s: %v", signal, err)
	}
}

// close withdraws a live notification from Android and reports why.
func (d *Daemon) close(id, reason uint32) {
	if !d.forget(id) {
		return
	}
	if _, err := client.GuestCall(d.socket, protocol.GuestRequest{Type: protocol.GuestNotifyClose, ID: id}); err != nil {
		log.Printf("close %d: %v", id, err)
	}
	d.emit("NotificationClosed", id, reason)
}

// server holds the D-Bus methods, apart from Daemon's own.
type server struct{ d *Daemon }

func (s server) GetCapabilities() ([]string, *dbus.Error) {
	return []string{"body", "actions", "persistence"}, nil
}

func (s server) GetServerInformation() (name, vendor, version, specVersion string, err *dbus.Error) {
	return "pocketnix", "pocketnix", s.d.version, "1.2", nil
}

func (s server) Notify(appName string, replacesID uint32, appIcon, summary, body string, actions []string, hints map[string]dbus.Variant, expireTimeout int32) (uint32, *dbus.Error) {
	req := protocol.GuestRequest{
		Type: protocol.GuestNotify, Title: summary, Body: body,
		Replaces: replacesID, AppName: appName, Urgency: urgency(hints),
	}
	for i := 0; i+1 < len(actions); i += 2 {
		req.Actions = append(req.Actions, protocol.NotifyAction{Key: actions[i], Label: actions[i+1]})
	}
	if req.Title == "" && req.Body == "" {
		// The spec allows an empty summary; Android would show a blank notification.
		req.Title = appName
	}
	// Held across the call: the app may answer before the reply arrives here.
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	reply, err := client.GuestCall(s.d.socket, req)
	if err != nil {
		return 0, dbus.MakeFailedError(err)
	}
	id := reply.ID
	timeout := time.Duration(expireTimeout) * time.Millisecond
	if expireTimeout < 0 && hintBool(hints, "transient") {
		timeout = transientTimeout
	}
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() { s.d.close(id, protocol.ClosedExpired) })
	}
	if old := s.d.live[id]; old != nil {
		old.Stop()
	}
	s.d.live[id] = timer
	return id, nil
}

func (s server) CloseNotification(id uint32) *dbus.Error {
	s.d.close(id, protocol.ClosedByCall)
	return nil
}

// urgency reads the spec's urgency hint (a byte, 0–2).
func urgency(hints map[string]dbus.Variant) string {
	v, ok := hints["urgency"]
	if !ok {
		return ""
	}
	var n int64
	switch u := v.Value().(type) {
	case byte:
		n = int64(u)
	case int32:
		n = int64(u)
	case uint32:
		n = int64(u)
	default:
		return ""
	}
	switch n {
	case 0:
		return protocol.UrgencyLow
	case 2:
		return protocol.UrgencyCritical
	default:
		return protocol.UrgencyNormal
	}
}

func hintBool(hints map[string]dbus.Variant, name string) bool {
	b, ok := hints[name].Value().(bool)
	return ok && b
}

// Run serves the session bus until its connection ends.
func Run(socket, version string) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return fmt.Errorf("session bus: %w", err)
	}
	defer conn.Close()
	d, err := Serve(conn, socket, version)
	if err != nil {
		return err
	}
	defer d.Close()
	<-conn.Context().Done()
	return errors.New("the session bus went away")
}
