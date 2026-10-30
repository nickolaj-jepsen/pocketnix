package agent

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// audioReader is the one audio stream reading the pipe: a new one replaces it.
var audioReader struct {
	sync.Mutex
	stop func()
	gen  uint64
}

// serveAudio streams the guest's sound to the app: the raw PCM that
// PipeWire's pipe sink writes to the configured FIFO ([protocol.StreamAudio]).
func (s *Server) serveAudio(c net.Conn) {
	path := s.config().AudioPipe
	if path == "" {
		reject(c, "audio is not enabled")
		return
	}
	// Nonblocking, so opening doesn't wait for a writer and Go's poller serves reads.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err == nil {
		if fi, serr := f.Stat(); serr != nil || fi.Mode().Type() != os.ModeNamedPipe {
			f.Close()
			err = fmt.Errorf("%s is not a FIFO", path)
		}
	}
	if err != nil {
		reject(c, err.Error())
		return
	}
	defer f.Close()

	audioReader.Lock()
	if audioReader.stop != nil {
		audioReader.stop()
	}
	var once sync.Once
	stop := func() { once.Do(func() { f.Close(); c.Close() }) }
	audioReader.stop = stop
	audioReader.gen++
	gen := audioReader.gen
	audioReader.Unlock()
	defer func() {
		audioReader.Lock()
		if audioReader.gen == gen {
			audioReader.stop = nil
		}
		audioReader.Unlock()
		stop()
	}()

	drainPipe(f)
	if protocol.WriteJSON(c, s.hello()) != nil {
		return
	}
	// The app sends nothing: its end closing ends the stream, even while the guest is silent.
	go func() {
		io.Copy(io.Discard, c)
		stop()
	}()
	io.Copy(c, f)
}

// drainPipe discards what f holds now: sound written while no app listened.
func drainPipe(f *os.File) {
	rc, err := f.SyscallConn()
	if err != nil {
		return
	}
	buf := make([]byte, 64<<10)
	rc.Read(func(fd uintptr) bool {
		for {
			n, err := syscall.Read(int(fd), buf)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			// Empty (EAGAIN) or without a writer (0).
			if err != nil || n == 0 {
				return true
			}
		}
	})
}
