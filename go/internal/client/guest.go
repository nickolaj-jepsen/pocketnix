package client

import (
	"encoding/json"
	"errors"
	"net"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// Guest sends one request to the agent's guest socket at path and returns
// the reply's text.
func Guest(path string, req protocol.GuestRequest) (string, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := protocol.WriteJSON(conn, req); err != nil {
		return "", err
	}
	line, err := protocol.ReadLine(conn)
	if err != nil {
		return "", err
	}
	var reply protocol.GuestReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return "", err
	}
	if reply.Error != "" {
		return "", errors.New(reply.Error)
	}
	return reply.Text, nil
}
