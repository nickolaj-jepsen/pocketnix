package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/adb"
)

// sshProxyUsage is a format for sshProxyDefaultPort.
const sshProxyUsage = `usage: pocketnix ssh-proxy [--serial <serial>] [--port <phone port>]

An ssh ProxyCommand reaching the guest's sshd over USB: adb forwards a free
local port to the app's port forward on the phone's 127.0.0.1, and stdin and
stdout are spliced to it. In ~/.ssh/config:

  Host phone-usb
    User <user>
    HostKeyAlias <host>
    ProxyCommand pocketnix ssh-proxy

then ssh phone-usb, pocketnix deploy, or a push from a desktop
(docs/updates.md, "Pushing from a desktop").

flags:
  --port <phone port>  the ssh forward's port on the phone, its
                       pocketnix.guest.forwards.<name>.hostPort (default %d,
                       a managed guest's and the example's)
  --serial <serial>    adb device to use (default: adb's only device)
`

// sshProxyDefaultPort is a managed guest's and the example's forwards.ssh.hostPort.
const sshProxyDefaultPort = 2022

func runSSHProxy(args []string) error {
	fs := flag.NewFlagSet("ssh-proxy", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, sshProxyUsage, sshProxyDefaultPort) }
	serial := fs.String("serial", "", "")
	port := fs.Int("port", sshProxyDefaultPort, "")
	if err := fs.Parse(args); err != nil {
		// flag has printed the error, or the usage for -h.
		return errUsage
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return errUsage
	}
	dev := adb.Device{Serial: *serial}
	out, err := dev.Run("forward", "tcp:0", "tcp:"+strconv.Itoa(*port))
	if err != nil {
		return err
	}
	local := strings.TrimSpace(out)
	if _, err := strconv.Atoi(local); err != nil {
		return fmt.Errorf("adb forward answered %q, not a port", local)
	}
	remove := sync.OnceFunc(func() { dev.Run("forward", "--remove", "tcp:"+local) })
	defer remove()
	// ssh ends its ProxyCommand with SIGHUP, which would skip the defer.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-sig
		remove()
		os.Exit(1)
	}()
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", local))
	if err != nil {
		return err
	}
	defer conn.Close()
	return spliceStdio(conn)
}
