// Package protocol is the wire contract between the app and the guest agent.
//
// The app always dials (a guest can't dial an app over AVF vsock), every
// connection to the same agent port. Each connection opens with one JSON line
// from the app, a [Hello] naming the stream it wants, and the agent answers
// with its own [Hello] or an [Error] before the stream's own traffic starts:
//
//   - control: newline-delimited JSON messages both ways for the app's
//     lifetime; the agent pushes [Config] and [Updates] first, then [Config]
//     again on every reload and [Updates] whenever the update state changes.
//     Requests are answered in order, with a message of the request's own
//     type or an [Error].
//   - pty: binary frames (see frame.go) carrying one terminal session.
//   - forward: raw bytes spliced to a TCP port on the guest's loopback.
//   - udp: datagrams (see datagram.go) relayed to a UDP port on the guest's
//     loopback, one stream per client address on the phone.
//   - font: the configured font file's bytes, then EOF.
package protocol

// Version is bumped on any incompatible change. Additive changes (new
// optional fields, new message types, which receivers ignore when unknown)
// keep it. A side speaks every version from MinVersion to Version: the app's
// Hello sends its range, the agent answers with the highest version both
// speak (see [Negotiate]), so a newer app can still reach an older guest, say
// to update it.
const (
	Version    = 1
	MinVersion = 1
)

// Negotiate returns the version a peer speaking [min, max] (min 0: just max)
// and this side share, or false when there is none.
func Negotiate(min, max int) (int, bool) {
	if min == 0 {
		min = max
	}
	v := Version
	if max < v {
		v = max
	}
	return v, v >= min && v >= MinVersion
}

// ControlPort is the vsock port the agent listens on for every stream.
const ControlPort = 9100

// Stream kinds a Hello can open.
const (
	StreamControl = "control"
	StreamPTY     = "pty"
	StreamForward = "forward"
	// StreamUDP is a stream kind of its own, not a forward option, so an
	// agent without it refuses rather than splicing datagrams into TCP.
	StreamUDP  = "udp"
	StreamFont = "font"
)

// Message types on the control stream, and the Hello/Error replies.
const (
	TypeHello    = "hello"
	TypeError    = "error"
	TypeConfig   = "config"
	TypePing     = "ping"
	TypePong     = "pong"
	TypePoweroff = "poweroff"
	TypeReboot   = "reboot"

	// TypeUpdates is the agent's push of [Updates], and the app's request for
	// a fresh one (a desktop's `switch` changes generations under the app).
	TypeUpdates = "updates"
	// TypeUpdateCheck looks for a newer system now.
	TypeUpdateCheck = "update-check"
	// TypeUpdateApply fetches and switches to the newest system, checking
	// first when no check has found one.
	TypeUpdateApply = "update-apply"
	// TypeSwitchGeneration switches to [Request.Generation]: a rollback, or
	// forward again. It cancels a running check or download first.
	TypeSwitchGeneration = "switch-generation"
	// TypeUpdateCancel stops a running check or download.
	TypeUpdateCancel = "update-cancel"
	// TypeHistory asks for the user's shell history, answered with [History].
	TypeHistory = "history"
)

