package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

const doctorUsage = `usage: pocketnix doctor [--serial <s>] [--debug] [--json] [--smoke] [--gpu-apps <id>,<id>] [--record <dir>]

Probes a phone for what pocketnix needs (AVF, its kernel, u-boot, the app)
and prints a verdict: ok, warn or fail. It exits 1 only on fail, which means
the phone definitely can't run the VM.

flags:
  --json             print the report as JSON, for a device report
  --debug            look at the debug build (dev.nickolaj.pocketnix.debug)
  --smoke            also run the end-to-end checks against the debug build's
                     running VM (implies --debug; the phone unlocked, pocketnix in front)
  --gpu-apps <ids>   two app ids from pocketnix control dump's apps=, for the GPU step
  --record <dir>     record the run (screen, logcat, commands) into a bundle under dir
  --serial <serial>  adb device to use (default: adb's only device)
`

type doctorOpts struct {
	serial, record     string
	debug, json, smoke bool
	gpuApps            []string
}

func parseDoctor(args []string) (*doctorOpts, error) {
	o := &doctorOpts{}
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, doctorUsage) }
	fs.StringVar(&o.serial, "serial", "", "")
	fs.BoolVar(&o.debug, "debug", false, "")
	fs.BoolVar(&o.json, "json", false, "")
	fs.BoolVar(&o.smoke, "smoke", false, "")
	fs.StringVar(&o.record, "record", "", "")
	fs.Func("gpu-apps", "", func(v string) error {
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id != "" {
				o.gpuApps = append(o.gpuApps, id)
			}
		}
		return nil
	})
	if err := fs.Parse(args); err != nil {
		// flag has printed the error, or the usage for -h.
		return nil, errUsage
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return nil, errUsage
	}
	if o.smoke {
		o.debug = true
	}
	return o, nil
}

func runDoctor(args []string) error {
	if _, err := parseDoctor(args); err != nil {
		return err
	}
	return errors.New("doctor: not implemented")
}
