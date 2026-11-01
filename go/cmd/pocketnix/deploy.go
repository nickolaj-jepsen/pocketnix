package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

const deployUsage = `usage: pocketnix deploy [--host <ssh host>] [--ssh-config <file>] [--flake <ref>] [--native] agent|compositor
       pocketnix deploy [--host <ssh host>] [--ssh-config <file>] --remove [agent|compositor]

Builds a component on this machine and runs it in a phone guest until the
guest reboots, through a runtime systemd drop-in; the system's own build
comes back on reboot or with --remove. Nothing is built in the guest.

flags:
  --host <ssh host>    the guest, as ssh names it (default phone-usb)
  --ssh-config <file>  an ssh config for every ssh and nix copy call
  --flake <ref>        the flake to build from (default: this checkout's git toplevel)
  --native             build the flake's own aarch64 package instead of cross-building
  --remove             end a deploy early (both components without one named)
`

const deployDefaultHost = "phone-usb"

type deployOpts struct {
	host, sshConfig, flake string
	native, remove         bool
	// components is what to deploy or remove: "agent", "compositor".
	components []string
}

func parseDeploy(args []string) (*deployOpts, error) {
	o := &deployOpts{}
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, deployUsage) }
	fs.StringVar(&o.host, "host", deployDefaultHost, "")
	fs.StringVar(&o.sshConfig, "ssh-config", "", "")
	fs.StringVar(&o.flake, "flake", "", "")
	fs.BoolVar(&o.native, "native", false, "")
	fs.BoolVar(&o.remove, "remove", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, errUsage
		}
		return nil, err
	}
	switch {
	case fs.NArg() == 1 && (fs.Arg(0) == "agent" || fs.Arg(0) == "compositor"):
		o.components = []string{fs.Arg(0)}
	case fs.NArg() == 0 && o.remove:
		o.components = []string{"agent", "compositor"}
	default:
		fs.Usage()
		return nil, errUsage
	}
	return o, nil
}

func runDeploy(args []string) error {
	if _, err := parseDeploy(args); err != nil {
		return err
	}
	return errors.New("deploy: not implemented")
}
