package protocol

// The clipboard as guest programs follow it (`pocketnix-guest clipboard`,
// the Wayland bridge): the agent hands every change of its clipboard, whether
// Android's or a guest copy, to each [GuestSubscribeClipboard] connection.

// GuestSubscribeClipboard is answered once, then the connection carries a
// [ClipboardEvent] line for the clipboard as it is (when it holds anything)
// and one for each change after it, until either side closes it. A reader
// that falls behind gets only the latest change. No idle deadline applies.
const GuestSubscribeClipboard = "subscribe-clipboard"

// TypeClipboard is a [ClipboardEvent]'s type.
const TypeClipboard = "clipboard"

// ClipboardEvent is the agent's clipboard after a change: its MIME types,
// and its text when one of them is text/plain. Other types' bytes come from
// a typed [GuestPaste].
type ClipboardEvent struct {
	Type  string   `json:"type"`
	Text  string   `json:"text,omitempty"`
	Types []string `json:"types,omitempty"`
}

// Clipboard limits, both ways; the agent refuses more.
const (
	// MaxClipboardText bounds clipboard text: Android's binder can't carry much more.
	MaxClipboardText = 128 << 10
	// MaxClipboardData bounds typed clipboard data, which the agent keeps for a guest paste.
	MaxClipboardData = 32 << 20
)
