package agent

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

const (
	login1        = "org.freedesktop.login1"
	login1Path    = "/org/freedesktop/login1"
	login1Manager = "org.freedesktop.login1.Manager"
)

// inhibitor is one entry of logind's ListInhibitors.
type inhibitor struct {
	What, Who, Why, Mode string
	UID, PID             uint32
}

// wakeOf is the wake state a list of inhibitors makes: block-mode sleep or
// idle inhibitors whose who isn't ignored, sorted and without repeats so
// that an unchanged set is an unchanged state.
func wakeOf(list []inhibitor, c WakeLocksConfig) protocol.Wake {
	w := protocol.Wake{Type: protocol.TypeWake, Holders: []protocol.WakeHolder{}}
	if !c.Enable {
		return w
	}
	for _, i := range list {
		what := strings.Split(i.What, ":")
		if i.Mode != "block" || !(slices.Contains(what, "sleep") || slices.Contains(what, "idle")) || slices.Contains(c.Ignore, i.Who) {
			continue
		}
		w.Holders = append(w.Holders, protocol.WakeHolder{Who: i.Who, Why: i.Why})
	}
	slices.SortFunc(w.Holders, func(a, b protocol.WakeHolder) int {
		return cmp.Or(strings.Compare(a.Who, b.Who), strings.Compare(a.Why, b.Why))
	})
	w.Holders = slices.Compact(w.Holders)
	w.Held = len(w.Holders) > 0
	return w
}

// wakeTracker keeps logind's last inhibitor list and the wake state the
// app last heard.
type wakeTracker struct {
	mu   sync.Mutex
	list []inhibitor
	last protocol.Wake
}

// wake is the current wake state, for a control stream that just opened.
func (s *Server) wake() protocol.Wake {
	s.wakes.mu.Lock()
	defer s.wakes.mu.Unlock()
	return wakeOf(s.wakes.list, s.config().Power.WakeLocks)
}

// setInhibitors takes logind's list (nil: unknown) and pushes the wake
// state when it changed.
func (s *Server) setInhibitors(list []inhibitor) {
	s.wakes.mu.Lock()
	s.wakes.list = list
	s.wakes.mu.Unlock()
	s.wakeChanged()
}

// wakeChanged pushes the wake state if it differs from the last one pushed:
// after a new inhibitor list, or a config that filters it differently.
func (s *Server) wakeChanged() {
	s.wakes.mu.Lock()
	w := wakeOf(s.wakes.list, s.config().Power.WakeLocks)
	same := w.Held == s.wakes.last.Held && slices.Equal(w.Holders, s.wakes.last.Holders)
	s.wakes.last = w
	s.wakes.mu.Unlock()
	if same {
		return
	}
	if w.Held {
		log.Printf("asking the phone to stay awake for %v", w.Holders)
	} else {
		log.Printf("nothing keeps the phone awake")
	}
	s.pushLatest(func() any { return s.wake() })
}

// WatchInhibitors follows logind's inhibitors until ctx ends. logind
// signals every change, so an idle guest isn't woken to look.
func (s *Server) WatchInhibitors(ctx context.Context) {
	for {
		err := s.watchInhibitors(ctx)
		s.setInhibitors(nil)
		if ctx.Err() != nil {
			return
		}
		log.Printf("logind inhibitors: %v (retrying)", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func (s *Server) watchInhibitors(ctx context.Context) error {
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return err
	}
	defer conn.Close()
	// BlockInhibited and NCurrentInhibitors change with every inhibitor.
	if err := conn.AddMatchSignalContext(ctx,
		dbus.WithMatchSender(login1), dbus.WithMatchObjectPath(login1Path),
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"), dbus.WithMatchMember("PropertiesChanged"),
		dbus.WithMatchArg(0, login1Manager),
	); err != nil {
		return err
	}
	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	obj := conn.Object(login1, login1Path)
	for {
		var list []inhibitor
		if err := obj.CallWithContext(ctx, login1Manager+".ListInhibitors", 0).Store(&list); err != nil {
			return err
		}
		s.setInhibitors(list)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-signals:
			if !ok {
				return errors.New("system bus connection closed")
			}
		}
		// One list answers a burst of signals.
		for len(signals) > 0 {
			<-signals
		}
	}
}

// LogindInhibit takes a logind sleep inhibitor (mode block) as "pocketnix",
// held until the returned file is closed.
func LogindInhibit(why string) (io.Closer, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var fd dbus.UnixFD
	if err := conn.Object(login1, login1Path).Call(login1Manager+".Inhibit", 0, "sleep", "pocketnix", why, "block").Store(&fd); err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "logind inhibitor"), nil
}
