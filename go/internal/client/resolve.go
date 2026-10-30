package client

import (
	"encoding/json"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// ResolvePath returns the file-provider path (relative to files.root) of
// the absolute guest path, or the agent's refusal when it's outside the root.
func (c *Control) ResolvePath(path string) (string, error) {
	line, err := c.request(protocol.Request{Type: protocol.TypeResolvePath, Path: path})
	if err != nil {
		return "", err
	}
	var d protocol.Done
	err = json.Unmarshal(line, &d)
	return d.Path, err
}
