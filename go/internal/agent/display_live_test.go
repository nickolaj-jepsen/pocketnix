package agent

import (
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// TestDisplayStreamLive streams from a real agent and compositor, which no
// unit test can: set POCKETNIX_DISPLAY_AGENT to an agent address (tcp:host:port
// or unix:path) whose display capture is a working grim.
func TestDisplayStreamLive(t *testing.T) {
	addr := os.Getenv("POCKETNIX_DISPLAY_AGENT")
	if addr == "" {
		t.Skip("set POCKETNIX_DISPLAY_AGENT to a live agent to stream from it")
	}
	network, target, ok := strings.Cut(addr, ":")
	if !ok {
		t.Fatalf("address %q is not network:target", addr)
	}
	conn, err := net.DialTimeout(network, target, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	if err := protocol.WriteJSON(conn, protocol.Hello{Type: protocol.TypeHello, Version: protocol.Version, Stream: protocol.StreamDisplay}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	reply, err := protocol.ReadHello(conn)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if reply.Type != protocol.TypeHello {
		t.Fatalf("agent refused the display stream: %+v", reply)
	}

	typ, payload, err := protocol.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read geometry: %v", err)
	}
	if typ != protocol.FrameDisplayGeometry {
		t.Fatalf("first frame type %d, want geometry", typ)
	}
	w, h, format, err := protocol.DecodeDisplayGeometry(payload)
	if err != nil {
		t.Fatal(err)
	}
	if format != protocol.DisplayFormatRGBA || w == 0 || h == 0 {
		t.Fatalf("geometry %dx%d format %d", w, h, format)
	}
	t.Logf("geometry %dx%d, %d bytes a frame", w, h, int(w)*int(h)*4)

	// One frame: the bands that cover the screen, in order and in full, and
	// within the frame cap.
	start := time.Now()
	covered := make([]bool, h)
	bands := 0
	for row := 0; row < int(h); {
		typ, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			t.Fatalf("read band: %v", err)
		}
		if typ != protocol.FrameDisplayBand {
			t.Fatalf("frame type %d, want a band", typ)
		}
		if len(payload) > protocol.MaxFramePayload {
			t.Fatalf("band frame is %d bytes, over the cap", len(payload))
		}
		y, rows, stride, _, err := protocol.DecodeDisplayBand(payload)
		if err != nil {
			t.Fatal(err)
		}
		if int(y) != row {
			t.Fatalf("band starts at %d, want %d", y, row)
		}
		if stride != uint32(w)*4 {
			t.Fatalf("stride %d, want %d", stride, uint32(w)*4)
		}
		for i := 0; i < int(rows); i++ {
			covered[int(y)+i] = true
		}
		row += int(rows)
		bands++
	}
	for row, ok := range covered {
		if !ok {
			t.Fatalf("row %d missing from the first frame", row)
		}
	}
	t.Logf("first frame: %d bands in %v", bands, time.Since(start))
}
