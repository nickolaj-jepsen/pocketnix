package protocol

import "encoding/json"

// TypePortListeners is the agent's push of [PortListeners], on connect and
// whenever the set changes, and only while the module's forwardOffers is on.
const TypePortListeners = "port-listeners"

// PortListeners is the guest's TCP listeners the app may offer to forward:
// the session user's, on an address a forward can dial, minus module
// forwards and the agent's own ports. It is the whole set each time; the app
// keeps every offer's state.
type PortListeners struct {
	Type string `json:"type"` // "port-listeners"
	// LAN is forwardOffers.lan: an allowed forward binds 0.0.0.0, not 127.0.0.1.
	LAN bool `json:"lan"`
	// Ports are guest ports, ascending and unique.
	Ports []int `json:"ports"`
}

// MarshalJSON always writes the type and a ports array, empty rather than
// null: the app drops a snapshot without one.
func (p PortListeners) MarshalJSON() ([]byte, error) {
	type wire PortListeners
	w := wire(p)
	w.Type = TypePortListeners
	if w.Ports == nil {
		w.Ports = []int{}
	}
	return json.Marshal(w)
}
