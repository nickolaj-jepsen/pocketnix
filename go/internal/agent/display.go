package agent

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// Display serves a display stream: the guest compositor's output, captured
// once per frame and forwarded as the bands that changed.
//
// The capture command is a process per frame (grim, say) rather than a
// compositor client in this process: it keeps the agent free of Wayland
// protocol bindings, and at the sizes a phone screen needs the copy is not
// what costs — the capture is.
type Display struct {
	// Compositor starts once, on the first stream. Empty captures whatever
	// already runs, such as a session's own compositor or a desktop's.
	Compositor []string
	// Env is added to the compositor's and the capture command's environment:
	// XDG_RUNTIME_DIR, WLR_BACKENDS, WLR_RENDERER, VK_ICD_FILENAMES, …
	Env []string
	// Capture prints one PPM (P6) frame on stdout; it runs once per frame.
	// Ignored when Helper is set.
	Capture []string
	// Helper is a long-lived capture client (capture/pocketnix-capture) that
	// prints damage rects and pixels itself; see display_helper.go.
	Helper []string
	// Input is the virtual device the app's touches and keys land on; nil
	// streams the screen without input.
	Input *Input
	// Interval is the quiet time between captures; zero means 100 ms.
	Interval time.Duration

	once sync.Once
}

// serveDisplay writes the geometry and then a frame's changed bands until the
// app goes away.
func (s *Server) serveDisplay(c net.Conn) {
	if s.Display == nil || (len(s.Display.Capture) == 0 && len(s.Display.Helper) == 0) {
		reject(c, "display: this agent has no capture command")
		return
	}
	if len(s.Display.Helper) > 0 {
		if protocol.WriteJSON(c, s.hello()) != nil {
			return
		}
		s.Display.serveHelper(c)
		return
	}
	s.Display.serve(c)
}

func (d *Display) serve(c net.Conn) {
	d.once.Do(d.startCompositor)
	interval := d.Interval
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	// The app sends nothing on a display stream, so a read that ends means it
	// closed us: without that, a stream whose capture keeps failing (no
	// compositor yet, say) would spin until the next successful frame.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The app sends nothing but input events on a display stream; a read
		// that ends means it closed us, and input stops with it.
		defer c.Close()
		for {
			typ, payload, err := protocol.ReadFrame(c)
			if err != nil {
				return
			}
			if typ != protocol.FrameDisplayInput {
				continue
			}
			kind, a, b, err := protocol.DecodeDisplayInput(payload)
			if err != nil {
				log.Printf("display: %v", err)
				continue
			}
			d.Input.Apply(kind, a, b)
		}
	}()
	var (
		prev       []byte // last frame, for the band diff
		width      int
		announced  bool
		lastErrLog time.Time
	)
	for {
		select {
		case <-done:
			return
		default:
		}
		rgba, w, h, err := d.capture()
		if err != nil {
			// A compositor that is still coming up fails for a moment; the
			// stream stays and retries, but doesn't fill the log.
			if time.Since(lastErrLog) > time.Second {
				log.Printf("display: capture: %v", err)
				lastErrLog = time.Now()
			}
			time.Sleep(interval)
			continue
		}
		if !announced || w != width {
			if err := protocol.WriteFrame(c, protocol.FrameDisplayGeometry, protocol.EncodeDisplayGeometry(uint16(w), uint16(h))); err != nil {
				return
			}
			announced, width, prev = true, w, nil
		}
		rows := protocol.MaxDisplayBandHeight(w)
		stride := w * 4
		for y := 0; y < h; y += rows {
			bandRows := min(rows, h-y)
			off := y * stride
			pixels := rgba[off : off+bandRows*stride]
			if prev != nil && bytes.Equal(prev[off:off+bandRows*stride], pixels) {
				continue
			}
			payload, err := protocol.EncodeDisplayBand(uint16(y), uint16(bandRows), uint32(stride), pixels)
			if err != nil {
				log.Printf("display: encode band: %v", err)
				return
			}
			if err := protocol.WriteFrame(c, protocol.FrameDisplayBand, payload); err != nil {
				return
			}
		}
		prev = rgba
		time.Sleep(interval)
	}
}

