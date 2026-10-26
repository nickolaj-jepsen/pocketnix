package agent_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// startHost serves a guest whose pocketnix-*.target units are files in a
// temp dir (a fake systemctl starts and stops them), with the host file and
// the guest socket there too.
func startHost(t *testing.T) (h *harness, active func() []string, hostFile, guestSock string) {
	t.Helper()
	h, _, _, guestSock = startIntegrations(t, nil)
	dir := t.TempDir()
	units := filepath.Join(dir, "units")
	must(t, os.Mkdir(units, 0o755))
	systemctl := filepath.Join(dir, "systemctl")
	must(t, os.WriteFile(systemctl, fmt.Appendf(nil, `#!/bin/sh
for a; do
  case $a in
  --*) ;;
  start|stop) verb=$a ;;
  *) if [ $verb = start ]; then touch %[1]s/$a; else rm -f %[1]s/$a; fi ;;
  esac
done
`, units), 0o755))
	h.srv.Systemctl = systemctl
	h.srv.HostFile = filepath.Join(dir, "host.json")
	active = func() []string {
		es, _ := os.ReadDir(units)
		var names []string
		for _, e := range es {
			names = append(names, e.Name())
		}
		return names
	}
	return h, active, h.srv.HostFile, guestSock
}

// eventually fails the test unless ok becomes true within 5 s.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no %s within 5s", what)
		}
	}
}

func wantTargets(t *testing.T, active func() []string, want ...string) {
	t.Helper()
	var got []string
	eventually(t, fmt.Sprintf("targets %v", want), func() bool {
		got = active()
		return slices.Equal(got, want)
	})
}

func readHostFile(t *testing.T, path string) protocol.HostState {
	t.Helper()
	var st protocol.HostState
	b, err := os.ReadFile(path)
	must(t, err)
	must(t, json.Unmarshal(b, &st))
	return st
}

func TestHostStateDrivesTargets(t *testing.T) {
	h, active, hostFile, guestSock := startHost(t)
	ctl, err := h.client.Control()
	must(t, err)
	defer ctl.Close()

	must(t, ctl.HostState(protocol.HostState{TimeZone: "Europe/Copenhagen", Online: true, Charging: true}))
	wantTargets(t, active, "pocketnix-charging.target", "pocketnix-online.target", "pocketnix-unmetered.target")
	if st := readHostFile(t, hostFile); !st.Connected || !st.Online || st.TimeZone != "Europe/Copenhagen" {
		t.Errorf("host file: %+v", st)
	}

	// A metered network is online but not unmetered; power saving has its own target.
	must(t, ctl.HostState(protocol.HostState{Online: true, Metered: true, PowerSave: true}))
	wantTargets(t, active, "pocketnix-online.target", "pocketnix-power-save.target")

	text, err := client.Guest(guestSock, protocol.GuestRequest{Type: protocol.GuestHost})
	must(t, err)
	var st protocol.HostState
	must(t, json.Unmarshal([]byte(text), &st))
	if !st.Metered || !st.PowerSave || !st.Connected {
		t.Errorf("pocketnix-guest host: %s", text)
	}
}

func TestHostStateLostWithItsStream(t *testing.T) {
	h, active, hostFile, _ := startHost(t)
	first, err := h.client.Control()
	must(t, err)
	defer first.Close()
	must(t, first.HostState(protocol.HostState{Online: true, Charging: true, PowerSave: true}))
	wantTargets(t, active, "pocketnix-charging.target", "pocketnix-online.target", "pocketnix-power-save.target", "pocketnix-unmetered.target")

	// Another stream that never sent a state (the CLI's events) changes nothing when it goes.
	other, err := h.client.Control()
	must(t, err)
	other.Close()
	time.Sleep(100 * time.Millisecond)
	wantTargets(t, active, "pocketnix-charging.target", "pocketnix-online.target", "pocketnix-power-save.target", "pocketnix-unmetered.target")

	// Unknown is not good: no network and no charger, but power saving stays.
	first.Close()
	wantTargets(t, active, "pocketnix-power-save.target")
	eventually(t, "a disconnected host file", func() bool { return !readHostFile(t, hostFile).Connected })
}
