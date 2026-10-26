package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// fakeAgent answers a control stream's Hello, then hands each request to
// answer, which writes whatever lines it likes.
func fakeAgent(t *testing.T, answer func(w net.Conn, req protocol.Request)) *Control {
	t.Helper()
	app, agent := net.Pipe()
	t.Cleanup(func() { app.Close(); agent.Close() })
	go func() {
		r := bufio.NewReader(agent)
		if _, err := protocol.ReadHello(r); err != nil {
			return
		}
		protocol.WriteJSON(agent, protocol.Hello{Type: protocol.TypeHello, Version: protocol.Version})
		for {
			line, err := protocol.ReadLine(r)
			if err != nil {
				return
			}
			var req protocol.Request
			json.Unmarshal(line, &req)
			answer(agent, req)
		}
	}()
	c := &Client{App: "test", Dial: func() (net.Conn, error) { return app, nil }}
	ctl, err := c.Control()
	if err != nil {
		t.Fatal(err)
	}
	return ctl
}

func line(w net.Conn, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}

func TestRepliesMatchByID(t *testing.T) {
	ctl := fakeAgent(t, func(w net.Conn, req protocol.Request) {
		// A newer agent's push, then another request's late answer, then this one's.
		line(w, `{"type":"something-new"}`)
		line(w, `{"type":"history","id":%d,"entries":["stale"]}`, req.ID+100)
		line(w, `{"type":"history","id":%d,"entries":["ls"]}`, req.ID)
	})
	for range 3 {
		h, err := ctl.History(0)
		if err != nil || len(h) != 1 || h[0] != "ls" {
			t.Fatalf("history = %q, %v", h, err)
		}
	}
}

func TestErrorAnswersItsRequest(t *testing.T) {
	ctl := fakeAgent(t, func(w net.Conn, req protocol.Request) {
		line(w, `{"type":"error","message":"nope","id":%d}`, req.ID)
	})
	if err := ctl.Ping(); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("ping: %v", err)
	}
}

func TestAnswersWithoutIDFromAnOlderAgent(t *testing.T) {
	ctl := fakeAgent(t, func(w net.Conn, req protocol.Request) {
		line(w, `{"type":"something-new"}`)
		line(w, `{"type":"pong"}`)
	})
	if err := ctl.Ping(); err != nil {
		t.Fatal(err)
	}
}

func TestUnreadConfigsDontStallTheStream(t *testing.T) {
	ctl := fakeAgent(t, func(w net.Conn, req protocol.Request) {
		for i := range 100 {
			line(w, `{"type":"config","hostname":"h%d"}`, i)
		}
		line(w, `{"type":"pong","id":%d}`, req.ID)
	})
	if err := ctl.Ping(); err != nil {
		t.Fatal(err)
	}
	var last protocol.Config
	for {
		select {
		case cfg := <-ctl.Configs():
			last = cfg
			continue
		case <-time.After(100 * time.Millisecond):
		}
		break
	}
	if last.Hostname != "h99" {
		t.Errorf("latest config %q, want h99", last.Hostname)
	}
}

func TestUnansweredRequestTimesOut(t *testing.T) {
	old := replyTimeout
	replyTimeout = 100 * time.Millisecond
	t.Cleanup(func() { replyTimeout = old })
	first := true
	ctl := fakeAgent(t, func(w net.Conn, req protocol.Request) {
		if first {
			first = false
			return
		}
		line(w, `{"type":"pong","id":%d}`, req.ID-1) // the first one's, late
		line(w, `{"type":"pong","id":%d}`, req.ID)
	})
	if err := ctl.Ping(); err == nil {
		t.Fatal("an unanswered ping succeeded")
	}
	if err := ctl.Ping(); err != nil {
		t.Fatalf("the next ping: %v", err)
	}
}
