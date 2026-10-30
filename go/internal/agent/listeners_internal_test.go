package agent

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// From a little-endian kernel, which prints each address word in host order.
const (
	procTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 9103 1 0000000000000000 100 0 0 10 0
   1: 00000000:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 9104 1 0000000000000000 100 0 0 10 0
   2: 0500000A:1F92 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 9105 1 0000000000000000 100 0 0 10 0
   3: 0100007F:03FF 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 9106 1 0000000000000000 100 0 0 10 0
   4: 0100007F:1FA4 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 9107 1 0000000000000000 100 0 0 10 0
   5: 0100007F:1F90 0100007F:C350 01 00000000:00000000 00:00000000 00000000  1000        0 9108 1 0000000000000000 20 4 30 10 -1
   6: 0100007F:1F96 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 9109 1 0000000000000000 100 0 0 10 0
   7: 0100007F:238C 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 9110 1 0000000000000000 100 0 0 10 0
`
	procTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1F91 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 9201 1 0000000000000000 100 0 0 10 0
   1: 0000000000000000FFFF00000100007F:1F93 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 9202 1 0000000000000000 100 0 0 10 0
   2: 00000000000000000000000000000000:0BB8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 9203 1 0000000000000000 100 0 0 10 0
`
)

func TestParseProcNet(t *testing.T) {
	socks := slices.Concat(parseProcNet(strings.NewReader(procTCP)), parseProcNet(strings.NewReader(procTCP6)))
	for _, want := range []socket{
		{local: netip.MustParseAddrPort("127.0.0.1:8080"), state: tcpListen, uid: 1000},
		{local: netip.MustParseAddrPort("0.0.0.0:3000"), state: tcpListen, uid: 1000},
		{local: netip.MustParseAddrPort("10.0.0.5:8082"), state: tcpListen, uid: 1000},
		{local: netip.MustParseAddrPort("127.0.0.1:8100"), state: tcpListen, uid: 0},
		{local: netip.MustParseAddrPort("127.0.0.1:8080"), state: 1, uid: 1000},
		{local: netip.MustParseAddrPort("[::1]:8081"), state: tcpListen, uid: 1000},
		{local: netip.MustParseAddrPort("[::ffff:127.0.0.1]:8083"), state: tcpListen, uid: 1000},
		{local: netip.MustParseAddrPort("[::]:3000"), state: tcpListen, uid: 1000},
	} {
		if !slices.Contains(socks, want) {
			t.Errorf("no %+v in %+v", want, socks)
		}
	}
}

func TestOfferable(t *testing.T) {
	socks := slices.Concat(parseProcNet(strings.NewReader(procTCP)), parseProcNet(strings.NewReader(procTCP6)))
	forwards := []protocol.Forward{
		{Name: "web", GuestPort: 8086, Protocol: protocol.ProtocolTCP},
		// A UDP forward leaves its TCP port alone.
		{Name: "dns", GuestPort: 3000, Protocol: protocol.ProtocolUDP},
	}
	own := map[int]int{9100: 1}
	got := offerable(socks, 1000, forwards, own)
	// Not 10.0.0.5:8082 (unreachable), 1023, root's 8100, the forward's 8086 or the agent's 9100.
	if want := []int{3000, 8080, 8081, 8083}; !slices.Equal(got, want) {
		t.Errorf("offerable = %v, want %v", got, want)
	}
	if got := offerable(socks, 0, forwards, own); !slices.Equal(got, []int{8100}) {
		t.Errorf("root's offerable = %v, want [8100]", got)
	}
}
