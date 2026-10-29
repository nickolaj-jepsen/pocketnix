package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// File is the agent's config file, written by the NixOS module: the
// app-facing protocol.Config plus settings only the agent reads.
type File struct {
	protocol.Config
	// User runs PTY sessions; empty means the agent's own user.
	User string `json:"user,omitempty"`
	// Shell overrides the user's login shell from the passwd database.
	Shell string `json:"shell,omitempty"`
	// History overrides the shell history file the command palette reads;
	// empty picks the login shell's default.
	History string        `json:"history,omitempty"`
	Updates UpdatesConfig `json:"updates"`
	// Share is where Android's share sheet delivers to.
	Share ShareConfig `json:"share"`
	// FilesRoot is the directory the app's file provider exposes; empty is
	// the session user's home.
	FilesRoot string `json:"filesRoot,omitempty"`

	Power PowerConfig `json:"power"`
	Host  HostConfig  `json:"host"`
	// HostFolders is where Android folders the app shares are mounted.
	HostFolders HostFoldersConfig `json:"hostFolders"`
	// GUISocket is the compositor's GUI socket: absolute, or relative to the
	// session user's runtime directory; empty is "pocketnix-gui.sock".
	GUISocket string `json:"guiSocket,omitempty"`
}

// PowerConfig is how the guest keeps the phone awake.
type PowerConfig struct {
	WakeLocks WakeLocksConfig `json:"wakeLocks"`
}

// WakeLocksConfig turns the guest's logind sleep and idle inhibitors into
// the app's wake lock, except those whose who is in Ignore.
type WakeLocksConfig struct {
	Enable bool     `json:"enable"`
	Ignore []string `json:"ignore,omitempty"`
}

// HostConfig is what the guest takes from the phone's state.
type HostConfig struct {
	// TimeZone sets the guest's timezone to the phone's.
	TimeZone bool `json:"timeZone,omitempty"`
}

// HostFoldersConfig places the app's shared folders: each at
// <MountPoint>/<name>, MountPoint absolute or relative to the session
// user's home; empty is "Android".
type HostFoldersConfig struct {
	MountPoint string `json:"mountPoint,omitempty"`
}

// ShareConfig delivers shared text and files: into Directory, then (when
// set) running Command as the session user with the delivered file as its
// argument.
type ShareConfig struct {
	// Directory is absolute, or relative to the session user's home; empty is "Inbox".
	Directory string `json:"directory,omitempty"`
	Command   string `json:"command,omitempty"`
}

// UpdatesConfig names where updated systems come from (docs/updates.md). A check tries BuildHost, then Published.
type UpdatesConfig struct {
	BuildHost *BuildHost `json:"buildHost,omitempty"`
	Published *Published `json:"published,omitempty"`
	// CheckInterval is a Go duration between background checks; empty or
	// zero checks only when the app asks.
	CheckInterval string `json:"checkInterval,omitempty"`

	interval time.Duration
	// hostname names the toplevel a source may answer (nixos-system-<hostname>-…).
	hostname string
}

// BuildHost evaluates and builds the guest's system over ssh.
type BuildHost struct {
	// Host is the ssh destination ([user@]host); ssh_config supplies the rest.
	Host string `json:"host"`
	// Installable is what the build host builds: the toplevel of the guest's
	// nixosConfiguration.
	Installable string `json:"installable"`
	// SSHKey is the identity file ssh uses; empty leaves it to ssh.
	SSHKey string `json:"sshKey,omitempty"`
}

// Published is a URL whose body is a toplevel store path, which the guest
// substitutes from its binary caches.
type Published struct {
	URL string `json:"url"`
}

func (u *UpdatesConfig) sources() []string {
	var s []string
	if u.BuildHost != nil {
		s = append(s, protocol.SourceBuildHost)
	}
	if u.Published != nil {
		s = append(s, protocol.SourcePublished)
	}
	return s
}

