package protocol

// The phone and the guest's power (design.md, Lifecycle): the guest asks
// the phone to stay awake.

// TypeWake is the agent's push of [Wake], on connect and on every change.
const TypeWake = "wake"

// Wake says whether something in the guest holds a logind sleep or idle
// inhibitor (mode block), which the app answers with a wake lock.
type Wake struct {
	Type    string       `json:"type"` // "wake"
	Held    bool         `json:"held"`
	Holders []WakeHolder `json:"holders"`
}

// WakeHolder is one inhibitor that keeps the phone awake, as logind lists it.
type WakeHolder struct {
	Who string `json:"who"`
	Why string `json:"why,omitempty"`
}
