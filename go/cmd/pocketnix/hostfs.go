package main

import (
	"flag"
	"fmt"
	"path/filepath"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// agentHostFS plays the app sharing one folder: it announces it, then
// answers the agent's file operations from dir.
func agentHostFS(c *client.Client, args []string) error {
	fs := flag.NewFlagSet("hostfs-serve", flag.ContinueOnError)
	name := fs.String("name", "", "")
	readOnly := fs.Bool("read-only", false, "")
	streams := fs.Int("streams", 2, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *streams < 1 {
		return errUsage
	}
	dir, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return err
	}
	if *name == "" {
		*name = filepath.Base(dir)
	}
	srv, err := client.NewHostFSServer(dir, !*readOnly)
	if err != nil {
		return err
	}
	defer srv.Close()
	ctl, err := c.Control()
	if err != nil {
		return err
	}
	defer ctl.Close()
	if err := ctl.HostFolders([]protocol.HostFolder{{Name: *name, Writable: !*readOnly}}); err != nil {
		return err
	}
	ended := make(chan error, *streams+1)
	for range *streams {
		conn, err := c.HostFS(*name)
		if err != nil {
			return err
		}
		defer conn.Close()
		go func() { ended <- srv.Serve(conn) }()
	}
	go func() {
		for range ctl.Configs() {
		}
		ended <- fmt.Errorf("control stream closed")
	}()
	if err := <-ended; err != nil {
		return err
	}
	return fmt.Errorf("the agent closed a stream")
}
