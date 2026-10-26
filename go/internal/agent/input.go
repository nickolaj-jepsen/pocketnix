package agent

import (
	"fmt"
	"log"
	"os"
	"syscall"
	"unsafe"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// Input is a virtual input device: pointer and keyboard events from the app
// become evdev events through /dev/uinput, so the guest's compositor treats
// them as a real mouse and keyboard (no protocol extensions needed).
type Input struct {
	f *os.File
}

// linux/uinput.h and input-event-codes.h; the guest kernel is Linux.
const (
	evSyn = 0
	evKey = 1
	evRel = 2
	evAbs = 3

	absX = 0
	absY = 1

	relHWheel = 6
	relWheel  = 8

	synReport = 0
	btnLeft   = 0x110
	btnRight  = 0x111

	uiDevCreate  = 0x5501
	uiDevDestroy = 0x5502
	uiSetEvBit   = 0x40045564
	uiSetKeyBit  = 0x40045565
	uiSetRelBit  = 0x40045566
	uiSetAbsBit  = 0x40045567
	uiDevSetup   = 0x401c5503 // _IOW('U', 3, struct uinput_setup), 92 bytes
	uiAbsSetup   = 0x401c5504 // _IOW('U', 4, struct uinput_abs_setup), 28 bytes

	busVirtual = 0x06
)

type uinputSetup struct {
	ID struct {
		Bus, Vendor, Product, Version uint16
	}
	Name [80]byte
	FF   uint32
}

type inputAbsInfo struct {
	Value, Minimum, Maximum, Fuzz, Flat, Resolution int32
}

type uinputAbsSetup struct {
	Code uint16
	Pad  uint16
	Info inputAbsInfo
}

type inputEvent struct {
	Sec, Usec int64
	Type      uint16
	Code      uint16
	Value     int32
}

// OpenInput creates the virtual device. Failure is not fatal: a guest without
// /dev/uinput streams the screen, just without input.
func OpenInput(path string) (*Input, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	in := &Input{f: f}
	steps := []struct {
		req uintptr
		arg int
	}{
		{uiSetEvBit, evKey}, {uiSetEvBit, evAbs}, {uiSetEvBit, evRel},
		{uiSetKeyBit, btnLeft}, {uiSetKeyBit, btnRight},
		{uiSetRelBit, relWheel}, {uiSetRelBit, relHWheel},
		{uiSetAbsBit, absX}, {uiSetAbsBit, absY},
	}
	for _, s := range steps {
		if err := ioctl(f.Fd(), s.req, uintptr(s.arg)); err != nil {
			f.Close()
			return nil, fmt.Errorf("uinput bit %#x: %w", s.req, err)
		}
	}
	for _, code := range []uint16{absX, absY} {
		setup := uinputAbsSetup{Code: code, Info: inputAbsInfo{Minimum: 0, Maximum: 65535}}
		if err := ioctl(f.Fd(), uiAbsSetup, uintptr(unsafe.Pointer(&setup))); err != nil {
			f.Close()
			return nil, fmt.Errorf("uinput abs %d: %w", code, err)
		}
	}
	var setup uinputSetup
	setup.ID.Bus, setup.ID.Vendor, setup.ID.Product = busVirtual, 0x1209, 0x0001
	copy(setup.Name[:], "pocketnix")
	if err := ioctl(f.Fd(), uiDevSetup, uintptr(unsafe.Pointer(&setup))); err != nil {
		f.Close()
		return nil, fmt.Errorf("uinput setup: %w", err)
	}
	if err := ioctl(f.Fd(), uiDevCreate, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("uinput create: %w", err)
	}
	return in, nil
}

func ioctl(fd uintptr, req, arg uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); errno != 0 {
		return errno
	}
	return nil
}

func (in *Input) emit(typ, code uint16, value int32) {
	ev := inputEvent{Type: typ, Code: code, Value: value}
	_, _ = in.f.Write(unsafe.Slice((*byte)(unsafe.Pointer(&ev)), int(unsafe.Sizeof(ev))))
}

func (in *Input) sync() { in.emit(evSyn, synReport, 0) }

// Apply performs one FrameDisplayInput event.
func (in *Input) Apply(kind byte, a, b int32) {
	if in == nil {
		return
	}
	switch kind {
	case protocol.InputPointer:
		in.emit(evAbs, absX, clampAbs(a))
		in.emit(evAbs, absY, clampAbs(b))
		in.sync()
	case protocol.InputButton:
		code := uint16(a)
		if code == 0 {
			code = btnLeft
		}
		in.emit(evKey, code, boolValue(b))
		in.sync()
	case protocol.InputWheel:
		code := uint16(relWheel)
		if a == 1 {
			code = relHWheel
		}
		in.emit(evRel, code, b)
		in.sync()
	case protocol.InputKey:
		in.emit(evKey, uint16(a), boolValue(b))
		in.sync()
	default:
		log.Printf("input: unknown event kind %d", kind)
	}
}

func (in *Input) Close() error { return in.f.Close() }

func clampAbs(v int32) int32 {
	if v < 0 {
		return 0
	}
	if v > 65535 {
		return 65535
	}
	return v
}

func boolValue(b int32) int32 {
	if b != 0 {
		return 1
	}
	return 0
}
