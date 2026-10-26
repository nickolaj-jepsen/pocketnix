package agent

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// writePPM writes a P6 frame whose pixels the callers can predict.
func writePPM(t *testing.T, path string, w, h int, rgb []byte) {
	t.Helper()
	hdr := fmt.Sprintf("P6\n# pocketnix test fixture\n%d %d\n255\n", w, h)
	if err := os.WriteFile(path, append([]byte(hdr), rgb...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func serveForTest(t *testing.T, d *Display) net.Conn {
	t.Helper()
	app, agentSide := net.Pipe()
	t.Cleanup(func() { app.Close() })
	go d.serve(agentSide)
	return app
}

func readFrame(t *testing.T, c net.Conn) (byte, []byte) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	typ, payload, err := protocol.ReadFrame(c)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return typ, payload
}

func expectQuiet(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	typ, payload, err := protocol.ReadFrame(c)
	if err == nil {
		t.Fatalf("expected no frame, got type %d (%d bytes)", typ, len(payload))
	}
}

func TestDisplaySendsGeometryThenOnlyChangedBands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frame.ppm")
	red := bytes.Repeat([]byte{0xff, 0x00, 0x00}, 4*2)
	blue := bytes.Repeat([]byte{0x00, 0x00, 0xff}, 4*2)
	writePPM(t, path, 4, 2, red)

	d := &Display{Capture: []string{"cat", path}, Interval: 10 * time.Millisecond}
	c := serveForTest(t, d)

	typ, payload := readFrame(t, c)
	if typ != protocol.FrameDisplayGeometry {
		t.Fatalf("first frame type %d, want geometry", typ)
	}
	w, h, format, err := protocol.DecodeDisplayGeometry(payload)
	if err != nil || w != 4 || h != 2 || format != protocol.DisplayFormatRGBA {
		t.Fatalf("geometry %dx%d format %d: %v", w, h, format, err)
	}

	typ, payload = readFrame(t, c)
	if typ != protocol.FrameDisplayBand {
		t.Fatalf("frame type %d, want a band", typ)
	}
	y, rows, stride, pixels, err := protocol.DecodeDisplayBand(payload)
	if err != nil {
		t.Fatal(err)
	}
	if y != 0 || int(rows) != int(h) || stride != uint32(w)*4 {
		t.Fatalf("band at %d rows %d stride %d, want 0/%d/%d", y, rows, stride, h, w*4)
	}
	want := []byte{0xff, 0x00, 0x00, 0xff, 0xff, 0x00, 0x00, 0xff}
	for i := 0; i < 8; i++ {
		if pixels[i] != want[i] {
			t.Fatalf("pixel byte %d is %#x, want %#x (RGBA, opaque)", i, pixels[i], want[i])
		}
	}

	// The same frame again: nothing to send.
	expectQuiet(t, c)

	// Every pixel changes: the frame's single band comes again.
	writePPM(t, path, 4, 2, blue)
	typ, payload = readFrame(t, c)
	if typ != protocol.FrameDisplayBand {
		t.Fatalf("frame type %d, want a band", typ)
	}
	y, _, _, pixels, err = protocol.DecodeDisplayBand(payload)
	if err != nil {
		t.Fatal(err)
	}
	if y != 0 {
		t.Fatalf("changed band starts at %d, want 0", y)
	}
	if !bytes.Equal(pixels[:8], bytes.Repeat([]byte{0x00, 0x00, 0xff, 0xff}, 2)) {
		t.Fatalf("band did not carry the new pixels: %#x", pixels[:8])
	}
}

func TestDisplaySplitsTallFramesIntoBandsThatFit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frame.ppm")
	const w, h = 300, 1300
	// One row per band height, every row a different shade, so a split that
	// loses or reorders rows shows up.
	rgb := make([]byte, w*h*3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 3
			rgb[i], rgb[i+1], rgb[i+2] = byte(y), byte(y>>8), 0x7f
		}
	}
	writePPM(t, path, w, h, rgb)

	d := &Display{Capture: []string{"cat", path}, Interval: 10 * time.Millisecond}
	c := serveForTest(t, d)
	if typ, _ := readFrame(t, c); typ != protocol.FrameDisplayGeometry {
		t.Fatalf("first frame type %d, want geometry", typ)
	}

	rows := make([]byte, h)
	for y := 0; y < h; y += protocol.MaxDisplayBandHeight(w) {
		typ, payload := readFrame(t, c)
		if typ != protocol.FrameDisplayBand {
			t.Fatalf("frame type %d, want a band", typ)
		}
		if len(payload) > protocol.MaxFramePayload {
			t.Fatalf("band frame is %d bytes, over the %d cap", len(payload), protocol.MaxFramePayload)
		}
		bandY, bandRows, stride, pixels, err := protocol.DecodeDisplayBand(payload)
		if err != nil {
			t.Fatal(err)
		}
		if int(bandY) != y {
			t.Fatalf("band starts at %d, want %d", bandY, y)
		}
		for i := 0; i < int(bandRows); i++ {
			row := int(bandY) + i
			if got := pixels[i*int(stride)]; got != byte(row) {
				t.Fatalf("band row %d starts with %d, want %d", row, got, byte(row))
			}
			rows[row] = 1
		}
	}
	for y, seen := range rows {
		if seen == 0 {
			t.Fatalf("row %d never arrived", y)
		}
	}

	// Change one pixel in the last band: only that band comes again.
	rowsPer := protocol.MaxDisplayBandHeight(w)
	lastBand := (h - 1) / rowsPer * rowsPer
	rgb[((h-1)*w+7)*3] = 0x42
	writePPM(t, path, w, h, rgb)
	typ, payload := readFrame(t, c)
	if typ != protocol.FrameDisplayBand {
		t.Fatalf("frame type %d, want a band", typ)
	}
	bandY, _, _, _, err := protocol.DecodeDisplayBand(payload)
	if err != nil {
		t.Fatal(err)
	}
	if int(bandY) != lastBand {
		t.Fatalf("changed band starts at %d, want the last band at %d", bandY, lastBand)
	}
	expectQuiet(t, c)
}

func TestDecodePPMRejectsWhatItCannotRead(t *testing.T) {
	cases := map[string][]byte{
		"png":       []byte("\x89PNG\r\n\x1a\n"),
		"maxval":    []byte("P6\n1 1\n65535\n\x00\x00\x00"),
		"short":     []byte("P6\n2 2\n255\n\x00\x00"),
		"empty":     nil,
		"truncated": []byte("P6\n2"),
	}
	for name, in := range cases {
		if _, _, _, err := decodePPM(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ppm := []byte("P6\r\n# comment only\r\n1 1\r\n255\r\n\x01\x02\x03")
	rgba, w, h, err := decodePPM(ppm)
	if err != nil || w != 1 || h != 1 {
		t.Fatalf("decode %dx%d: %v", w, h, err)
	}
	if !bytes.Equal(rgba, []byte{0x01, 0x02, 0x03, 0xff}) {
		t.Fatalf("pixels %#x", rgba)
	}
}