func (d *Display) startCompositor() {
	if len(d.Compositor) == 0 {
		return
	}
	cmd := exec.Command(d.Compositor[0], d.Compositor[1:]...)
	cmd.Env = d.environ()
	if err := cmd.Start(); err != nil {
		log.Printf("display: start compositor: %v", err)
		return
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("display: compositor exited: %v", err)
		}
	}()
}

func (d *Display) capture() (rgba []byte, w, h int, err error) {
	cmd := exec.Command(d.Capture[0], d.Capture[1:]...)
	cmd.Env = d.environ()
	out, err := cmd.Output()
	if err != nil {
		return nil, 0, 0, err
	}
	return decodePPM(out)
}

// environ is the agent's environment plus the display's, later entries
// winning.
func (d *Display) environ() []string {
	return append(os.Environ(), d.Env...)
}

var errNotPPM = errors.New("not a binary PPM")

// decodePPM turns a P6 PPM into the RGBA bytes the app expects.
func decodePPM(b []byte) (rgba []byte, width, height int, err error) {
	r := ppmReader{b: b}
	if magic, err := r.token(); err != nil || magic != "P6" {
		return nil, 0, 0, errNotPPM
	}
	ws, err := r.token()
	if err != nil {
		return nil, 0, 0, err
	}
	hs, err := r.token()
	if err != nil {
		return nil, 0, 0, err
	}
	ms, err := r.token()
	if err != nil {
		return nil, 0, 0, err
	}
	var maxval int
	if _, err := fmt.Sscanf(ms, "%d", &maxval); err != nil || maxval != 255 {
		return nil, 0, 0, fmt.Errorf("ppm maxval %q, want 255", ms)
	}
	if _, err := fmt.Sscanf(ws, "%d", &width); err != nil {
		return nil, 0, 0, fmt.Errorf("ppm width %q", ws)
	}
	if _, err := fmt.Sscanf(hs, "%d", &height); err != nil {
		return nil, 0, 0, fmt.Errorf("ppm height %q", hs)
	}
	if width <= 0 || height <= 0 || width > 1<<14 || height > 1<<14 {
		return nil, 0, 0, fmt.Errorf("ppm size %dx%d", width, height)
	}
	// Exactly one whitespace byte separates the header from the raster (at
	// most a CRLF pair), and that byte is where the pixels start: skipping
	// whitespace generally would eat pixels whose first byte is one.
	switch {
	case r.i+1 < len(b) && b[r.i] == '\r' && b[r.i+1] == '\n':
		r.i += 2
	case r.i < len(b) && (b[r.i] == '\n' || b[r.i] == '\r' || b[r.i] == ' ' || b[r.i] == '\t'):
		r.i++
	}
	need := width * height * 3
	if len(b)-r.i < need {
		return nil, 0, 0, fmt.Errorf("ppm has %d pixel bytes, want %d", len(b)-r.i, need)
	}
	rgb := b[r.i : r.i+need]
	rgba = make([]byte, width*height*4)
	for i, j := 0, 0; i < len(rgb); i, j = i+3, j+4 {
		rgba[j] = rgb[i]
		rgba[j+1] = rgb[i+1]
		rgba[j+2] = rgb[i+2]
		rgba[j+3] = 0xff
	}
	return rgba, width, height, nil
}

// ppmReader walks the header's whitespace-separated tokens, skipping '#' comments.
type ppmReader struct {
	b []byte
	i int
}

func (r *ppmReader) token() (string, error) {
	for r.i < len(r.b) {
		switch c := r.b[r.i]; {
		case c == '#':
			for r.i < len(r.b) && r.b[r.i] != '\n' {
				r.i++
			}
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			r.i++
		default:
			start := r.i
			for r.i < len(r.b) {
				c := r.b[r.i]
				if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
					return string(r.b[start:r.i]), nil
				}
				r.i++
			}
			return string(r.b[start:r.i]), nil
		}
	}
	return "", errors.New("ppm header ended early")
}
