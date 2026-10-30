package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// The clipboard bridge: Wayland's selection (through wl-clipboard, over the
// compositor's data-control) and the agent's clipboard, which is Android's.
// One MIME type crosses per change, since wl-copy offers one.

const (
	// hookArg runs the process as wl-paste --watch's command, which tells
	// the bridge of a change on hookFD.
	hookArg = "--watch-hook"
	// hookFD is the bridge's pipe in the hook: its ExtraFiles[0], which
	// wl-paste passes on.
	hookFD = 3
	// sensitiveHint is the type a password manager's copy offers besides its data.
	sensitiveHint = "x-kde-passwordManagerHint"
	// maxResubscribe caps the wait between attempts to reach the agent.
	maxResubscribe = 30 * time.Second
)

var errTooLarge = errors.New("too large to cross")

// clipboardBridge runs the bridge until wl-paste ends (the compositor went away).
func clipboardBridge(args []string) error {
	if len(args) == 1 && args[0] == hookArg {
		return watchHook()
	}
	if len(args) != 0 {
		return errUsage
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &bridge{sock: socket(), wl: wlClipboard{}, logf: log.Printf}
	changes, events := newSlot[string](), newSlot[protocol.ClipboardEvent]()
	go followClipboard(ctx, b.sock, events, b.logf)
	go b.run(ctx, changes, events)
	return watchWayland(changes)
}

// watchHook drains the selection wl-paste hands it, which the bridge reads
// again in the type it picks, and passes CLIPBOARD_STATE on.
func watchHook() error {
	io.Copy(io.Discard, os.Stdin)
	_, err := fmt.Fprintln(os.NewFile(hookFD, "bridge"), os.Getenv("CLIPBOARD_STATE"))
	return err
}

// watchWayland puts each selection change's state (data, sensitive or nil)
// in changes, until wl-paste ends.
func watchWayland(changes slot[string]) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	cmd := exec.Command("wl-paste", "--watch", self, "clipboard", hookArg)
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{w}
	err = cmd.Start()
	w.Close()
	if err != nil {
		return err
	}
	lines := bufio.NewScanner(r)
	for lines.Scan() {
		changes.put(lines.Text())
	}
	return fmt.Errorf("wl-paste ended: %w", cmd.Wait())
}

// followClipboard puts the agent's clipboard events in events, subscribing
// again whenever the agent goes away (it restarts with the guest).
func followClipboard(ctx context.Context, sock string, events slot[protocol.ClipboardEvent], logf func(string, ...any)) {
	wait := time.Second
	for ctx.Err() == nil {
		sub, err := client.SubscribeClipboard(sock)
		if err != nil {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
			}
			wait = min(wait*2, maxResubscribe)
			continue
		}
		wait = time.Second
		stop := context.AfterFunc(ctx, func() { sub.Close() })
		for {
			ev, err := sub.Next()
			if err != nil {
				break
			}
			events.put(ev)
		}
		stop()
		sub.Close()
		if ctx.Err() == nil {
			logf("clipboard: the agent went away; subscribing again")
		}
	}
}

// slot holds the latest value put in it, for one reader: a burst of changes
// leaves only the last one to act on.
type slot[T any] struct{ ch chan T }

func newSlot[T any]() slot[T] { return slot[T]{make(chan T, 1)} }

// put replaces any unread value; it takes one writer.
func (s slot[T]) put(v T) {
	for {
		select {
		case s.ch <- v:
			return
		default:
		}
		select {
		case <-s.ch:
		default:
		}
	}
}

// wayland is the Wayland selection.
type wayland interface {
	types() ([]string, error)
	// read returns the selection's data of type t, failing with errTooLarge
	// past limit bytes.
	read(t string, limit int64) ([]byte, error)
	// offer takes the selection with data, of type t only.
	offer(t string, data []byte) error
}

// bridge carries changes between the Wayland selection and the agent's
// clipboard. Each change it makes on one side comes back from it, so it
// drops a change equal to what the Wayland selection holds.
type bridge struct {
	sock string
	wl   wayland
	logf func(string, ...any)
	// held identifies what the Wayland selection holds as far as the bridge
	// knows (see clipKey); "" when it doesn't.
	held string
}

// run acts on the latest change from either side, one at a time, until ctx ends.
func (b *bridge) run(ctx context.Context, changes slot[string], events slot[protocol.ClipboardEvent]) {
	for {
		var err error
		select {
		case state := <-changes.ch:
			err = b.fromWayland(state)
		case ev := <-events.ch:
			err = b.fromAgent(ev)
		case <-ctx.Done():
			return
		}
		if err != nil {
			b.logf("clipboard: %v", err)
		}
	}
}