func (u *UpdatesConfig) normalize() error {
	if b := u.BuildHost; b != nil && (b.Host == "" || b.Installable == "") {
		return errors.New("updates.buildHost needs host and installable")
	}
	if p := u.Published; p != nil {
		// Whoever can change the answer picks the system the guest switches to.
		if !strings.HasPrefix(p.URL, "https://") {
			return fmt.Errorf("updates.published.url %q is not an https URL", p.URL)
		}
	}
	u.interval = 0
	if u.CheckInterval != "" {
		d, err := time.ParseDuration(u.CheckInterval)
		if err != nil || d < 0 {
			return fmt.Errorf("updates.checkInterval %q: want a duration such as 6h", u.CheckInterval)
		}
		u.interval = d
	}
	return nil
}

// LoadFile reads and validates a config file, filling Theme.FontSHA256 from
// the font file's contents.
func LoadFile(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := f.normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

func (f *File) normalize() error {
	f.Type = ""
	if f.Forwards == nil {
		f.Forwards = []protocol.Forward{}
	}
	for i, fw := range f.Forwards {
		if fw.GuestPort < 1 || fw.GuestPort > 65535 {
			return fmt.Errorf("forward %q: guest port %d out of range", fw.Name, fw.GuestPort)
		}
		if fw.HostPort == 0 {
			f.Forwards[i].HostPort = fw.GuestPort
		} else if fw.HostPort < 1 || fw.HostPort > 65535 {
			return fmt.Errorf("forward %q: host port %d out of range", fw.Name, fw.HostPort)
		}
		switch fw.Protocol {
		case "":
			f.Forwards[i].Protocol = protocol.ProtocolTCP
		case protocol.ProtocolTCP, protocol.ProtocolUDP:
		default:
			return fmt.Errorf("forward %q: protocol %q is neither tcp nor udp", fw.Name, fw.Protocol)
		}
	}
	t := &f.Terminal
	if len(t.Profiles) == 0 {
		t.Profiles = []protocol.Profile{{Name: "shell"}}
	}
	if t.DefaultProfile == "" {
		t.DefaultProfile = t.Profiles[0].Name
	}
	if !slices.ContainsFunc(t.Profiles, func(p protocol.Profile) bool { return p.Name == t.DefaultProfile }) {
		return fmt.Errorf("default profile %q is not a profile", t.DefaultProfile)
	}
	if t.ExtraKeys == nil {
		t.ExtraKeys = [][]protocol.ExtraKey{}
	}
	if t.Theme.Colors == nil {
		t.Theme.Colors = map[string]string{}
	}
	t.Theme.FontSHA256 = ""
	if t.Theme.Font != "" {
		sum, err := fileSHA256(userHome(f.User), t.Theme.Font)
		if err != nil {
			return fmt.Errorf("font: %w", err)
		}
		t.Theme.FontSHA256 = sum
	}
	if t.Snippets == nil {
		t.Snippets = []protocol.Snippet{}
	}
	if err := checkGUI(f.GUI); err != nil {
		return fmt.Errorf("gui: %w", err)
	}
	f.Updates.hostname = f.Hostname
	return f.Updates.normalize()
}

func (f *File) profile(name string) (protocol.Profile, bool) {
	if name == "" {
		name = f.Terminal.DefaultProfile
	}
	i := slices.IndexFunc(f.Terminal.Profiles, func(p protocol.Profile) bool { return p.Name == name })
	if i < 0 {
		return protocol.Profile{}, false
	}
	return f.Terminal.Profiles[i], true
}

func fileSHA256(home, path string) (string, error) {
	fh, err := configuredFile(home, path)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func checkGUI(g *protocol.GUI) error {
	if g == nil {
		return nil
	}
	check := func(what string, a protocol.AppSettings) error {
		switch a.Size {
		case "", protocol.SizePhone, protocol.SizeCompact, protocol.SizeDesktop:
		default:
			return fmt.Errorf("%s: unknown size %q", what, a.Size)
		}
		switch a.Input {
		case "", protocol.InputTouch, protocol.InputPointer, protocol.InputTrackpad:
		default:
			return fmt.Errorf("%s: unknown input %q", what, a.Input)
		}
		return nil
	}
	if err := check("defaults", g.Defaults); err != nil {
		return err
	}
	for id, a := range g.AppDefaults {
		if err := check("appDefaults."+id, a); err != nil {
			return err
		}
	}
	return nil
}
