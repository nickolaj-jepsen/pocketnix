package protocol

// Transfers (docs/architecture.md, Integrations): guest requests the app carries
// out with Android's pickers, share sheet or clipboard, and the bytes that
// cross for them. The agent can't dial the app, so each is an [Event] with a
// Ref; the app answers on streams whose Hello carries that Ref, and ends the
// request with TypeGuestDone where no stream does.
//
// A guest request may attach descriptors (SCM_RIGHTS with its line): save and
// a typed copy read the first, a typed paste writes to it. The guest program
// opened it with its own rights, so these reach any file its user can.

// Guest requests for transfers.
const (
	// GuestShare offers Paths (absolute, under the file provider's root) or
	// Text to Android's share sheet. It doesn't wait for the user.
	GuestShare = "share"
	// GuestPick asks the user for files on Android (Types: MIME types such as
	// image/*, Multiple). They land in the share directory, and the answer's
	// Paths name them; Cancelled when the user backed out.
	GuestPick = "pick"
	// GuestSave stores the attached file on Android where the user picks, as
	// Name (MIME); answered once stored, or Cancelled. With nothing attached
	// it is answered (Ref) once the user picked, and GuestSaveData brings
	// the bytes.
	GuestSave = "save"
	// GuestSaveData supplies the attached file to the deferred save Ref;
	// answered once stored.
	GuestSaveData = "save-data"
	// GuestClipboardTypes answers the clipboard's MIME types in Types.
	GuestClipboardTypes = "clipboard-types"
)

// A GuestCopy or GuestPaste with a MIME other than text/plain moves bytes
// through the attached descriptor instead of Text: a copy hands Android its
// contents, a paste fetches the clipboard's data of that type into it.

// Agent → app pushes ([Event]).
const (
	// TypeShare offers Paths (relative to the file provider's root) or Text
	// through Android's share sheet.
	TypeShare = "share"
	// TypePick shows Android's document picker (Types, Multiple). The app
	// sends each picked file on a share stream with Ref, then TypeGuestDone.
	TypePick = "pick"
	// TypeSave asks where to store Name (MIME, Size; 0 when deferred) on
	// Android. The app fetches it on a save stream with Ref, or answers
	// TypeGuestDone Cancelled.
	TypeSave = "save"
	// TypeGuestClipboardData puts a guest copy of MIME (Size bytes) on
	// Android's clipboard; the app fetches it on a save stream with Ref.
	// Older apps ignore it, where a guest-clipboard would clear the clipboard.
	TypeGuestClipboardData = "guest-clipboard-data"
	// TypeClipboardRequest asks for the clipboard's data of MIME, which the
	// app sends on a share stream with Ref, or answers TypeGuestDone with Error.
	TypeClipboardRequest = "clipboard-request"
)

// TypeGuestDone is the app's request ending the transfer Ref:
// Request.Cancelled when the user backed out, Request.Error when it failed,
// otherwise done (a pick, after its files).
const TypeGuestDone = "guest-done"

// StreamSave carries a transfer's bytes to the app: Hello.Ref names it, and
// the agent's Hello (Name, Size) comes once the bytes are there, then the
// bytes; the app answers [Done] once it stored them, or an [Error]. A share
// stream with Hello.Ref carries bytes the other way, answering a pick or a
// clipboard request instead of being a share.
const StreamSave = "save"
