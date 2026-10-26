// Package protocol is the wire contract between the app and the guest agent.
//
// The app always dials (a guest can't dial an app over AVF vsock). The
// control stream carries newline-delimited JSON messages in both directions;
// PTY sessions, port forwards and file operations each get their own stream.
package protocol

// Version is bumped on any incompatible change. The app and the agent exchange
// it in Hello and refuse to talk across a major mismatch.
const Version = 1

// ControlPort is the vsock port the agent's control stream listens on.
const ControlPort = 9100

// Hello is the first message on the control stream, sent by both sides.
type Hello struct {
	Type    string `json:"type"` // always "hello"
	Version int    `json:"version"`
	// Agent is the sender's build version, for display and bug reports only.
	Agent string `json:"agent,omitempty"`
}
