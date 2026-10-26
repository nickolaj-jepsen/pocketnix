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

const sshProxyUsage = `usage: pocketnix ssh-proxy [--serial <serial>] [--port <phone port>]

An ssh ProxyCommand reaching the guest's sshd over USB: adb forwards a free
local port to the app's port forward on the phone's 127.0.0.1, and stdin and
stdout are spliced to it. For a desktop push, in ~/.ssh/config:

  Host phone-usb
    HostKeyAlias <host>
    ProxyCommand pocketnix ssh-proxy --port 2222

then nixos-rebuild switch --flake .#<host> --target-host <user>@phone-usb
`

func runSSHProxy(args []string) error {
	fs := flag.NewFlagSet("ssh-proxy", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, sshProxyUsage) }
	serial := fs.String("serial", "", "adb device serial (default: adb's only device)")
	port := fs.Int("port", 2222, "the ssh forward's port on the phone (pocketnix.guest.forwards.<name>.hostPort)")
	if err := fs.Parse(args); err != nil {
		return err
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
