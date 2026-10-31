package client

import (
	"encoding/json"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// UpdateCheckPin checks with a managed guest's flake pinned to release pin.
func (c *Control) UpdateCheckPin(pin string) error {
	_, err := c.request(protocol.Request{Type: protocol.TypeUpdateCheck, Pin: pin})
	return err
}

// UpdateApplyWith applies, pinning a managed guest's flake to release pin
// and building what its plan lists when allowBuilds.
func (c *Control) UpdateApplyWith(pin string, allowBuilds bool) error {
	_, err := c.request(protocol.Request{Type: protocol.TypeUpdateApply, Pin: pin, AllowBuilds: allowBuilds})
	return err
}

// Selections writes a managed guest's selections when s is set, and returns
// what its flake holds.
func (c *Control) Selections(s *protocol.Selections) (protocol.Selections, error) {
	var got protocol.Selections
	line, err := c.request(protocol.Request{Type: protocol.TypeSelections, Selections: s})
	if err != nil {
		return got, err
	}
	err = json.Unmarshal(line, &got)
	return got, err
}

func (c *Control) PackageAdd(attrs []string, allowBuilds bool) error {
	_, err := c.request(protocol.Request{Type: protocol.TypePackageAdd, Packages: attrs, AllowBuilds: allowBuilds})
	return err
}

func (c *Control) PackageRemove(attrs []string) error {
	_, err := c.request(protocol.Request{Type: protocol.TypePackageRemove, Packages: attrs})
	return err
}

// RunPackage fetches a nixpkgs attribute to run; the outcome arrives as Updates.Run.
func (c *Control) RunPackage(attr string, allowBuilds bool) error {
	_, err := c.request(protocol.Request{Type: protocol.TypeRun, Package: attr, AllowBuilds: allowBuilds})
	return err
}
