package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
)

// agentTransfer answers a guest's pick, save or clipboard request, as the
// app does after its picker or clipboard: events prints the refs.
func agentTransfer(c *client.Client, cmd string, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	ref, files := args[0], args[1:]
	ctl, err := c.Control()
	if err != nil {
		return err
	}
	defer ctl.Close()
	switch cmd {
	case "pick-answer":
		if len(files) == 0 {
			return ctl.GuestDone(ref, true, "")
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				ctl.GuestDone(ref, false, err.Error())
				return err
			}
			p, err := c.ShareFor(ref, filepath.Base(f), b)
			if err != nil {
				return err
			}
			fmt.Println(p)
		}
		return ctl.GuestDone(ref, false, "")
	case "save-answer":
		if len(files) == 0 {
			return ctl.GuestDone(ref, true, "")
		}
		if len(files) != 1 {
			return errUsage
		}
		return c.Fetch(ref, func(_ string, r io.Reader) error {
			b, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			return os.WriteFile(files[0], b, 0o644)
		})
	default: // paste-answer
		if len(files) != 1 {
			return ctl.GuestDone(ref, false, "nothing to paste")
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			ctl.GuestDone(ref, false, err.Error())
			return err
		}
		_, err = c.ShareFor(ref, filepath.Base(files[0]), b)
		return err
	}
}
