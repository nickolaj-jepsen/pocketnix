package protocol

// GUI apps (docs/gui.md). The GUI protocol itself is the compositor's
// and the app's (docs/gui-protocol.md); the agent only carries it.

// StreamGUI is spliced byte for byte to the guest compositor's GUI socket
// after the agent's Hello. The agent refuses it while [GUI.Enable] is off.
const StreamGUI = "gui"

// GUI is the module's GUI configuration, in [Config].
type GUI struct {
	// Enable is whether the guest runs the compositor; the app then keeps a
	// gui stream open.
	Enable   bool        `json:"enable"`
	Defaults AppSettings `json:"defaults"`
	// AppDefaults override Defaults by Wayland app id; a field left empty
	// inherits the default's.
	AppDefaults map[string]AppSettings `json:"appDefaults,omitempty"`
}

// AppSettings are an app's window settings; empty fields are unset.
type AppSettings struct {
	// Size is one of the Size* presets.
	Size string `json:"size,omitempty"`
	// Input is one of the Input* touch modes.
	Input string `json:"input,omitempty"`
}

// Size presets: the window's scale relative to Android's density.
const (
	SizePhone   = "phone"
	SizeCompact = "compact"
	SizeDesktop = "desktop"
)

// Touch modes: what a touch on the window becomes.
const (
	InputTouch    = "touch"
	InputPointer  = "pointer"
	InputTrackpad = "trackpad"
)
