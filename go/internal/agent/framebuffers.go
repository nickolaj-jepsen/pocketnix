package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// maxFrameBuffers bounds one GuestFrameBuffers request: the compositor's
// set at its default frames in flight (a slot more than it lets fly) of the
// largest buffer the GUI protocol allows (the compositor's MAX_BUFFER_BYTES).
const maxFrameBuffers = 256 << 20

// frameBuffers serves a GuestFrameBuffers request: pinned memory, answered
// with its memfd attached and its pages' guest-physical addresses, and held
// until the connection closes. Pinning is root's to do, and pagemap shows
// page frames only to CAP_SYS_ADMIN.
func (s *Server) frameBuffers(c net.Conn, r *bufio.Reader, req protocol.GuestRequest) {
	fb, err := pinFrames(req.Bytes)
	if err != nil {
		protocol.WriteJSON(c, protocol.GuestReply{Error: err.Error()})
		return
	}
	defer fb.close()
	if err := writeWithFd(c, protocol.GuestReply{Pages: fb.pages}, fb.memfd); err != nil {
		log.Printf("frame buffers: %v", err)
		return
	}
	// The pin lasts as long as the connection.
	for {
		if _, err := r.ReadByte(); err != nil {
			return
		}
	}
}

// writeWithFd writes v as a JSON line carrying fd (SCM_RIGHTS).
func writeWithFd(c net.Conn, v any, fd int) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return sendFd(c, append(line, '\n'), fd)
}

// sendFd writes all of payload to c, a unix socket, with a copy of fd
// attached (SCM_RIGHTS) to its first byte.
func sendFd(c net.Conn, payload []byte, fd int) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("a %T can't carry a descriptor", c)
	}
	// WriteMsgUnix returns a short count, not an error, once the send buffer fills.
	n, _, err := uc.WriteMsgUnix(payload, unix.UnixRights(fd), nil)
	if err == nil && n < len(payload) {
		_, err = uc.Write(payload[n:])
	}
	return err
}

// pinnedFrames is memory whose pages stay put: what the app reads through
// guest RAM must not move while the compositor writes it.
type pinnedFrames struct {
	memfd int
	mem   []byte
	// ring holds the long-term pin, -1 when only mlock does.
	ring  int
	pages []uint64
}

func pinFrames(n int64) (*pinnedFrames, error) {
	page := int64(os.Getpagesize())
	if n <= 0 || n > maxFrameBuffers {
		return nil, fmt.Errorf("frame buffers of %d bytes: want 1..%d", n, maxFrameBuffers)
	}
	n = (n + page - 1) / page * page
	fd, err := unix.MemfdCreate("pocketnix-frames", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("memfd: %w", err)
	}
	fb := &pinnedFrames{memfd: fd, ring: -1}
	fail := func(what string, err error) (*pinnedFrames, error) {
		fb.close()
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if err := unix.Ftruncate(fd, n); err != nil {
		return fail("sizing the memfd", err)
	}
	if fb.mem, err = unix.Mmap(fd, 0, int(n), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED); err != nil {
		return fail("mapping the memfd", err)
	}
	for i := int64(0); i < n; i += page {
		fb.mem[i] = 0
	}
	// A long-term pin (io_uring's registered buffers) also moves the pages out of
	// movable memory first, so compaction can't move them later; mlock alone allows that.
	if fb.ring, err = registerBuffer(fb.mem); err != nil {
		log.Printf("frame buffers: no io_uring pin (%v); locking them instead", err)
		if err := unix.Mlock(fb.mem); err != nil {
			return fail("locking the frame buffers", err)
		}
	}
	if fb.pages, err = guestPhysical(fb.mem); err != nil {
		return fail("reading pagemap", err)
	}
	return fb, nil
}

func (fb *pinnedFrames) close() {
	if fb.ring >= 0 {
		unix.Close(fb.ring)
	}
	if fb.mem != nil {
		unix.Munmap(fb.mem)
	}
	unix.Close(fb.memfd)
}

// registerBuffer pins mem as an io_uring fixed buffer; closing the ring it
// returns unpins it.
func registerBuffer(mem []byte) (int, error) {
	// struct io_uring_params: zero asks for defaults.
	var params [120]byte
	ring, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, 1, uintptr(unsafe.Pointer(&params)), 0)
	if errno != 0 {
		return -1, fmt.Errorf("io_uring_setup: %w", errno)
	}
	iov := unix.Iovec{Base: &mem[0]}
	iov.SetLen(len(mem))
	const ioringRegisterBuffers = 0
	if _, _, errno := unix.Syscall6(unix.SYS_IO_URING_REGISTER, ring, ioringRegisterBuffers, uintptr(unsafe.Pointer(&iov)), 1, 0, 0); errno != 0 {
		unix.Close(int(ring))
		return -1, fmt.Errorf("registering the buffer: %w", errno)
	}
	return int(ring), nil
}

// guestPhysical returns the physical address of each of mem's pages, which
// must be present.
func guestPhysical(mem []byte) ([]uint64, error) {
	f, err := os.Open("/proc/self/pagemap")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	page := uintptr(os.Getpagesize())
	start := uintptr(unsafe.Pointer(&mem[0]))
	n := uintptr(len(mem)) / page
	raw := make([]byte, 8*n)
	if _, err := f.ReadAt(raw, int64(start/page*8)); err != nil {
		return nil, err
	}
	pages := make([]uint64, n)
	for i := range pages {
		e := uint64(0)
		for b := 7; b >= 0; b-- {
			e = e<<8 | uint64(raw[8*i+b])
		}
		// Bit 63: present; bits 0–54: the page frame, which reads 0 without CAP_SYS_ADMIN.
		frame := e & (1<<55 - 1)
		if e>>63 == 0 || frame == 0 {
			return nil, errors.New("pagemap shows no page frame (needs CAP_SYS_ADMIN)")
		}
		pages[i] = frame * uint64(page)
	}
	return pages, nil
}
