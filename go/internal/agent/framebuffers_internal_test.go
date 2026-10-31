package agent

import (
	"bytes"
	"io"
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPinFramesRefusesSizesOutOfRange(t *testing.T) {
	for _, n := range []int64{0, -1, maxFrameBuffers + 1} {
		if fb, err := pinFrames(n); err == nil {
			fb.close()
			t.Errorf("pinFrames(%d) succeeded", n)
		}
	}
}

func TestPinFramesListsEveryPage(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("pagemap shows page frames only to root")
	}
	page := int64(os.Getpagesize())
	fb, err := pinFrames(3*page + 1)
	if err != nil {
		t.Fatal(err)
	}
	defer fb.close()
	if len(fb.pages) != 4 {
		t.Fatalf("%d pages for 3 pages and a byte, want 4", len(fb.pages))
	}
	seen := map[uint64]bool{}
	for _, p := range fb.pages {
		if p%uint64(page) != 0 || seen[p] {
			t.Errorf("page address %#x: unaligned or repeated", p)
		}
		seen[p] = true
	}
}

func TestSendFdWritesAPayloadLargerThanTheSendBuffer(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "socketpair")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}
		return c.(*net.UnixConn)
	}
	sender, receiver := conn(fds[0]), conn(fds[1])
	defer receiver.Close()
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	sent := make(chan error, 1)
	go func() {
		defer sender.Close()
		sent <- sendFd(sender, payload, int(os.Stdin.Fd()))
	}()

	buf := make([]byte, len(payload))
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := receiver.ReadMsgUnix(buf, oob)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		t.Fatalf("control messages %v, %v: want the descriptor", msgs, err)
	}
	got, err := unix.ParseUnixRights(&msgs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range got {
		unix.Close(fd)
	}
	rest, err := io.ReadAll(receiver)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(buf[:n], rest...), payload) {
		t.Errorf("received %d bytes, want the %d sent", n+len(rest), len(payload))
	}
}
