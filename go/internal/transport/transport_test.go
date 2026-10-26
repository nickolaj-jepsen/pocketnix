package transport

import (
	"net"
	"testing"

	"github.com/mdlayher/vsock"
)

// fakeListener hands out conns from peers at the given vsock CIDs.
type fakeListener struct {
	net.Listener
	cids   []uint32
	closed []bool
}

type fakeConn struct {
	net.Conn
	addr   net.Addr
	closed *bool
}

func (c fakeConn) RemoteAddr() net.Addr { return c.addr }
func (c fakeConn) Close() error         { *c.closed = true; return nil }

func (l *fakeListener) Accept() (net.Conn, error) {
	if len(l.cids) == 0 {
		return nil, net.ErrClosed
	}
	cid := l.cids[0]
	l.cids = l.cids[1:]
	l.closed = append(l.closed, false)
	return fakeConn{addr: &vsock.Addr{ContextID: cid, Port: 1234}, closed: &l.closed[len(l.closed)-1]}, nil
}

// A guest process (any CID but the host's) never gets the app's streams.
func TestHostOnlyRefusesGuestPeers(t *testing.T) {
	fl := &fakeListener{cids: []uint32{vsock.Local, 3, vsock.Host}, closed: make([]bool, 0, 3)}
	l := hostOnly{fl}
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if a := c.RemoteAddr().(*vsock.Addr); a.ContextID != vsock.Host {
		t.Fatalf("accepted CID %d", a.ContextID)
	}
	if !fl.closed[0] || !fl.closed[1] {
		t.Errorf("refused peers left open: %v", fl.closed)
	}
	if _, err := l.Accept(); err == nil {
		t.Error("accepted past the listener's end")
	}
}
