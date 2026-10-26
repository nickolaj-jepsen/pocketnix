package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"syscall"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// Guest sends one request to the agent's guest socket at path and returns
// the reply's text.
func Guest(path string, req protocol.GuestRequest) (string, error) {
	reply, err := GuestCall(path, req)
	return reply.Text, err
}

// GuestCall sends one request to the agent's guest socket at path, passing
// files as descriptors, and returns the whole reply; a reply's Error is the
// error.
func GuestCall(path string, req protocol.GuestRequest, files ...*os.File) (protocol.GuestReply, error) {
	return GuestCallContext(context.Background(), path, req, files...)
}

// GuestCallContext is GuestCall that hangs up when ctx ends, which cancels
// a request waiting on the app.
func GuestCallContext(ctx context.Context, path string, req protocol.GuestRequest, files ...*os.File) (protocol.GuestReply, error) {
	var reply protocol.GuestReply
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return reply, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	line, err := json.Marshal(req)
	if err != nil {
		return reply, err
	}
	line = append(line, '\n')
	if len(files) == 0 {
		_, err = conn.Write(line)
	} else {
		fds := make([]int, len(files))
		for i, f := range files {
			fds[i] = int(f.Fd())
		}
		_, _, err = conn.(*net.UnixConn).WriteMsgUnix(line, syscall.UnixRights(fds...), nil)
	}
	if err != nil {
		return reply, err
	}
	b, err := protocol.ReadLine(conn)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return reply, err
	}
	if err := json.Unmarshal(b, &reply); err != nil {
		return reply, err
	}
	if reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
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
