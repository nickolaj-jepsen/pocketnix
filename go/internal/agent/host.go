package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sync"

	"github.com/godbus/dbus/v5"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// hostTargets are the module's targets, each active while its condition holds.
var hostTargets = []struct {
	unit string
	on   func(protocol.HostState) bool
}{
	{"pocketnix-online.target", func(h protocol.HostState) bool { return h.Online }},
	{"pocketnix-unmetered.target", func(h protocol.HostState) bool { return h.Online && !h.Metered }},
	{"pocketnix-charging.target", func(h protocol.HostState) bool { return h.Charging }},
	{"pocketnix-power-save.target", func(h protocol.HostState) bool { return h.PowerSave }},
}

// hostTracker keeps the phone's state, as the app last sent it.
type hostTracker struct {
	mu    sync.Mutex
	state protocol.HostState
	// owner is the control stream that sent state; its end makes the state unknown.
	owner *lockedWriter
	// kick wakes the applier, which works on the latest state.
	kick chan struct{}
	once sync.Once

	// The applier's own: the targets it last set (nil: unknown) and the zone.
	active map[string]bool
	zone   string
}

// setHostState takes the phone's state from the control stream w.
func (s *Server) setHostState(w *lockedWriter, st *protocol.HostState) error {
	if st == nil {
		return errors.New("host-state without a host")
	}
	h := &s.host
	h.mu.Lock()
	h.owner = w
	h.state = *st
	h.state.Connected = true
	h.mu.Unlock()
	s.hostChanged()
	return nil
}

// hostLost forgets what w said about the phone, if it said the latest:
// with no app, the guest is offline and on battery as far as it knows. The
// timezone and power saving stay as they were.
func (s *Server) hostLost(w *lockedWriter) {
	h := &s.host
	h.mu.Lock()
	if h.owner != w || w == nil {
		h.mu.Unlock()
		return
	}
	h.owner = nil
	h.state.Connected, h.state.Online, h.state.Charging = false, false, false
	h.mu.Unlock()
	s.hostChanged()
}

// hostState is the phone's state as the agent knows it.
func (s *Server) hostState() protocol.HostState {
	s.host.mu.Lock()
	defer s.host.mu.Unlock()
	return s.host.state
}

// knownOffline is true only when the app said the phone is offline.
func (s *Server) knownOffline() bool {
	st := s.hostState()
	return st.Connected && !st.Online
}

// hostChanged has the applier catch up with the latest state. Applying runs
// systemctl and D-Bus calls, which a control request mustn't wait for.
func (s *Server) hostChanged() {
	h := &s.host
	h.once.Do(func() {
		h.kick = make(chan struct{}, 1)
		go s.applyHost()
	})
	select {
	case h.kick <- struct{}{}:
	default:
	}
}

func (s *Server) applyHost() {
	for range s.host.kick {
		st := s.hostState()
		if s.HostFile != "" {
			if err := writeHostFile(s.HostFile, st); err != nil {
				log.Printf("host state: %v", err)
			}
		}
		if s.Systemctl != "" {
			s.applyTargets(st)
		}
		if s.config().Host.TimeZone && st.TimeZone != "" && st.TimeZone != s.host.zone {
			if err := setTimeZone(st.TimeZone); err != nil {
				log.Printf("timezone %s: %v", st.TimeZone, err)
			} else {
				log.Printf("timezone set to %s", st.TimeZone)
				s.host.zone = st.TimeZone
			}
		}
	}
}

// applyTargets starts and stops the module's targets to match st.
func (s *Server) applyTargets(st protocol.HostState) {
	var start, stop []string
	want := map[string]bool{}
	for _, t := range hostTargets {
		on := t.on(st)
		want[t.unit] = on
		if cur, known := s.host.active[t.unit]; known && cur == on {
			continue
		}
		if on {
			start = append(start, t.unit)
		} else {
			stop = append(stop, t.unit)
		}
	}
	s.host.active = want
	for _, c := range []struct {
		verb  string
		units []string
	}{{"stop", stop}, {"start", start}} {
		if len(c.units) == 0 {
			continue
		}
		// --no-block: a target's wanted units may take their time.
		out, err := exec.Command(s.Systemctl, append([]string{"--no-block", "--no-ask-password", c.verb}, c.units...)...).CombinedOutput()
		if err != nil {
			log.Printf("systemctl %s %v: %v: %s", c.verb, c.units, err, out)
			s.host.active = nil // try them all again next time
		}
	}
}

// writeHostFile replaces path with st's JSON, readable by everyone.
func writeHostFile(path string, st protocol.HostState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".host-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(append(b, '\n'))
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

var zoneName = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$`)

// tzdirs are where the guest's tz database may be, $TZDIR first.
func tzdirs() []string {
	return slices.DeleteFunc([]string{os.Getenv("TZDIR"), "/etc/zoneinfo", "/usr/share/zoneinfo"}, func(d string) bool { return d == "" })
}

// validZone checks that id names a zone file under one of dirs.
func validZone(id string, dirs []string) error {
	if !zoneName.MatchString(id) {
		return fmt.Errorf("%q is not a tz database id", id)
	}
	for _, d := range dirs {
		if fi, err := os.Stat(filepath.Join(d, id)); err == nil && fi.Mode().IsRegular() {
			return nil
		}
	}
	return fmt.Errorf("no zone %q in %v", id, dirs)
}

// setTimeZone sets the system timezone through timedated, as timedatectl does.
func setTimeZone(id string) error {
	if err := validZone(id, tzdirs()); err != nil {
		return err
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Object("org.freedesktop.timedate1", "/org/freedesktop/timedate1").
		Call("org.freedesktop.timedate1.SetTimezone", 0, id, false).Err
}

// controlRequest runs a request that belongs to the stream it came on, or
// else hands it to [Server.request].
func (s *Server) controlRequest(w *lockedWriter, req protocol.Request) (any, error) {
	if req.Type == protocol.TypeHostState {
		return protocol.Envelope{Type: req.Type}, s.setHostState(w, req.Host)
	}
	return s.request(req)
}
