package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

const (
	// PortPollInterval is how often the port watcher rescans /proc without hints.
	PortPollInterval = 2 * time.Second
	// portRescanDebounce gives a hinted listener time to reach /proc/net/tcp
	// (a hint comes before the port is hashed) and folds a burst into one scan.
	portRescanDebounce = 100 * time.Millisecond
	// tcpListen is TCP_LISTEN, the st column of /proc/net/tcp.
	tcpListen = 0x0A
)

// PortHints wakes the port watcher when the guest's TCP listeners may have
// changed. String names it in the log.
type PortHints interface {
	// Wait blocks until the next hint, and errors once closed.
	Wait() error
	Close() error
	fmt.Stringer
}

// portWatch is the forwardOffers state: the watcher, its latest scan and
// the snapshot the app last heard.
type portWatch struct {
	mu       sync.Mutex
	watching bool
	load     func() (PortHints, error)
	// stop ends the running watcher; nil while offers are off.
	stop context.CancelFunc
	// gen tells a stopped watcher's late scan from the running one's.
	gen int
	// uid is the session user's; -1 when unknown.
	uid int
	// socks is the running watcher's latest scan, LISTEN sockets only; nil before it.
	socks []socket
	// own counts the agent's TCP listeners per port, each being served once per count.
	own  map[int]int
	last *protocol.PortListeners
}

// WatchPorts follows the guest's TCP listeners whenever the config's
// forwardOffers is on, from now and across reloads, and pushes them to every
// control stream. Unless load is nil or fails, its hints say when to rescan;
// otherwise /proc is polled every PortPoll.
func (s *Server) WatchPorts(load func() (PortHints, error)) {
	s.ports.mu.Lock()
	s.ports.watching, s.ports.load = true, load
	s.ports.mu.Unlock()
	s.portsConfigured()
}

// portsConfigured follows a new config: it resolves the session user's uid,
// starts or stops the watcher, and pushes what the new filter leaves.
func (s *Server) portsConfigured() {
	p := &s.ports
	p.mu.Lock()
	cfg := s.config()
	on := p.watching && cfg.ForwardOffers.Enable
	p.uid = -1
	if u, err := sessionUser(cfg.User); err == nil {
		p.uid = u.uid
	} else if on {
		log.Printf("port offers: %v", err)
	}
	switch {
	case on && p.stop == nil:
		var ctx context.Context
		ctx, p.stop = context.WithCancel(context.Background())
		p.gen++
		go s.watchPorts(ctx, p.gen, p.load)
	case !on && p.stop != nil:
		p.stop()
		p.stop, p.socks, p.last = nil, nil, nil
		p.gen++
	}
	p.mu.Unlock()
	s.portsChanged()
}

