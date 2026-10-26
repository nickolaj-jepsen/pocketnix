package protocol

// Update sources, as listed in [Updates.Sources].
const (
	SourceBuildHost = "buildHost"
	SourcePublished = "published"
)

// Update states.
const (
	UpdateIdle      = "idle"
	UpdateChecking  = "checking"
	UpdateFetching  = "fetching"
	UpdateSwitching = "switching"
)

// Updates is the guest's update state: where updates come from, what the
// last check found, the operation running, and the system generations.
type Updates struct {
	Type string `json:"type"` // "updates" on the wire
	// Sources lists the configured update sources in the order a check tries
	// them; empty when the module configures none.
	Sources []string `json:"sources"`
	State   string   `json:"state"`
	// Op counts the operations started since the agent did, so a requester
	// can tell when its own has finished: State is idle with Op at least the
	// value it saw plus one.
	Op int `json:"op"`
	// Current and Booted are the running and the booted system's store paths.
	Current string `json:"current"`
	Booted  string `json:"booted"`
	// Available is a system other than Current that the last check found,
	// and Source the source it came from.
	Available string `json:"available,omitempty"`
	Source    string `json:"source,omitempty"`
	// CheckedAt is when the last check finished, in Unix seconds.
	CheckedAt int64 `json:"checkedAt,omitempty"`
	// Error is why the last operation failed; cleared when the next starts.
	Error string `json:"error,omitempty"`
	// Log holds the last lines of output of the running or the last operation.
	Log         []string     `json:"log"`
	Generations []Generation `json:"generations"`
}

// Generation is one entry of the system profile; [Updates] lists them newest first.
type Generation struct {
	Number int    `json:"number"`
	Path   string `json:"path"`
	// Date is when the generation was made, in Unix seconds.
	Date int64 `json:"date"`
	// Label is the system's nixos-version.
	Label   string `json:"label,omitempty"`
	Current bool   `json:"current,omitempty"`
	Booted  bool   `json:"booted,omitempty"`
}

// History answers a history request: the user's shell commands, most recent
// first, each once.
type History struct {
	Type    string   `json:"type"` // "history"
	Entries []string `json:"entries"`
}
