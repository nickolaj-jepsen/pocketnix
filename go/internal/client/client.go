// Package client speaks the app's side of the agent protocol. The Android
// app has its own implementation; this one serves the CLI's debug commands
// and the tests.
package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

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
	h.MinVersion = protocol.MinVersion
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
	if reply.Version < protocol.MinVersion || reply.Version > protocol.Version {
		conn.Close()
		return nil, protocol.Hello{}, fmt.Errorf("agent chose protocol %d, this client speaks %d–%d", reply.Version, protocol.MinVersion, protocol.Version)
	}
	return conn, reply, nil
}

// Control is an open control stream.
type Control struct {
	conn  net.Conn
	Agent string // the agent's build version

	configs chan protocol.Config
	events  chan protocol.Event
	ended   chan struct{}
	wmu     sync.Mutex

	// The one request awaiting its answer (requests go one at a time).
	pmu     sync.Mutex
	nextID  int
	pending *call

	umu     sync.Mutex
	updates protocol.Updates
	uchange chan struct{}
}

type call struct {
	id   int
	want string
	done chan reply
}

type reply struct {
	typ  string
	line []byte
	err  error
}

// replyTimeout bounds the wait for an answer; the agent answers at once,
// long operations included (their outcome arrives as Updates).
var replyTimeout = 30 * time.Second

// Control opens the control stream.
func (c *Client) Control() (*Control, error) {
	conn, hello, err := c.open(protocol.Hello{Stream: protocol.StreamControl})
	if err != nil {
		return nil, err
	}
	ctl := &Control{
		conn: conn, Agent: hello.Agent,
		configs: make(chan protocol.Config, 16), uchange: make(chan struct{}),
		events: make(chan protocol.Event, 64), ended: make(chan struct{}),
	}
	go ctl.read()
	return ctl, nil
}

func (c *Control) read() {
	defer close(c.ended)
	defer close(c.configs)
	defer close(c.events)
	defer func() {
		c.umu.Lock()
		close(c.uchange)
		c.umu.Unlock()
	}()
	// Only JSON lines follow the Hello, so the reader may read ahead.
	r := bufio.NewReader(c.conn)
	for {
		line, err := protocol.ReadLine(r)
		if err != nil {
			return
		}
		var env struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			ID      int    `json:"id"`
		}
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		switch env.Type {
		case protocol.TypeConfig:
			var cfg protocol.Config
			if json.Unmarshal(line, &cfg) == nil {
				// Only the latest config matters: a reader that fell behind
				// loses an older one rather than stalling the stream.
				select {
				case c.configs <- cfg:
				default:
					select {
					case <-c.configs:
					default:
					}
					c.configs <- cfg
				}
			}
		case protocol.TypeUpdates:
			// Pushed, and the answer to an updates request: both are the latest state.
			var u protocol.Updates
			if json.Unmarshal(line, &u) == nil {
				c.umu.Lock()
				c.updates = u
				close(c.uchange)
				c.uchange = make(chan struct{})
				c.umu.Unlock()
			}
		case protocol.TypeNotify, protocol.TypeOpen, protocol.TypeGuestClipboard:
			var ev protocol.Event
			if json.Unmarshal(line, &ev) == nil {
				select {
				case c.events <- ev:
				default: // nobody reads them
				}
			}
		case protocol.TypeError:
			c.answer(env.ID, env.Type, reply{err: &protocol.RemoteError{Message: env.Message}})
		default:
			c.answer(env.ID, env.Type, reply{typ: env.Type, line: line})
		}
	}
}

// answer hands r to the pending request it answers. An answer names its
// request's id; one without (an older agent) answers by its type, and
// anything else is a push this client doesn't know, which it ignores.
func (c *Control) answer(id int, typ string, r reply) {
	c.pmu.Lock()
	p := c.pending
	if p == nil || (id != 0 && id != p.id) || (id == 0 && typ != protocol.TypeError && typ != p.want) {
		c.pmu.Unlock()
		return
	}
	c.pending = nil
	c.pmu.Unlock()
	p.done <- r
}

