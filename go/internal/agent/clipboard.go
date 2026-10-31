package agent

import (
	"bufio"
	"net"
	"sync"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// clipWatchers are the guest's clipboard subscribers. Each holds only the
// latest change: a burst must never leave one on a stale value.
type clipWatchers struct {
	mu   sync.Mutex
	subs map[chan protocol.ClipboardEvent]struct{}
}

func (w *clipWatchers) unsubscribe(ch chan protocol.ClipboardEvent) {
	w.mu.Lock()
	delete(w.subs, ch)
	w.mu.Unlock()
}

// replace leaves ev as ch's only unread event; callers hold w.mu, so no
// other sender races it.
func replace(ch chan protocol.ClipboardEvent, ev protocol.ClipboardEvent) {
	select {
	case <-ch:
	default:
	}
	ch <- ev
}

// clipboardEvent is the clipboard as it is; ok is false while it holds nothing.
func (s *Server) clipboardEvent() (ev protocol.ClipboardEvent, ok bool) {
	x := &s.transfers
	x.mu.Lock()
	types, sensitive := append([]string(nil), x.clipTypes...), x.clipSensitive
	x.mu.Unlock()
	ev = protocol.ClipboardEvent{Type: protocol.TypeClipboard, Text: s.clipboardText(), Types: types, Sensitive: sensitive}
	return ev, len(types) > 0
}

// publishClipboard hands the clipboard to every subscriber. It reads the
// clipboard under the subscribers' lock, so whichever publish runs last
// leaves them on the latest value, however concurrent changes interleave.
func (s *Server) publishClipboard() {
	w := &s.clipWatchers
	w.mu.Lock()
	defer w.mu.Unlock()
	ev, ok := s.clipboardEvent()
	if !ok {
		return
	}
	for ch := range w.subs {
		replace(ch, ev)
	}
}

// subscribeClipboard serves a GuestSubscribeClipboard on c, starting with
// the clipboard as it is.
func (s *Server) subscribeClipboard(c net.Conn, r *bufio.Reader) {
	ch := make(chan protocol.ClipboardEvent, 1)
	w := &s.clipWatchers
	w.mu.Lock()
	if w.subs == nil {
		w.subs = map[chan protocol.ClipboardEvent]struct{}{}
	}
	w.subs[ch] = struct{}{}
	if ev, ok := s.clipboardEvent(); ok {
		ch <- ev
	}
	w.mu.Unlock()
	defer w.unsubscribe(ch)
	serveSubscription(c, r, ch)
}
