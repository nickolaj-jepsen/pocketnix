package protocol

// Integrations (design.md, Integrations and UX): guest programs reach Android
// through the agent, and Android shares and files reach the guest.

// Agent → app pushes on the control stream, sent as an [Event].
const (
	// TypeNotify posts an Android notification (Title, Body).
	TypeNotify = "notify"
	// TypeOpen opens URL on Android, or Path: a file under the file provider's root.
	TypeOpen = "open"
	// TypeGuestClipboard sets Android's clipboard to Text.
	TypeGuestClipboard = "guest-clipboard"
)

// App → agent requests on the control stream, fields in [Request].
const (
	// TypeSetClipboard hands the agent Android's clipboard, which a guest
	// paste returns.
	TypeSetClipboard = "set-clipboard"
	// TypeShareText delivers shared text to the share directory or command.
	TypeShareText = "share-text"
)

// Event is an agent push for the app to act on.
type Event struct {
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	URL   string `json:"url,omitempty"`
	Path  string `json:"path,omitempty"`
	Text  string `json:"text,omitempty"`
}

// Streams for shared files and the file provider.
const (
	// StreamShare carries one shared file: Hello.Name and Hello.Size, then
	// Size bytes; the agent answers [Done] with where it went.
	StreamShare = "share"
	// StreamFile is one file-provider operation, Hello.Op on Hello.Path.
	StreamFile = "file"
)

// File-provider operations, in Hello.Op. Paths are slash-separated and
// relative to the provider's root; "" is the root itself.
const (
	// FileList answers Hello.Size entries, then that many [FileInfo] lines.
	FileList = "list"
	// FileStat answers Hello.File.
	FileStat = "stat"
	// FileRead answers Hello.Size, then the file's bytes.
	FileRead = "read"
	// FileWrite replaces the file with the bytes the app sends after the
	// agent's Hello, up to its half-close; the agent answers [Done].
	FileWrite = "write"
	// FileCreate makes an empty file (or Hello.Dir a directory) named
	// Hello.Name in the directory Path, renamed "name (n)" if taken; answers
	// Hello.File.
	FileCreate = "create"
	// FileDelete removes Path, recursively.
	FileDelete = "delete"
	// FileRename renames Path to Hello.Name in the same directory; answers Hello.File.
	FileRename = "rename"
)

// FileInfo is one file under the file provider's root.
type FileInfo struct {
	// Path is relative to the root; Name is its last element.
	Path string `json:"path"`
	Name string `json:"name"`
	Dir  bool   `json:"dir,omitempty"`
	Size int64  `json:"size"`
	// MTime is in Unix milliseconds, as Android's DocumentsContract wants.
	MTime    int64 `json:"mtime"`
	Writable bool  `json:"writable,omitempty"`
}

// TypeDone ends a share or file-write stream.
const TypeDone = "done"

// Done reports where a shared file or a write landed.
type Done struct {
	Type string `json:"type"` // "done"
	Path string `json:"path,omitempty"`
}

// Guest requests, on the agent's guest-local unix socket: one JSON line each
// way per request, as many as the connection likes. The agent answers with
// [GuestReply].
const (
	GuestNotify = "notify" // Title, Body
	GuestOpen   = "open"   // URL, or Path (absolute in the guest)
	GuestCopy   = "copy"   // Text
	GuestPaste  = "paste"  // answers Text
)

// GuestRequest is a guest program's request.
type GuestRequest struct {
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	URL   string `json:"url,omitempty"`
	Path  string `json:"path,omitempty"`
	Text  string `json:"text,omitempty"`
}

// GuestReply answers a GuestRequest: Error is set when it failed.
type GuestReply struct {
	Error string `json:"error,omitempty"`
	Text  string `json:"text,omitempty"`
}

// GuestSocket is where the module's agent serves guest requests.
const GuestSocket = "/run/pocketnix/guest.sock"
