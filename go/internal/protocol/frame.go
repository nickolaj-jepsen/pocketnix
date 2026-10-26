package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// PTY streams carry frames of [type u8][length u32 BE][payload].
const (
	// FrameData is terminal bytes: keyboard input from the app, output from the guest.
	FrameData byte = 0
	// FrameResize (app → agent) is [rows u16][cols u16][xpixel u16][ypixel u16].
	FrameResize byte = 1
	// FrameExit (agent → app) is [status i32]: the exit code, or minus the
	// signal number that killed the process. The agent closes after it.
	FrameExit byte = 2
)

// MaxFramePayload bounds a frame so a corrupt length can't make a reader
// allocate without limit.
const MaxFramePayload = 1 << 20

const frameHeader = 5

// ErrFrameTooLarge is returned for a frame longer than MaxFramePayload.
var ErrFrameTooLarge = errors.New("frame too large")

// WriteFrame writes one frame in a single Write call, so frames from
// concurrent writers don't interleave as long as w serializes Writes.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxFramePayload {
		return ErrFrameTooLarge
	}
	buf := make([]byte, frameHeader+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:frameHeader], uint32(len(payload)))
	copy(buf[frameHeader:], payload)
	_, err := w.Write(buf)
	return err
}

// ReadFrame reads one frame. It returns io.EOF only at a clean frame boundary.
func ReadFrame(r io.Reader) (typ byte, payload []byte, err error) {
	var hdr [frameHeader]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFramePayload {
		return 0, nil, ErrFrameTooLarge
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// Winsize is a FrameResize payload.
type Winsize struct {
	Rows, Cols, X, Y uint16
}

// Encode returns the FrameResize payload.
func (w Winsize) Encode() []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint16(b[0:], w.Rows)
	binary.BigEndian.PutUint16(b[2:], w.Cols)
	binary.BigEndian.PutUint16(b[4:], w.X)
	binary.BigEndian.PutUint16(b[6:], w.Y)
	return b
}

// DecodeWinsize parses a FrameResize payload.
func DecodeWinsize(b []byte) (Winsize, error) {
	if len(b) != 8 {
		return Winsize{}, fmt.Errorf("resize payload is %d bytes, want 8", len(b))
	}
	return Winsize{
		Rows: binary.BigEndian.Uint16(b[0:]),
		Cols: binary.BigEndian.Uint16(b[2:]),
		X:    binary.BigEndian.Uint16(b[4:]),
		Y:    binary.BigEndian.Uint16(b[6:]),
	}, nil
}

// EncodeExit returns the FrameExit payload.
func EncodeExit(status int32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(status))
	return b
}

// DecodeExit parses a FrameExit payload.
func DecodeExit(b []byte) (int32, error) {
	if len(b) != 4 {
		return 0, fmt.Errorf("exit payload is %d bytes, want 4", len(b))
	}
	return int32(binary.BigEndian.Uint32(b)), nil
}
