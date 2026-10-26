package client

import "github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"

// NotifyAction reports that the user chose action key on notification id.
func (c *Control) NotifyAction(id uint32, key string) error {
	_, err := c.request(protocol.Request{Type: protocol.TypeNotifyAction, Notification: id, Key: key})
	return err
}

// NotifyClosed reports that notification id went away (a protocol.Closed* reason).
func (c *Control) NotifyClosed(id, reason uint32) error {
	_, err := c.request(protocol.Request{Type: protocol.TypeNotifyClosed, Notification: id, Reason: reason})
	return err
}
