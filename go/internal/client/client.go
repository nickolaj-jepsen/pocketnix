// Package client speaks the app's side of the agent protocol. The Android
// app has its own implementation; this one serves the CLI's debug commands
// and the tests.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// Client opens streams to one agent.
type Client struct {
	Dial func() (net.Conn, error)
	App  string // sent in Hello
}

func (c *Client) open(h protocol.Hello) (net.Conn, protocol.Hello, error) {
	conn, err := c.Dial()
	if err != nil {
		return nil, protocol.Hello{}, err
	}
	h.Type = protocol.TypeHello
	h.Version = protocol.Version
	h.App = c.App
	if err := protocol.WriteJSON(conn, h); err != nil {
		conn.Close()
		return nil, protocol.Hello{}, err
	}
	reply, err := protocol.ReadHello(conn)
	if err != nil {
		conn.Close()
		return nil, protocol.Hello{}, err
	}
	if reply.Version != protocol.Version {
		conn.Close()
		return nil, protocol.Hello{}, fmt.Errorf("agent speaks protocol %d, want %d", reply.Version, protocol.Version)
	}
	return conn, reply, nil
}

// Control is an open control stream.
type Control struct {
	conn  net.Conn
	Agent string // the agent's build version

	configs chan protocol.Config
	replies chan reply
	wmu     sync.Mutex
}

type reply struct {
	typ string
	err error
}

// Control opens the control stream and returns once the first config arrived.
func (c *Client) Control() (*Control, error) {
	conn, hello, err := c.open(protocol.Hello{Stream: protocol.StreamControl})
	if err != nil {
		return nil, err
	}
	ctl := &Control{conn: conn, Agent: hello.Agent, configs: make(chan protocol.Config, 16), replies: make(chan reply, 4)}
	go ctl.read()
	return ctl, nil
}

func (c *Control) read() {
	defer close(c.configs)
	defer close(c.replies)
	for {
		line, err := protocol.ReadLine(c.conn)
		if err != nil {
			return
		}
		var env struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		switch env.Type {
		case protocol.TypeConfig:
			var cfg protocol.Config
			if json.Unmarshal(line, &cfg) == nil {
				c.configs <- cfg
			}
		case protocol.TypeError:
			c.replies <- reply{err: &protocol.RemoteError{Message: env.Message}}
		default:
			c.replies <- reply{typ: env.Type}
		}
	}
}

// Configs delivers the config on connect and after each agent reload. It's
// closed when the stream ends.
func (c *Control) Configs() <-chan protocol.Config { return c.configs }

func (c *Control) request(typ, want string) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := protocol.WriteJSON(c.conn, protocol.Envelope{Type: typ}); err != nil {
		return err
	}
	r, ok := <-c.replies
	if !ok {
		return io.ErrUnexpectedEOF
	}
	if r.err != nil {
		return r.err
	}
	if r.typ != want {
		return fmt.Errorf("sent %s, agent answered %s", typ, r.typ)
	}
	return nil
}

func (c *Control) Ping() error     { return c.request(protocol.TypePing, protocol.TypePong) }
func (c *Control) Poweroff() error { return c.request(protocol.TypePoweroff, protocol.TypePoweroff) }
func (c *Control) Reboot() error   { return c.request(protocol.TypeReboot, protocol.TypeReboot) }
func (c *Control) Close() error    { return c.conn.Close() }

// PTY is an open terminal session.
type PTY struct {
	conn net.Conn
	wmu  sync.Mutex

	pending []byte
	exited  bool
	status  int32
}

// PTY starts a session running profile ("" for the default) at the given size.
func (c *Client) PTY(profile string, rows, cols int) (*PTY, error) {
	conn, _, err := c.open(protocol.Hello{Stream: protocol.StreamPTY, Profile: profile, Rows: rows, Cols: cols})
	if err != nil {
		return nil, err
	}
	return &PTY{conn: conn}, nil
}

// Read returns session output. After the session ends it returns io.EOF and
// Exit reports the status.
func (p *PTY) Read(b []byte) (int, error) {
	for len(p.pending) == 0 {
		if p.exited {
			return 0, io.EOF
		}
		typ, payload, err := protocol.ReadFrame(p.conn)
		if err != nil {
			return 0, err
		}
		switch typ {
		case protocol.FrameData:
			p.pending = payload
		case protocol.FrameExit:
			p.status, err = protocol.DecodeExit(payload)
			if err != nil {
				return 0, err
			}
			p.exited = true
		}
	}
	n := copy(b, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}

// Exit reports the exit status once Read has returned io.EOF.
func (p *PTY) Exit() (int32, error) {
	if !p.exited {
		return 0, errors.New("session still running")
	}
	return p.status, nil
}

// Write sends keyboard input.
func (p *PTY) Write(b []byte) (int, error) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	for off := 0; off < len(b); off += protocol.MaxFramePayload {
		end := min(off+protocol.MaxFramePayload, len(b))
		if err := protocol.WriteFrame(p.conn, protocol.FrameData, b[off:end]); err != nil {
			return off, err
		}
	}
	return len(b), nil
}

// Resize changes the session's window size.
func (p *PTY) Resize(rows, cols int) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	return protocol.WriteFrame(p.conn, protocol.FrameResize, protocol.Winsize{Rows: uint16(rows), Cols: uint16(cols)}.Encode())
}

// Close hangs the session up.
func (p *PTY) Close() error { return p.conn.Close() }

// Forward opens a stream to TCP port on the guest's loopback.
func (c *Client) Forward(port int) (net.Conn, error) {
	conn, _, err := c.open(protocol.Hello{Stream: protocol.StreamForward, Port: port})
	return conn, err
}

// Font fetches the configured font file.
func (c *Client) Font() ([]byte, error) {
	conn, hello, err := c.open(protocol.Hello{Stream: protocol.StreamFont})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	b := make([]byte, hello.Size)
	if _, err := io.ReadFull(conn, b); err != nil {
		return nil, err
	}
	return b, nil
}