// Request is an app request on the control stream; fields past Type belong
// to particular types. Long operations are acknowledged once started; their
// progress and outcome arrive as [Updates].
type Request struct {
	Type       string `json:"type"`
	Generation int    `json:"generation,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Text       string `json:"text,omitempty"`
	// ID, when set, comes back in the answer's "id": requests are answered in
	// order, but a requester that gave up on one can tell its late answer apart.
	ID int `json:"id,omitempty"`
}

// Envelope is the part of every JSON message a receiver reads first.
type Envelope struct {
	Type string `json:"type"`
}

// Hello is the first line on every connection, sent by both sides. The app's
// names the stream and its parameters; the agent's echoes the version.
type Hello struct {
	Type    string `json:"type"` // always "hello"
	Version int    `json:"version"`
	// MinVersion is the oldest version the app speaks; 0 means just Version.
	// The agent's Version is the negotiated one.
	MinVersion int `json:"minVersion,omitempty"`
	// Agent or App is the sender's build version, for display and bug reports only.
	Agent string `json:"agent,omitempty"`
	App   string `json:"app,omitempty"`

	Stream string `json:"stream,omitempty"`
	// PTY: the profile to run (empty for the default one), and the initial size.
	Profile string `json:"profile,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Cols    int    `json:"cols,omitempty"`
	// Forward and udp: the guest port on 127.0.0.1.
	Port int `json:"port,omitempty"`

	// The byte count that follows: the agent's on a font or file read, the
	// app's on a share; on a file list, the entry count.
	Size int64 `json:"size,omitempty"`

	// Share and file streams (integrations.go).
	Op   string    `json:"op,omitempty"`
	Path string    `json:"path,omitempty"`
	Name string    `json:"name,omitempty"`
	Dir  bool      `json:"dir,omitempty"`
	File *FileInfo `json:"file,omitempty"`
	// Mode is a FileOpen's.
	Mode string `json:"mode,omitempty"`
}

// Error rejects a Hello, or reports a failed control request.
type Error struct {
	Type    string `json:"type"` // always "error"
	Message string `json:"message"`
	// ID is the failed request's.
	ID int `json:"id,omitempty"`
}

// Config is what the NixOS module declares for the app; the agent sends it on
// connect and after each reload. The module writes the same shape (minus Type)
// as the agent's config file.
type Config struct {
	Type     string    `json:"type,omitempty"` // "config" on the wire
	Hostname string    `json:"hostname,omitempty"`
	Forwards []Forward `json:"forwards"`
	Terminal Terminal  `json:"terminal"`
}

// Forward is one port the app exposes on the phone.
type Forward struct {
	Name      string `json:"name"`
	GuestPort int    `json:"guestPort"`
	HostPort  int    `json:"hostPort"`
	// Protocol is ProtocolTCP (also when empty) or ProtocolUDP.
	Protocol string `json:"protocol,omitempty"`
	// LAN binds 0.0.0.0 instead of 127.0.0.1.
	LAN bool `json:"lan"`
}

// Forward protocols.
const (
	ProtocolTCP = "tcp"
	ProtocolUDP = "udp"
)

// Terminal is the module's terminal UI configuration.
type Terminal struct {
	Profiles       []Profile    `json:"profiles"`
	DefaultProfile string       `json:"defaultProfile"`
	ExtraKeys      [][]ExtraKey `json:"extraKeys"`
	Theme          Theme        `json:"theme"`
	// Snippets are the command palette's module-defined entries.
	Snippets []Snippet `json:"snippets"`
}

// Snippet is one command-palette entry.
type Snippet struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	// Submit presses Enter after inserting Command.
	Submit bool `json:"submit,omitempty"`
}

// Profile is one kind of terminal tab.
type Profile struct {
	Name string `json:"name"`
	// Command runs through the user's login shell (`$SHELL -l -c`); empty runs
	// the login shell itself.
	Command string `json:"command,omitempty"`
}

// ExtraKey is one key in a row above the keyboard. Key and Popup use Termux's
// extra-keys names (ESC, TAB, CTRL, UP, PGUP, F1, …) or a literal character.
type ExtraKey struct {
	Key string `json:"key"`
	// Popup is sent on a swipe up from the key.
	Popup string `json:"popup,omitempty"`
	// Label replaces the key's name on the button.
	Label string `json:"label,omitempty"`
}

// Theme colors use Termux's color property names (foreground, background,
// cursor, color0–color15) and #rrggbb values.
type Theme struct {
	Colors map[string]string `json:"colors"`
	// Font is the guest path of a TTF/OTF file, served on a font stream. The
	// app caches it by FontSHA256.
	Font       string `json:"font,omitempty"`
	FontSHA256 string `json:"fontSha256,omitempty"`
}
