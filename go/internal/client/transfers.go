package client

import (
	"bytes"
	"errors"
	"io"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// GuestDone ends the transfer ref: cancelled, failed with message, or done.
func (c *Control) GuestDone(ref string, cancelled bool, message string) error {
	_, err := c.request(protocol.Request{Type: protocol.TypeGuestDone, Ref: ref, Cancelled: cancelled, Error: message})
	return err
}

// SetClipboardTypes hands the agent Android's clipboard: its text and MIME types.
func (c *Control) SetClipboardTypes(text string, types []string) error {
	_, err := c.request(protocol.Request{Type: protocol.TypeSetClipboard, Text: text, Types: types})
	return err
}

// ShareFor sends data named name for the transfer ref (a picked file, a
// paste's data), returning where a picked file landed.
func (c *Client) ShareFor(ref, name string, data []byte) (string, error) {
	conn, _, err := c.open(protocol.Hello{Stream: protocol.StreamShare, Ref: ref, Name: name, Size: int64(len(data))})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	var d protocol.Done
	err = sendAndAwaitDone(conn, bytes.NewReader(data), &d)
	return d.Path, err
}

// Fetch takes the bytes of the transfer ref (a save, a typed copy), waiting
// until the guest has them, and passes them to store; the agent learns
// store's outcome, which Fetch returns.
func (c *Client) Fetch(ref string, store func(name string, r io.Reader) error) error {
	conn, hello, err := c.open(protocol.Hello{Stream: protocol.StreamSave, Ref: ref})
	if err != nil {
		return err
	}
	defer conn.Close()
	r := &io.LimitedReader{R: conn, N: hello.Size}
	err = store(hello.Name, r)
	if err == nil && r.N > 0 {
		err = errors.New("the stream ended early, or store left bytes unread")
	}
	if err != nil {
		protocol.WriteJSON(conn, protocol.Error{Type: protocol.TypeError, Message: err.Error()})
		return err
	}
	return protocol.WriteJSON(conn, protocol.Done{Type: protocol.TypeDone})
}
