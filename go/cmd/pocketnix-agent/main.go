// Command pocketnix-agent is the guest side of pocketnix: it serves the app's
// control stream over vsock.
package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net"

	"github.com/mdlayher/vsock"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

var version = "dev"

func main() {
	l, err := vsock.Listen(protocol.ControlPort, nil)
	if err != nil {
		log.Fatalf("listen on vsock port %d: %v", protocol.ControlPort, err)
	}
	log.Printf("pocketnix-agent %s on vsock port %d", version, protocol.ControlPort)
	for {
		c, err := l.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go serveControl(c)
	}
}

func serveControl(c net.Conn) {
	defer c.Close()
	enc := json.NewEncoder(c)
	if err := enc.Encode(protocol.Hello{Type: "hello", Version: protocol.Version, Agent: version}); err != nil {
		log.Printf("send hello: %v", err)
		return
	}
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		var msg struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			log.Printf("bad message: %v", err)
			continue
		}
		log.Printf("unhandled message type %q", msg.Type)
	}
}
