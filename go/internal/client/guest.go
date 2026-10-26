package client

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// Guest sends one request to the agent's guest socket at path and returns
// the reply's text.
func Guest(path string, req protocol.GuestRequest) (string, error) {
	reply, err := GuestCall(path, req)
	return reply.Text, err
}

// GuestCall sends one request to the agent's guest socket at path and
// returns the whole reply; a reply's Error is returned as an error.
func GuestCall(path string, req protocol.GuestRequest) (protocol.GuestReply, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return protocol.GuestReply{}, err
	}
	defer conn.Close()
	return guestRoundTrip(conn, conn, req)
}

func guestRoundTrip(w io.Writer, r io.Reader, req protocol.GuestRequest) (protocol.GuestReply, error) {
	if err := protocol.WriteJSON(w, req); err != nil {
		return protocol.GuestReply{}, err
	}
	line, err := protocol.ReadLine(r)
	if err != nil {
		return protocol.GuestReply{}, err
	}
	var reply protocol.GuestReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return reply, err
	}
	if reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

// Subscription is a guest socket connection carrying notification actions
// and closes from the app.
type Subscription struct {
	conn net.Conn
	r    *bufio.Reader
}

// Subscribe opens a subscription on the agent's guest socket at path.
func Subscribe(path string) (*Subscription, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	s := &Subscription{conn: conn, r: bufio.NewReader(conn)}
	if _, err := guestRoundTrip(conn, s.r, protocol.GuestRequest{Type: protocol.GuestSubscribe}); err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

// Next waits for the next event; it fails once the connection ends.
func (s *Subscription) Next() (protocol.GuestEvent, error) {
	var ev protocol.GuestEvent
	line, err := protocol.ReadLine(s.r)
	if err != nil {
		return ev, err
	}
	err = json.Unmarshal(line, &ev)
	return ev, err
}

func (s *Subscription) Close() error { return s.conn.Close() }
