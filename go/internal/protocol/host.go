package protocol

// The phone and the guest's power and network (design.md, Lifecycle and
// Configuration): the guest asks the phone to stay awake, and the phone
// tells the guest what it's on.

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

// TypeHostState is the app's request carrying [Request.Host]: on connect,
// and when the phone's state changes. Agents older than this answer an Error.
const TypeHostState = "host-state"

// HostState is the phone's state as the guest may want it.
type HostState struct {
	// TimeZone is a tz database id, such as Europe/Copenhagen.
	TimeZone string `json:"timeZone,omitempty"`
	// Online is a validated default network; Metered is that network's.
	Online  bool `json:"online"`
	Metered bool `json:"metered"`
	// Charging is on external power, charging or full.
	Charging   bool `json:"charging"`
	BatteryLow bool `json:"batteryLow"`
	PowerSave  bool `json:"powerSave"`
	// Connected is the agent's: an app is connected and the rest is
	// current. Without one, the agent reports offline and not charging.
	Connected bool `json:"connected"`
}

// GuestHost answers the agent's current [HostState] as JSON text.
const GuestHost = "host"
