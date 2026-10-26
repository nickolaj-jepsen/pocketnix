// Package transport names the agent's listen and dial addresses, so the same
// agent and client code runs over vsock (on the phone), TCP or a unix socket
// (desktop development and tests).
//
// Addresses are "vsock:<port>" to listen, "vsock:<cid>:<port>" to dial,
// "tcp:<host>:<port>" and "unix:<path>".
package transport

import (
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/mdlayher/vsock"
)

// Listen opens a listener on addr. A vsock listener only accepts the host
// (CID 2): any other peer is a process inside the guest, which must not get
// the app's access to PTYs and forwards.
func Listen(addr string) (net.Listener, error) {
	scheme, rest, ok := strings.Cut(addr, ":")
	if !ok {
		return nil, fmt.Errorf("address %q has no scheme", addr)
	}
	switch scheme {
	case "vsock":
		port, err := strconv.ParseUint(rest, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("vsock port %q: %w", rest, err)
		}
		l, err := vsock.Listen(uint32(port), nil)
		if err != nil {
			return nil, err
		}
		return hostOnly{l}, nil
	case "tcp":
		return net.Listen("tcp", rest)
	case "unix":
		// A stale socket from a previous run would make bind fail.
		if fi, err := os.Lstat(rest); err == nil && fi.Mode()&os.ModeSocket != 0 {
			os.Remove(rest)
		}
		return net.Listen("unix", rest)
	default:
		return nil, fmt.Errorf("unknown address scheme %q", scheme)
	}
}

// Dial connects to addr.
func Dial(addr string) (net.Conn, error) {
	scheme, rest, ok := strings.Cut(addr, ":")
	if !ok {
		return nil, fmt.Errorf("address %q has no scheme", addr)
	}
	switch scheme {
	case "vsock":
		cidStr, portStr, ok := strings.Cut(rest, ":")
		if !ok {
			return nil, fmt.Errorf("vsock dial address %q wants <cid>:<port>", rest)
		}
		cid, err := strconv.ParseUint(cidStr, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("vsock cid %q: %w", cidStr, err)
		}
		port, err := strconv.ParseUint(portStr, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("vsock port %q: %w", portStr, err)
		}
		return vsock.Dial(uint32(cid), uint32(port), nil)
	case "tcp", "unix":
		return net.Dial(scheme, rest)
	default:
		return nil, fmt.Errorf("unknown address scheme %q", scheme)
	}
}

type hostOnly struct{ net.Listener }

func (l hostOnly) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if a, ok := c.RemoteAddr().(*vsock.Addr); ok && a.ContextID == vsock.Host {
			return c, nil
		}
		log.Printf("refusing vsock connection from %v: not the host", c.RemoteAddr())
		c.Close()
	}
}
