package client

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"net"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// OpenFile is a FileOpen stream: ranged access to one file under the file
// provider's root. Close without Commit discards its writes.
type OpenFile struct {
	conn net.Conn
	r    *bufio.Reader
	// Info is the file as opened (Size 0 for a truncating mode).
	Info protocol.FileInfo
}

// Open opens path in an Android open mode ("r", "w", "wa", "rw", …).
func (c *Client) Open(path, mode string) (*OpenFile, error) {
	conn, reply, err := c.open(protocol.Hello{Stream: protocol.StreamFile, Op: protocol.FileOpen, Path: path, Mode: mode})
	if err != nil {
		return nil, err
	}
	f := &OpenFile{conn: conn, r: bufio.NewReader(conn)}
	if reply.File != nil {
		f.Info = *reply.File
	}
	return f, nil
}

func (f *OpenFile) call(typ byte, payload []byte) ([]byte, error) {
	if err := protocol.WriteFrame(f.conn, typ, payload); err != nil {
		return nil, err
	}
	t, out, err := protocol.ReadFrame(f.r)
	if err != nil {
		return nil, err
	}
	switch t {
	case protocol.FileFrameOK:
		return out, nil
	case protocol.FileFrameError:
		return nil, errors.New(string(out))
	}
	return nil, fmt.Errorf("unexpected file frame %#x", t)
}

// ReadAt reads up to n bytes at off; fewer only at the end of the file.
func (f *OpenFile) ReadAt(off int64, n int) ([]byte, error) {
	b := make([]byte, 12)
	binary.BigEndian.PutUint64(b, uint64(off))
	binary.BigEndian.PutUint32(b[8:], uint32(n))
	return f.call(protocol.FileFrameRead, b)
}

// WriteAt writes data at off.
func (f *OpenFile) WriteAt(off int64, data []byte) error {
	b := make([]byte, 8+len(data))
	binary.BigEndian.PutUint64(b, uint64(off))
	copy(b[8:], data)
	_, err := f.call(protocol.FileFrameWrite, b)
	return err
}

// Sync flushes the writes so far to the guest's disk.
func (f *OpenFile) Sync() error {
	_, err := f.call(protocol.FileFrameSync, nil)
	return err
}

// Commit puts the written file in place and ends the stream.
func (f *OpenFile) Commit() error {
	_, err := f.call(protocol.FileFrameCommit, nil)
	f.conn.Close()
	return err
}

// Close ends the stream; uncommitted writes are discarded.
func (f *OpenFile) Close() error { return f.conn.Close() }
