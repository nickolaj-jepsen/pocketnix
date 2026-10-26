package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"

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
		sum, err := fileSHA256(t.Theme.Font)
		if err != nil {
			return fmt.Errorf("font: %w", err)
		}
		t.Theme.FontSHA256 = sum
	}
	return nil
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

func fileSHA256(path string) (string, error) {
	fh, err := os.Open(path)
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
