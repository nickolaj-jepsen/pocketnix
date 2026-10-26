package agent

import (
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
