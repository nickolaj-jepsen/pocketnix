package agent

import (
	"net"
	"os"
	"strconv"
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

	// Throughput: what the capture and the wire actually deliver, which is
	// what bounds the app's frame rate.
	window := 3 * time.Second
	if v := os.Getenv("POCKETNIX_DISPLAY_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			window = time.Duration(n) * time.Second
		}
	}
	var frames, bytes int
	deadline := time.Now().Add(window)
	last := covered
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline)
		typ, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			break
		}
		if typ != protocol.FrameDisplayBand {
			continue
		}
		bytes += len(payload)
		y, rows, _, _, _ := 0, 0, 0, 0, error(nil)
		_ = y
		_ = rows
		last = nil
		frames++
	}
	t.Logf("%v: %d bands, %d KiB (%.0f KiB/s)", window, frames, bytes/1024, float64(bytes)/1024/window.Seconds())
	_ = last
}