// fromWayland hands the agent the Wayland selection after a change in state
// (wl-paste's CLIPBOARD_STATE). An emptied selection leaves Android's alone.
func (b *bridge) fromWayland(state string) error {
	if state == "nil" {
		b.held = ""
		return nil
	}
	types, err := b.wl.types()
	if err != nil {
		return err
	}
	t := pickType(types)
	if t == "" {
		return nil
	}
	limit := int64(protocol.MaxClipboardData)
	if isText(t) {
		limit = protocol.MaxClipboardText
	}
	data, err := b.wl.read(t, limit)
	if err != nil {
		b.held = ""
		return fmt.Errorf("the Wayland clipboard's %s: %w", t, err)
	}
	if len(data) == 0 {
		return nil
	}
	key := clipKey(t, data)
	if key == b.held {
		return nil
	}
	b.held = key
	req := protocol.GuestRequest{
		Type: protocol.GuestCopy, Sensitive: state == "sensitive" || slices.Contains(types, sensitiveHint),
	}
	if isText(t) {
		req.Text = string(data)
		_, err = client.GuestCall(b.sock, req)
		return err
	}
	req.MIME = t
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	go func() {
		// An agent that refuses the copy stops reading: closing r ends the write.
		w.Write(data)
		w.Close()
	}()
	_, err = client.GuestCall(b.sock, req, r)
	return err
}

// fromAgent puts a change of the agent's clipboard on the Wayland selection.
func (b *bridge) fromAgent(ev protocol.ClipboardEvent) error {
	t := pickType(ev.Types)
	if t == "" {
		return nil
	}
	var data []byte
	if isText(t) {
		data = []byte(ev.Text)
	} else {
		var err error
		if data, err = b.paste(t); err != nil {
			return fmt.Errorf("the clipboard's %s: %w", t, err)
		}
	}
	if len(data) == 0 {
		return nil
	}
	key := clipKey(t, data)
	if key == b.held {
		return nil
	}
	b.held = key
	return b.wl.offer(t, data)
}

// paste fetches the agent's clipboard data of type t.
func (b *bridge) paste(t string) ([]byte, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	type read struct {
		data []byte
		err  error
	}
	got := make(chan read, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(r, protocol.MaxClipboardData+1))
		// Past the limit, closing fails the agent's write and so the paste.
		r.Close()
		got <- read{data, err}
	}()
	_, err = client.GuestCall(b.sock, protocol.GuestRequest{Type: protocol.GuestPaste, MIME: t}, w)
	w.Close()
	res := <-got
	if len(res.data) > protocol.MaxClipboardData {
		return nil, errTooLarge
	}
	if err == nil {
		err = res.err
	}
	return res.data, err
}

// clipKey identifies data of type t, with every plain-text type as one.
func clipKey(t string, data []byte) string {
	if isText(t) {
		t = "text/plain"
	}
	h := sha256.New()
	h.Write([]byte(t))
	h.Write([]byte{0})
	h.Write(data)
	return string(h.Sum(nil))
}

// pickType chooses the one type of an offer that crosses: plain text (UTF-8
// first), then an image, then other text, then the first type offered.
// Targets that aren't MIME types (X11's TEXT, the password manager's hint)
// never cross; "" when only they are offered.
func pickType(types []string) string {
	var plain, image, text, first string
	for _, t := range types {
		m, _, err := mime.ParseMediaType(t)
		if err != nil || !strings.Contains(m, "/") {
			continue
		}
		switch {
		case m == "text/plain":
			if plain == "" || isText(t) && !isText(plain) {
				plain = t
			}
		case strings.HasPrefix(m, "image/"):
			image = cmp.Or(image, t)
		case strings.HasPrefix(m, "text/"):
			text = cmp.Or(text, t)
		}
		first = cmp.Or(first, t)
	}
	return cmp.Or(plain, image, text, first)
}

// wlClipboard is the Wayland selection through wl-clipboard's programs.
type wlClipboard struct{}

func (wlClipboard) types() ([]string, error) {
	out, err := exec.Command("wl-paste", "--list-types").Output()
	if err != nil {
		return nil, fmt.Errorf("wl-paste --list-types: %w", err)
	}
	// One per line; a type's parameters may hold spaces.
	return strings.FieldsFunc(string(out), func(r rune) bool { return r == '\n' }), nil
}

func (wlClipboard) read(t string, limit int64) ([]byte, error) {
	cmd := exec.Command("wl-paste", "--no-newline", "--type", t)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(out, limit+1))
	if int64(len(data)) > limit {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, errTooLarge
	}
	if werr := cmd.Wait(); err == nil && werr != nil {
		err = fmt.Errorf("wl-paste: %w", werr)
	}
	return data, err
}

// offer runs wl-copy in the foreground, which serves pastes until another
// copy replaces it, and then exits.
func (wlClipboard) offer(t string, data []byte) error {
	// --type keeps wl-copy from guessing the type with xdg-mime.
	cmd := exec.Command("wl-copy", "--foreground", "--type", t)
	cmd.Stdin = bytes.NewReader(data)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
