package agent

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// The capture helper (capture/pocketnix-capture.c) is one long-lived
// wlr-screencopy client: a process per frame cost 30-80 ms of its own, and a
// PPM round trip another few milliseconds of pure copying. It prints, per
// frame, its damage rects and their pixels as RGBA (big-endian header):
//
//	[width u32][height u32][rects u32]
//	rects × [x u32][y u32][width u32][height u32]
//	then, per rect, height rows of width × 4 bytes
//
// What the compositor calls damage is coarse — with pixman, often most of the
// output — so the agent tiles the frame itself and forwards only the tiles
// whose pixels really changed.
const (
	tileWidth  = 128
	tileHeight = 32
)

// helperFrame is one frame's pixels, already assembled at output size.
type helperFrame struct {
	width, height int
	rgba          []byte
}

// readHelperFrame reads one frame into a buffer of its own size.
func readHelperFrame(r *bufio.Reader, reuse []byte) (helperFrame, []byte, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return helperFrame{}, reuse, err
	}
	width := int(binary.BigEndian.Uint32(hdr[0:]))
	height := int(binary.BigEndian.Uint32(hdr[4:]))
	rects := int(binary.BigEndian.Uint32(hdr[8:]))
	if width <= 0 || height <= 0 || width > 1<<14 || height > 1<<14 || rects > 1024 {
		return helperFrame{}, reuse, fmt.Errorf("helper frame %dx%d with %d rects", width, height, rects)
	}
	pixels := width * height * 4
	if cap(reuse) < pixels {
		reuse = make([]byte, pixels)
	}
	frame := helperFrame{width: width, height: height, rgba: reuse[:pixels]}
	// All the rect headers come first, then their pixels: the helper writes it
	// that way so a reader can size its reads.
	type rect struct{ x, y, w, h int }
	boxes := make([]rect, rects)
	var hdr16 [16]byte
	for i := range boxes {
		if _, err := io.ReadFull(r, hdr16[:]); err != nil {
			return helperFrame{}, reuse, err
		}
		boxes[i] = rect{
			x: int(binary.BigEndian.Uint32(hdr16[0:])),
			y: int(binary.BigEndian.Uint32(hdr16[4:])),
			w: int(binary.BigEndian.Uint32(hdr16[8:])),
			h: int(binary.BigEndian.Uint32(hdr16[12:])),
		}
		if boxes[i].x < 0 || boxes[i].y < 0 || boxes[i].w <= 0 || boxes[i].h <= 0 ||
			boxes[i].x+boxes[i].w > width || boxes[i].y+boxes[i].h > height {
			return helperFrame{}, reuse, fmt.Errorf("helper rect %dx%d at %d,%d outside %dx%d",
				boxes[i].w, boxes[i].h, boxes[i].x, boxes[i].y, width, height)
		}
	}
	for _, b := range boxes {
		rowBytes := b.w * 4
		for row := 0; row < b.h; row++ {
			dst := frame.rgba[((b.y+row)*width+b.x)*4:]
			if _, err := io.ReadFull(r, dst[:rowBytes]); err != nil {
				return helperFrame{}, reuse, err
			}
		}
	}
	return frame, reuse, nil
}

// serveHelper relays the helper's frames as tiles that changed since the last
// one, so a coarse damage report still costs only what moved on screen.
func (d *Display) serveHelper(c net.Conn) {
	cmd := exec.Command(d.Helper[0], d.Helper[1:]...)
	cmd.Env = d.environ()
	out, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("display: helper: %v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("display: start helper: %v", err)
		return
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer c.Close()
		io.Copy(io.Discard, c)
	}()

	r := bufio.NewReaderSize(out, 1<<20)
	var (
		prev     []byte
		prevW    int
		prevH    int
		sentGeom bool
		buf      []byte
		frame    helperFrame
	)
	for {
		frame, buf, err = readHelperFrame(r, buf)
		if err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				log.Printf("display: helper frame: %v", err)
			}
			return
		}
		if !sentGeom || frame.width != prevW || frame.height != prevH {
			if err := protocol.WriteFrame(c, protocol.FrameDisplayGeometry, protocol.EncodeDisplayGeometry(uint16(frame.width), uint16(frame.height))); err != nil {
				return
			}
			sentGeom, prevW, prevH, prev = true, frame.width, frame.height, nil
		}
		if prev == nil {
			prev = make([]byte, len(frame.rgba))
		}
		if !sendChangedTiles(c, frame, prev) {
			return
		}
		copy(prev, frame.rgba)
		select {
		case <-done:
			return
		default:
		}
	}
}

// sendChangedTiles writes the tiles whose pixels differ from prev, and reports
// whether the connection took them.
func sendChangedTiles(c net.Conn, frame helperFrame, prev []byte) bool {
	for y := 0; y < frame.height; y += tileHeight {
		rows := min(tileHeight, frame.height-y)
		for x := 0; x < frame.width; x += tileWidth {
			cols := min(tileWidth, frame.width-x)
			rowBytes := cols * 4
			changed := false
			for row := 0; row < rows; row++ {
				off := ((y+row)*frame.width + x) * 4
				if !bytesEqual(frame.rgba[off:off+rowBytes], prev[off:off+rowBytes]) {
					changed = true
					break
				}
			}
			if !changed {
				continue
			}
			tile := make([]byte, rows*rowBytes)
			for row := 0; row < rows; row++ {
				off := ((y+row)*frame.width + x) * 4
				copy(tile[row*rowBytes:(row+1)*rowBytes], frame.rgba[off:off+rowBytes])
			}
			payload, err := protocol.EncodeDisplayRect(uint16(x), uint16(y), uint16(cols), uint16(rows), uint32(rowBytes), tile)
			if err != nil {
				log.Printf("display: encode tile: %v", err)
				return false
			}
			if err := protocol.WriteFrame(c, protocol.FrameDisplayRect, payload); err != nil {
				return false
			}
		}
	}
	return true
}

// bytesEqual is bytes.Equal without the import in the hot loop's callers.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
