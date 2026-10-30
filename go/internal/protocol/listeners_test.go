package protocol

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestPortListenersRoundTrip(t *testing.T) {
	const wire = `{"type":"port-listeners","lan":true,"ports":[3000,8123]}`
	var p PortListeners
	if err := json.Unmarshal([]byte(wire), &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != TypePortListeners || !p.LAN || !slices.Equal(p.Ports, []int{3000, 8123}) {
		t.Fatalf("decoded %+v", p)
	}
	out, _ := json.Marshal(p)
	if string(out) != wire {
		t.Errorf("encoded %s, want %s", out, wire)
	}
}

func TestEmptyPortListenersKeepTheirFields(t *testing.T) {
	out, _ := json.Marshal(PortListeners{})
	if want := `{"type":"port-listeners","lan":false,"ports":[]}`; string(out) != want {
		t.Errorf("encoded %s, want %s", out, want)
	}
}

func TestSensitiveClipboardRoundTrip(t *testing.T) {
	const wire = `{"type":"guest-clipboard","text":"hunter2","sensitive":true}`
	var ev Event
	if err := json.Unmarshal([]byte(wire), &ev); err != nil {
		t.Fatal(err)
	}
	if !ev.Sensitive || ev.Text != "hunter2" {
		t.Fatalf("decoded %+v", ev)
	}
	out, _ := json.Marshal(ev)
	if string(out) != wire {
		t.Errorf("encoded %s, want %s", out, wire)
	}
	plain, _ := json.Marshal(Event{Type: TypeGuestClipboard, Text: "x"})
	if want := `{"type":"guest-clipboard","text":"x"}`; string(plain) != want {
		t.Errorf("a plain copy encoded %s, want %s", plain, want)
	}
}

func TestSensitiveGuestCopyRoundTrip(t *testing.T) {
	const wire = `{"type":"copy","text":"hunter2","sensitive":true}`
	var req GuestRequest
	if err := json.Unmarshal([]byte(wire), &req); err != nil {
		t.Fatal(err)
	}
	if !req.Sensitive || req.Type != GuestCopy {
		t.Fatalf("decoded %+v", req)
	}
	out, _ := json.Marshal(req)
	if string(out) != wire {
		t.Errorf("encoded %s, want %s", out, wire)
	}
}