// Configs delivers the config on connect and after each agent reload. It's
// closed when the stream ends.
func (c *Control) Configs() <-chan protocol.Config { return c.configs }

// Updates returns the latest update state (zero before the first) and a
// channel closed when it changes or the stream ends.
func (c *Control) Updates() (protocol.Updates, <-chan struct{}) {
	c.umu.Lock()
	defer c.umu.Unlock()
	return c.updates, c.uchange
}

// WaitUpdates returns the first update state for which done is true.
func (c *Control) WaitUpdates(done func(protocol.Updates) bool) (protocol.Updates, error) {
	for {
		u, changed := c.Updates()
		if u.Type != "" && done(u) {
			return u, nil
		}
		<-changed
		// A change replaces the channel; the stream's end only closes it.
		c.umu.Lock()
		ended := c.uchange == changed
		c.umu.Unlock()
		if ended {
			return u, io.ErrUnexpectedEOF
		}
	}
}

// Run starts an update operation with start (UpdateCheck, UpdateApply, …)
// and waits for it to finish, returning the final state; the operation's
// failure is an error.
func (c *Control) Run(start func() error) (protocol.Updates, error) {
	before, err := c.WaitUpdates(func(protocol.Updates) bool { return true })
	if err != nil {
		return before, err
	}
	if err := start(); err != nil {
		return before, err
	}
	u, err := c.WaitUpdates(func(u protocol.Updates) bool {
		return u.Op > before.Op && u.State == protocol.UpdateIdle
	})
	if err == nil && u.Error != "" {
		err = &protocol.RemoteError{Message: u.Error}
	}
	return u, err
}

func (c *Control) request(req protocol.Request) ([]byte, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	want := req.Type
	if want == protocol.TypePing {
		want = protocol.TypePong
	}
	c.pmu.Lock()
	c.nextID++
	req.ID = c.nextID
	p := &call{id: req.ID, want: want, done: make(chan reply, 1)}
	if req.Type != protocol.TypeUpdates {
		// An updates request's answer arrives as an Updates, outside the replies.
		c.pending = p
	}
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		if c.pending == p {
			c.pending = nil
		}
		c.pmu.Unlock()
	}()
	if err := protocol.WriteJSON(c.conn, req); err != nil {
		return nil, err
	}
	if req.Type == protocol.TypeUpdates {
		return nil, nil
	}
	var r reply
	select {
	case r = <-p.done:
	case <-c.ended:
		return nil, io.ErrUnexpectedEOF
	case <-time.After(replyTimeout):
		return nil, fmt.Errorf("no answer to %s within %v", req.Type, replyTimeout)
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.typ != want {
		return nil, fmt.Errorf("sent %s, agent answered %s", req.Type, r.typ)
	}
	return r.line, nil
}

func (c *Control) simple(typ string) error {
	_, err := c.request(protocol.Request{Type: typ})
	return err
}

func (c *Control) Ping() error        { return c.simple(protocol.TypePing) }
func (c *Control) Poweroff() error    { return c.simple(protocol.TypePoweroff) }
func (c *Control) Reboot() error      { return c.simple(protocol.TypeReboot) }
func (c *Control) UpdateCheck() error { return c.simple(protocol.TypeUpdateCheck) }
func (c *Control) UpdateApply() error { return c.simple(protocol.TypeUpdateApply) }

// UpdateCancel stops the running check or download.
func (c *Control) UpdateCancel() error { return c.simple(protocol.TypeUpdateCancel) }

// RefreshUpdates asks for a fresh update state; it arrives through Updates.
func (c *Control) RefreshUpdates() error { return c.simple(protocol.TypeUpdates) }

func (c *Control) SwitchGeneration(n int) error {
	_, err := c.request(protocol.Request{Type: protocol.TypeSwitchGeneration, Generation: n})
	return err
}

