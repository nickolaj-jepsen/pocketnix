// Package portwatch notices TCP listeners in the guest coming and going.
package portwatch

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// The generated files are git-ignored; nix/go.nix runs this before building.
//go:generate go tool bpf2go -target bpfel -type event portwatch portwatch.c -- -I./headers

// Event is a TCP socket entering or leaving LISTEN, in any network namespace.
// It fires before the port is hashed, so /proc/net/tcp may not show it yet.
type Event struct {
	// Port is 0 for a socket that listen()s without bind().
	Port               uint16
	OldState, NewState int32
}

// EBPF is the raw tracepoint on sock:inet_sock_set_state.
type EBPF struct {
	objs   portwatchObjects
	link   link.Link
	events *ringbuf.Reader
}

// LoadEBPF loads and attaches the program. It needs CAP_BPF and CAP_PERFMON
// (root) and a kernel with BTF; the error carries the verifier's log.
func LoadEBPF() (*EBPF, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("memlock: %w", err)
	}
	w := &EBPF{}
	if err := loadPortwatchObjects(&w.objs, nil); err != nil {
		return nil, fmt.Errorf("load: %w", err)
	}
	var err error
	if w.link, err = link.AttachRawTracepoint(link.RawTracepointOptions{
		Name:    "inet_sock_set_state",
		Program: w.objs.InetSockSetState,
	}); err != nil {
		w.objs.Close()
		return nil, fmt.Errorf("attach: %w", err)
	}
	if w.events, err = ringbuf.NewReader(w.objs.Events); err != nil {
		w.Close()
		return nil, fmt.Errorf("ring buffer: %w", err)
	}
	return w, nil
}

// Read blocks for the next event. It returns ErrClosed after Close.
func (w *EBPF) Read() (Event, error) {
	rec, err := w.events.Read()
	if errors.Is(err, ringbuf.ErrClosed) {
		return Event{}, ErrClosed
	}
	if err != nil {
		return Event{}, err
	}
	var ev portwatchEvent
	if _, err := binary.Decode(rec.RawSample, binary.NativeEndian, &ev); err != nil {
		return Event{}, err
	}
	return Event{Port: ev.Port, OldState: ev.Oldstate, NewState: ev.Newstate}, nil
}

// ErrClosed is Read's error once the watcher is closed.
var ErrClosed = errors.New("portwatch: closed")

// Close detaches the program and ends a blocked Read.
func (w *EBPF) Close() error {
	var errs []error
	if w.events != nil {
		errs = append(errs, w.events.Close())
	}
	if w.link != nil {
		errs = append(errs, w.link.Close())
	}
	errs = append(errs, w.objs.Close())
	return errors.Join(errs...)
}
