package protocol

import (
	"encoding/binary"
	"errors"
	"io"
)

// UDP streams carry datagrams as [length u16 BE][payload], each way.
const datagramHeader = 2

// MaxDatagram is the largest payload a UDP stream carries (an IPv4 UDP
// datagram's maximum).
const MaxDatagram = 65507

// ErrDatagramTooLarge is returned for a payload over MaxDatagram.
var ErrDatagramTooLarge = errors.New("datagram too large")

// WriteDatagram writes one datagram in a single Write call, so datagrams
// from concurrent writers don't interleave as long as w serializes Writes.
func WriteDatagram(w io.Writer, p []byte) error {
	if len(p) > MaxDatagram {
		return ErrDatagramTooLarge
	}
	buf := make([]byte, datagramHeader+len(p))
	binary.BigEndian.PutUint16(buf, uint16(len(p)))
	copy(buf[datagramHeader:], p)
	_, err := w.Write(buf)
	return err
}

// ReadDatagram reads one datagram into buf, which must hold MaxDatagram
// bytes, and returns its length. It returns io.EOF only at a clean boundary.
func ReadDatagram(r io.Reader, buf []byte) (int, error) {
	var hdr [datagramHeader]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n > MaxDatagram || n > len(buf) {
		return 0, ErrDatagramTooLarge
	}
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	return n, nil
}
