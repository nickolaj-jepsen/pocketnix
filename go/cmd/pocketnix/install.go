package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

const installUsage = `usage: pocketnix install [--serial <s>] [--debug] [--no-start] <apk>
       pocketnix install [--serial <s>] [--debug] [--no-start] --restore

Swaps the app's build and never touches the VM's disk: stops a running VM,
backs up the installed APK unless a backup exists, installs the new one over
it, re-grants the AVF permissions and starts the VM again if it was running.

flags:
  --restore          reinstall the backed-up APK and delete the backup; starts the VM
  --debug            target the debug build (dev.nickolaj.pocketnix.debug)
  --no-start         leave the VM stopped afterwards
  --serial <serial>  adb device to use (default: adb's only device)
`

type installOpts struct {
	apk, serial             string
	debug, noStart, restore bool
}

func parseInstall(args []string) (*installOpts, error) {
	o := &installOpts{}
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, installUsage) }
	fs.StringVar(&o.serial, "serial", "", "")
	fs.BoolVar(&o.debug, "debug", false, "")
	fs.BoolVar(&o.noStart, "no-start", false, "")
	fs.BoolVar(&o.restore, "restore", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, errUsage
		}
		return nil, err
	}
	switch {
	case o.restore && fs.NArg() == 0:
	case !o.restore && fs.NArg() == 1:
		o.apk = fs.Arg(0)
	default:
		fs.Usage()
		return nil, errUsage
	}
	return o, nil
}

func runInstall(args []string) error {
	if _, err := parseInstall(args); err != nil {
		return err
	}
	return errors.New("install: not implemented")
}
