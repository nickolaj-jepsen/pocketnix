// Command pocketnix is the host-side CLI: provisioning a phone over adb, and
// talking to a guest agent directly for debugging.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

var version = "dev"

const usage = `usage: pocketnix <command> [args]

commands:
  provision   install the app, grant AVF permissions, push and boot an image
  agent       talk to a guest agent directly (debugging; see pocketnix agent -h)
  ssh-proxy   an ssh ProxyCommand to the guest's sshd over adb
  control     drive a debug build's tabs over adb and read them back
  version     print the CLI and protocol versions
`

var errUsage = errors.New("usage")

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "version":
		fmt.Printf("pocketnix %s (protocol %d)\n", version, protocol.Version)
	case "provision":
		err = runProvision(os.Args[2:])
	case "agent":
		err = runAgent(os.Args[2:])
	case "ssh-proxy":
		err = runSSHProxy(os.Args[2:])
	case "control":
		err = runControl(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var status exitStatus
	switch {
	case err == nil:
	case errors.Is(err, errUsage):
		os.Exit(2)
	case errors.As(err, &status):
		os.Exit(int(status) & 0xff)
	default:
		fmt.Fprintln(os.Stderr, "pocketnix:", err)
		os.Exit(1)
	}
}
