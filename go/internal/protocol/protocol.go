// Package protocol is the wire contract between the app and the guest agent.
//
// The app always dials (a guest can't dial an app over AVF vsock), every
// connection to the same agent port. Each connection opens with one JSON line
// from the app, a [Hello] naming the stream it wants, and the agent answers
// with its own [Hello] or an [Error] before the stream's own traffic starts:
//
//   - control: newline-delimited JSON messages both ways for the app's
//     lifetime; the agent pushes [Config], [Updates], [Wake] and, when port
//     offers are on, [PortListeners] first, then each again whenever it changes.
//     Requests are answered in order, with a message of the request's own
//     type or an [Error].
//   - pty: binary frames ([WriteFrame]) carrying one terminal session.
//   - forward: raw bytes spliced to a TCP port on the guest's loopback.
//   - udp: datagrams ([WriteDatagram]) relayed to a UDP port on the guest's
//     loopback, one stream per client address on the phone.
//   - font: the configured font file's bytes, then EOF.
//   - gui: the stream itself, handed to the guest compositor ([StreamGUI]).
//   - audio: the guest's sound as raw PCM ([StreamAudio]).
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
	// StreamAudio carries the guest's sound to the app after the Hello: raw
	// PCM, signed 16-bit little-endian, 48 kHz, 2 channels interleaved, as
	// it plays. Nothing comes while the guest is silent. A newer audio
	// stream ends the older one.
	StreamAudio = "audio"
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

	// TypeSelections writes a managed guest's [Selections] for its next
	// rebuild, and is answered with them.
	TypeSelections = "selections"
	// TypePackageAdd and TypePackageRemove change a managed guest's user
	// profile ([Request.Packages], nixpkgs attributes).
	TypePackageAdd    = "package-add"
	TypePackageRemove = "package-remove"
	// TypeRun fetches [Request.Package] to run it once; its outcome arrives
	// as [Updates.Run].
	TypeRun = "run"
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
	// Notify-action and notify-closed.
	Notification uint32 `json:"notification,omitempty"`
	Key          string `json:"key,omitempty"`
	Reason       uint32 `json:"reason,omitempty"`

	// Transfers: the Event's Ref a guest-done answers, and how it ended;
	// set-clipboard's MIME types.
	Ref       string   `json:"ref,omitempty"`
	Cancelled bool     `json:"cancelled,omitempty"`
	Error     string   `json:"error,omitempty"`
	Types     []string `json:"types,omitempty"`

	// Host is a host-state request's.
	Host *HostState `json:"host,omitempty"`

	// Folders is a host-folders request's.
	Folders []HostFolder `json:"folders,omitempty"`

	// Path is a resolve-path request's.
	Path string `json:"path,omitempty"`
	// Pin is an update-check's or update-apply's pocketnix release ("0.2.0")
	// for a managed guest's flake; empty keeps the one it has.
	Pin string `json:"pin,omitempty"`
	// AllowBuilds lets an update-apply or run build what its [Plan] lists.
	AllowBuilds bool        `json:"allowBuilds,omitempty"`
	Selections  *Selections `json:"selections,omitempty"`
	Packages    []string    `json:"packages,omitempty"`
	Package     string      `json:"package,omitempty"`
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
	// Command is a PTY's to run through the login shell instead of the
	// profile's; the app's way to start a fetched program in a tab.
	Command string `json:"command,omitempty"`
	// Tab is the agent's on a PTY: the session's id, unique across agent
	// restarts, which it sees as $POCKETNIX_TAB.
	Tab string `json:"tab,omitempty"`
	// Forward and udp: the guest port on 127.0.0.1.
	Port int `json:"port,omitempty"`

	// The byte count that follows: the agent's on a font or file read, the
	// app's on a share; on a file list, the entry count.
	Size int64 `json:"size,omitempty"`

	// Share and file streams.
	Op   string    `json:"op,omitempty"`
	Path string    `json:"path,omitempty"`
	Name string    `json:"name,omitempty"`
	Dir  bool      `json:"dir,omitempty"`
	File *FileInfo `json:"file,omitempty"`
	// Mode is a FileOpen's.
	Mode string `json:"mode,omitempty"`

	// Ref ties a share or save stream to the guest request an [Event] named.
	Ref string `json:"ref,omitempty"`
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
	// GUI is absent from an agent without GUI support.
	GUI *GUI `json:"gui,omitempty"`
	// Managed is present when the phone manages this guest: the app
	// configures it and it rebuilds itself.
	Managed *Managed `json:"managed,omitempty"`
}

// Managed is a managed guest's: Flake is its own flake's directory.
type Managed struct {
	Flake string `json:"flake"`
}

// Selections are what the app configures in a managed guest, its flake's
// pocketnix.json: the user, which [Services] are on, and how many
// generations to keep besides the running and the booted one.
type Selections struct {
	Type            string          `json:"type,omitempty"` // "selections" on the wire
	User            string          `json:"user"`
	Services        map[string]bool `json:"services"`
	KeepGenerations int             `json:"keepGenerations"`
	// Changed is the agent's answer: the request changed the file, so the
	// running system may not be what it describes.
	Changed bool `json:"changed,omitempty"`
}

// Services are the switches of [Selections], the options of the module's
// pocketnix.guest.managed.selections.services.
var Services = []string{"ssh", "tailscale", "podman", "gui"}

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