// History returns up to limit shell history entries (0 for the agent's default).
func (c *Control) History(limit int) ([]string, error) {
	line, err := c.request(protocol.Request{Type: protocol.TypeHistory, Limit: limit})
	if err != nil {
		return nil, err
	}
	var h protocol.History
	if err := json.Unmarshal(line, &h); err != nil {
		return nil, err
	}
	return h.Entries, nil
}

// Events delivers the agent's notify, open and guest-clipboard pushes (the
// last 64 unread); closed when the stream ends.
func (c *Control) Events() <-chan protocol.Event { return c.events }

// SetClipboard hands the agent Android's clipboard.
func (c *Control) SetClipboard(text string) error {
	_, err := c.request(protocol.Request{Type: protocol.TypeSetClipboard, Text: text})
	return err
}

// ShareText delivers text as the share sheet does, returning the file it landed in.
func (c *Control) ShareText(text string) (string, error) {
	line, err := c.request(protocol.Request{Type: protocol.TypeShareText, Text: text})
	if err != nil {
		return "", err
	}
	var d protocol.Done
	err = json.Unmarshal(line, &d)
	return d.Path, err
}

func (c *Control) Close() error { return c.conn.Close() }

// File runs a file-provider operation that has no data: stat, create,
// delete, rename. It returns the agent's Hello, whose File describes the result.
func (c *Client) File(h protocol.Hello) (protocol.Hello, error) {
	h.Stream = protocol.StreamFile
	conn, reply, err := c.open(h)
	if err != nil {
		return reply, err
	}
	conn.Close()
	return reply, nil
}

// List lists a directory under the file provider's root.
func (c *Client) List(path string) ([]protocol.FileInfo, error) {
	conn, reply, err := c.open(protocol.Hello{Stream: protocol.StreamFile, Op: protocol.FileList, Path: path})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	out := make([]protocol.FileInfo, 0, reply.Size)
	for range reply.Size {
		line, err := protocol.ReadLine(conn)
		if err != nil {
			return nil, err
		}
		var fi protocol.FileInfo
		if err := json.Unmarshal(line, &fi); err != nil {
			return nil, err
		}
		out = append(out, fi)
	}
	return out, nil
}

// ReadFile reads a file under the file provider's root.
func (c *Client) ReadFile(path string) ([]byte, error) {
	conn, reply, err := c.open(protocol.Hello{Stream: protocol.StreamFile, Op: protocol.FileRead, Path: path})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	b := make([]byte, reply.Size)
	_, err = io.ReadFull(conn, b)
	return b, err
}

// WriteFile replaces a file under the file provider's root with r's bytes.
func (c *Client) WriteFile(path string, data []byte) error {
	conn, _, err := c.open(protocol.Hello{Stream: protocol.StreamFile, Op: protocol.FileWrite, Path: path, Size: int64(len(data))})
	if err != nil {
		return err
	}
	defer conn.Close()
	return sendAndAwaitDone(conn, bytes.NewReader(data), nil)
}

// Share sends a file as the share sheet does, returning where it landed.
func (c *Client) Share(name string, data []byte) (string, error) {
	conn, _, err := c.open(protocol.Hello{Stream: protocol.StreamShare, Name: name, Size: int64(len(data))})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	var d protocol.Done
	err = sendAndAwaitDone(conn, bytes.NewReader(data), &d)
	return d.Path, err
}

// sendAndAwaitDone sends r, half-closes, and reads the agent's Done or Error.
func sendAndAwaitDone(conn net.Conn, r io.Reader, done *protocol.Done) error {
	if _, err := io.Copy(conn, r); err != nil {
		return err
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	line, err := protocol.ReadLine(conn)
	if err != nil {
		return err
	}
	var env struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return err
	}
	if env.Type == protocol.TypeError {
		return &protocol.RemoteError{Message: env.Message}
	}
	if done != nil {
		*done = protocol.Done{Type: env.Type, Path: env.Path}
	}
	return nil
}

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
