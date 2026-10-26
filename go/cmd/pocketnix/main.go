// Command pocketnix is the host-side CLI: provisioning a phone over adb and,
// later, an ssh ProxyCommand into the guest.
package main

import (
	"fmt"
	"os"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

var version = "dev"

const usage = `usage: pocketnix <command>

commands:
  provision   install the app, grant AVF permissions, push and boot an image (not implemented yet)
  version     print the CLI and protocol versions
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Printf("pocketnix %s (protocol %d)\n", version, protocol.Version)
	case "provision":
		fmt.Fprintln(os.Stderr, "provision: not implemented yet (milestone M0)")
		os.Exit(1)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}
