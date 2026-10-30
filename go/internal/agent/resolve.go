package agent

import (
	"fmt"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// resolveTimeout bounds resolving a path, which runs on the control loop: a
// mount that never answers (FUSE, a dead sshfs) would otherwise hold every
// later request. Shorter than the app's wait for an answer (ControlChannel.call),
// so it sees why.
const resolveTimeout = 3 * time.Second

// resolvePath answers a resolve-path request with the same check xdg-open
// gets: path, symlinks resolved, relative to the files root.
func (s *Server) resolvePath(path string) (protocol.Done, error) {
	type result struct {
		rel string
		err error
	}
	// Buffered: a resolve that answers after the timeout mustn't block forever.
	done := make(chan result, 1)
	go func() {
		rel, err := s.underFilesRoot(path)
		done <- result{rel, err}
	}()
	select {
	case r := <-done:
		return protocol.Done{Type: protocol.TypeResolvePath, Path: r.rel}, r.err
	case <-time.After(resolveTimeout):
		return protocol.Done{}, fmt.Errorf("%s didn't resolve within %s", path, resolveTimeout)
	}
}
