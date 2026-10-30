package agent

import "github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"

// resolvePath answers a resolve-path request with the same check xdg-open
// gets: path, symlinks resolved, relative to the files root.
func (s *Server) resolvePath(path string) (protocol.Done, error) {
	rel, err := s.underFilesRoot(path)
	return protocol.Done{Type: protocol.TypeResolvePath, Path: rel}, err
}
