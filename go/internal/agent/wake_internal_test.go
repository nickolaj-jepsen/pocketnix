package agent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

func TestWakeOf(t *testing.T) {
	list := []inhibitor{
		{What: "sleep", Who: "rsync", Why: "backup", Mode: "block"},
		{What: "shutdown:sleep", Who: "NetworkManager", Why: "disconnect first", Mode: "delay"},
		{What: "handle-lid-switch", Who: "desktop", Why: "lid", Mode: "block"},
		{What: "idle", Who: "mpv", Why: "playing", Mode: "block"},
		{What: "sleep:idle", Who: "forever", Why: "always", Mode: "block"},
		// The same holder twice is one entry.
		{What: "sleep", Who: "rsync", Why: "backup", Mode: "block"},
	}
	on := WakeLocksConfig{Enable: true, Ignore: []string{"forever"}}
	w := wakeOf(list, on)
	want := []protocol.WakeHolder{{Who: "mpv", Why: "playing"}, {Who: "rsync", Why: "backup"}}
	if !w.Held || !slices.Equal(w.Holders, want) {
		t.Errorf("wakeOf = %+v, want held by %v", w, want)
	}
	if w := wakeOf(list[1:3], on); w.Held || len(w.Holders) != 0 {
		t.Errorf("delay and lid inhibitors hold the phone: %+v", w)
	}
	if w := wakeOf(list, WakeLocksConfig{}); w.Held {
		t.Errorf("wake locks off, yet held: %+v", w)
	}
	if w := wakeOf(nil, on); w.Held || w.Holders == nil {
		t.Errorf("no inhibitors: %+v (holders must be an empty list)", w)
	}
}

func TestValidZone(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Europe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Europe", "Copenhagen"), []byte("TZif"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirs := []string{filepath.Join(dir, "missing"), dir}
	if err := validZone("Europe/Copenhagen", dirs); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"Europe", "Europe/Atlantis", "../etc/passwd", "/etc/passwd", "Europe/../Europe/Copenhagen", ""} {
		if validZone(bad, dirs) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