func (s *Server) watchPorts(ctx context.Context, gen int, load func() (PortHints, error)) {
	poll := s.PortPoll
	if poll <= 0 {
		poll = PortPollInterval
	}
	var tick <-chan time.Time
	polling := func() {
		t := time.NewTicker(poll)
		context.AfterFunc(ctx, func() { t.Stop() })
		tick = t.C
	}
	wake := make(chan struct{}, 1)
	ended := make(chan error, 1)
	var hints PortHints
	err := errors.New("forced")
	if load != nil {
		hints, err = load()
	}
	if err == nil {
		log.Printf("port watcher: %v", hints)
		context.AfterFunc(ctx, func() { hints.Close() })
		go func() {
			for {
				if err := hints.Wait(); err != nil {
					ended <- err
					return
				}
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}()
	} else {
		log.Printf("port watcher: polling /proc every %v (%v)", poll, err)
		polling()
	}
	var lastErr string
	for {
		socks, err := scanSockets(s.procNet())
		if err != nil {
			if err.Error() != lastErr {
				log.Printf("port watcher: %v", err)
			}
			lastErr = err.Error()
		} else {
			lastErr = ""
			s.portsScanned(gen, socks)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		case err := <-ended:
			// Offers turned off close the hints too.
			if ctx.Err() != nil {
				return
			}
			log.Printf("port watcher: %v ended: %v; polling /proc every %v", hints, err, poll)
			polling()
		case <-wake:
			// A hint during the wait stays queued: its socket may reach /proc only after this scan.
			select {
			case <-ctx.Done():
				return
			case <-time.After(portRescanDebounce):
			}
		}
	}
}

func (s *Server) procNet() string {
	if s.ProcNet == "" {
		return "/proc/net"
	}
	return s.ProcNet
}

func (s *Server) portsScanned(gen int, socks []socket) {
	s.ports.mu.Lock()
	if gen != s.ports.gen {
		s.ports.mu.Unlock()
		return
	}
	s.ports.socks = socks
	s.ports.mu.Unlock()
	s.portsChanged()
}

// ownPort counts one more (d 1) or one fewer (d -1) agent listener on port.
func (s *Server) ownPort(port, d int) {
	s.ports.mu.Lock()
	if s.ports.own == nil {
		s.ports.own = map[int]int{}
	}
	if s.ports.own[port] += d; s.ports.own[port] <= 0 {
		delete(s.ports.own, port)
	}
	s.ports.mu.Unlock()
	s.portsChanged()
}

// portsChanged pushes the snapshot if it differs from the last one pushed:
// after a scan, or a config or listener of the agent's that filters it
// differently.
func (s *Server) portsChanged() {
	p := &s.ports
	p.mu.Lock()
	if p.stop == nil || p.socks == nil {
		p.mu.Unlock()
		return
	}
	cfg := s.config()
	snap := protocol.PortListeners{LAN: cfg.ForwardOffers.LAN, Ports: offerable(p.socks, p.uid, cfg.Forwards, p.own)}
	if p.last != nil && p.last.LAN == snap.LAN && slices.Equal(p.last.Ports, snap.Ports) {
		p.mu.Unlock()
		return
	}
	p.last = &snap
	p.mu.Unlock()
	log.Printf("port offers: %v", snap.Ports)
	s.pushLatest(func() any {
		if snap, ok := s.portSnapshot(); ok {
			return snap
		}
		return nil
	})
}

// portSnapshot is the snapshot for a control stream, false while offers
// are off. Before the first scan its set is empty.
func (s *Server) portSnapshot() (protocol.PortListeners, bool) {
	p := &s.ports
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stop == nil {
		return protocol.PortListeners{}, false
	}
	if p.last != nil {
		return *p.last, true
	}
	return protocol.PortListeners{LAN: s.config().ForwardOffers.LAN}, true
}

// socket is a line of /proc/net/tcp or tcp6.
type socket struct {
	local netip.AddrPort
	state int
	uid   int
}

// dialable is where serveForward's dial reaches a listener.
var dialable = []netip.Addr{
	netip.IPv4Unspecified(),
	netip.IPv6Unspecified(),
	netip.MustParseAddr("127.0.0.1"),
	netip.MustParseAddr("::ffff:127.0.0.1"),
	netip.IPv6Loopback(),
}

// owned maps each port that has a LISTEN socket on a dialable address to
// whether uid owns every such socket. A forward dials by port, so one other
// owner may be the one that answers.
func owned(socks []socket, uid int) map[int]bool {
	ports := map[int]bool{}
	for _, sk := range socks {
		if sk.state != tcpListen || !slices.Contains(dialable, sk.local.Addr()) {
			continue
		}
		port := int(sk.local.Port())
		mine, seen := ports[port]
		ports[port] = sk.uid == uid && (mine || !seen)
	}
	return ports
}

// offerable is the set forwardOffers pushes: the ports, ascending and
// unique, whose dialable LISTEN sockets uid owns all of, from 1024 up, that
// are neither a module TCP forward's guest port nor the agent's own.
func offerable(socks []socket, uid int, forwards []protocol.Forward, own map[int]int) []int {
	ports := []int{}
	for port, mine := range owned(socks, uid) {
		if !mine || port < 1024 || own[port] > 0 || moduleForward(forwards, port) {
			continue
		}
		ports = append(ports, port)
	}
	slices.Sort(ports)
	return ports
}

func moduleForward(forwards []protocol.Forward, port int) bool {
	return slices.ContainsFunc(forwards, func(f protocol.Forward) bool {
		return f.GuestPort == port && f.Protocol == protocol.ProtocolTCP
	})
}

// sessionListener errs unless the session user owns every dialable
// listener on port, read from /proc now: a forward the module didn't
// declare reaches only the session user's servers.
func (s *Server) sessionListener(port int) error {
	socks, err := scanSockets(s.procNet())
	if err != nil {
		return err
	}
	s.ports.mu.Lock()
	uid := s.ports.uid
	s.ports.mu.Unlock()
	mine, listening := owned(socks, uid)[port]
	switch {
	case !listening:
		return fmt.Errorf("nothing listens on port %d", port)
	case !mine:
		return fmt.Errorf("port %d has a listener that isn't the session user's, and isn't a module forward", port)
	}
	return nil
}

// scanSockets reads the LISTEN sockets of dir's tcp and tcp6 (absent
// without IPv6), never nil.
func scanSockets(dir string) ([]socket, error) {
	socks := []socket{}
	for _, name := range []string{"tcp", "tcp6"} {
		f, err := os.Open(filepath.Join(dir, name))
		if name == "tcp6" && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, sk := range parseProcNet(f) {
			if sk.state == tcpListen {
				socks = append(socks, sk)
			}
		}
		f.Close()
	}
	return socks, nil
}

// parseProcNet reads /proc/net/tcp or tcp6, skipping the header and any
// line it can't parse.
func parseProcNet(r io.Reader) []socket {
	var socks []socket
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		// sl local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid …
		f := strings.Fields(sc.Text())
		if len(f) < 8 {
			continue
		}
		local, ok := parseHexAddrPort(f[1])
		state, err1 := strconv.ParseUint(f[3], 16, 8)
		uid, err2 := strconv.Atoi(f[7])
		if !ok || err1 != nil || err2 != nil {
			continue
		}
		socks = append(socks, socket{local: local, state: int(state), uid: uid})
	}
	return socks
}

// parseHexAddrPort parses /proc/net/tcp's ADDR:PORT, where the kernel prints
// each 32-bit word of the address in host order and the port in hex.
func parseHexAddrPort(s string) (netip.AddrPort, bool) {
	a, p, _ := strings.Cut(s, ":")
	port, err := strconv.ParseUint(p, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	b, err := hex.DecodeString(a)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return netip.AddrPort{}, false
	}
	for i := 0; i < len(b); i += 4 {
		binary.NativeEndian.PutUint32(b[i:], binary.BigEndian.Uint32(b[i:]))
	}
	addr, _ := netip.AddrFromSlice(b)
	return netip.AddrPortFrom(addr, uint16(port)), true
}
