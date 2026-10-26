package agent

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// notifications assigns notification ids and feeds the app's answers to the
// guest's subscribers (the notifications daemon).
type notifications struct {
	mu   sync.Mutex
	last uint32
	subs map[chan protocol.GuestEvent]struct{}
}

const (
	// maxActions bounds a notification's actions; Android shows three buttons.
	maxActions = 16
	// subscriberBacklog is how many events a slow subscriber may fall behind before losing some.
	subscriberBacklog = 64
)

// id returns the id a notify gets: replaces when set, else a fresh one.
func (n *notifications) id(replaces uint32) uint32 {
	n.mu.Lock()
	defer n.mu.Unlock()
	if replaces != 0 {
		// A replaced id handed out before a restart mustn't come round again soon.
		n.last = max(n.last, replaces)
		return replaces
	}
	n.last++
	if n.last == 0 {
		n.last = 1
	}
	return n.last
}

func (n *notifications) subscribe() chan protocol.GuestEvent {
	ch := make(chan protocol.GuestEvent, subscriberBacklog)
	n.mu.Lock()
	if n.subs == nil {
		n.subs = map[chan protocol.GuestEvent]struct{}{}
	}
	n.subs[ch] = struct{}{}
	n.mu.Unlock()
	return ch
}

func (n *notifications) unsubscribe(ch chan protocol.GuestEvent) {
	n.mu.Lock()
	delete(n.subs, ch)
	n.mu.Unlock()
}

func (n *notifications) publish(ev protocol.GuestEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for ch := range n.subs {
		select {
		case ch <- ev:
		default:
			log.Printf("notification subscriber fell behind; dropped %s %d", ev.Type, ev.ID)
		}
	}
}

// guestNotify serves GuestNotify and GuestNotifyClose.
func (s *Server) guestNotify(req protocol.GuestRequest) (protocol.GuestReply, error) {
	if req.Type == protocol.GuestNotifyClose {
		if req.ID == 0 {
			return protocol.GuestReply{}, errors.New("no notification id")
		}
		return protocol.GuestReply{}, s.event(protocol.Event{Type: protocol.TypeNotifyClose, Notification: req.ID})
	}
	if req.Title == "" && req.Body == "" {
		return protocol.GuestReply{}, errors.New("an empty notification")
	}
	switch req.Urgency {
	case "", protocol.UrgencyLow, protocol.UrgencyNormal, protocol.UrgencyCritical:
	default:
		return protocol.GuestReply{}, fmt.Errorf("unknown urgency %q", req.Urgency)
	}
	if len(req.Actions) > maxActions {
		return protocol.GuestReply{}, fmt.Errorf("more than %d actions", maxActions)
	}
	for _, a := range req.Actions {
		if a.Key == "" {
			return protocol.GuestReply{}, errors.New("an action without a key")
		}
	}
	id := s.notes.id(req.Replaces)
	err := s.event(protocol.Event{
		Type: protocol.TypeNotify, Title: req.Title, Body: req.Body,
		Notification: id, AppName: req.AppName, Urgency: req.Urgency, Actions: req.Actions,
	})
	if err != nil {
		return protocol.GuestReply{}, err
	}
	return protocol.GuestReply{ID: id}, nil
}

// notifyFromApp hands an action or a dismissal to the subscribers; with none
// (no daemon runs) it goes nowhere.
func (s *Server) notifyFromApp(req protocol.Request) error {
	if req.Notification == 0 {
		return errors.New("no notification id")
	}
	ev := protocol.GuestEvent{Type: req.Type, ID: req.Notification}
	if req.Type == protocol.TypeNotifyAction {
		if req.Key == "" {
			return errors.New("no action key")
		}
		ev.Key = req.Key
	} else {
		ev.Reason = req.Reason
		if ev.Reason == 0 {
			ev.Reason = protocol.ClosedDismissed
		}
	}
	s.notes.publish(ev)
	return nil
}

// subscribe answers a GuestSubscribe on c, then feeds it events until the
// guest closes it; r is c's reader, which may hold nothing but its EOF.
func (s *Server) subscribe(c net.Conn, r *bufio.Reader) {
	c.SetReadDeadline(time.Time{})
	ch := s.notes.subscribe()
	defer s.notes.unsubscribe(ch)
	if protocol.WriteJSON(c, protocol.GuestReply{}) != nil {
		return
	}
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			if _, err := r.ReadByte(); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case ev := <-ch:
			// A subscriber that stopped reading mustn't hold its slot forever.
			c.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
			if protocol.WriteJSON(c, ev) != nil {
				return
			}
		case <-gone:
			return
		}
	}
}
