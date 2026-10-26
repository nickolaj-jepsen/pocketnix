package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

var errCancelled = errors.New("cancelled")

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func call(req protocol.GuestRequest, files ...*os.File) (protocol.GuestReply, error) {
	reply, err := client.GuestCall(socket(), req, files...)
	if err == nil && reply.Cancelled {
		err = errCancelled
	}
	return reply, err
}

// share offers files, or text (--text: the arguments, else stdin), to Android's share sheet.
func share(args []string) error {
	fs := flags("share")
	text := fs.Bool("text", false, "")
	if fs.Parse(args) != nil {
		return errUsage
	}
	req := protocol.GuestRequest{Type: protocol.GuestShare}
	if *text {
		req.Text = strings.Join(fs.Args(), " ")
		if fs.NArg() == 0 {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			req.Text = string(b)
		}
	} else {
		if fs.NArg() == 0 {
			return errUsage
		}
		for _, p := range fs.Args() {
			abs, err := filepath.Abs(p)
			if err != nil {
				return err
			}
			req.Paths = append(req.Paths, abs)
		}
	}
	_, err := call(req)
	return err
}

// pick asks for files on Android and prints where they landed: the share
// directory, or --dest.
func pick(args []string) error {
	fs := flags("pick")
	var types listFlag
	fs.Var(&types, "type", "")
	multiple := fs.Bool("multiple", false, "")
	dest := fs.String("dest", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return errUsage
	}
	reply, err := call(protocol.GuestRequest{Type: protocol.GuestPick, Types: types, Multiple: *multiple})
	if err != nil {
		return err
	}
	for i, p := range reply.Paths {
		if *dest != "" {
			name := filepath.Base(p)
			if i < len(reply.Names) {
				name = reply.Names[i]
			}
			if p, err = moveInto(p, *dest, name); err != nil {
				return err
			}
		}
		fmt.Println(p)
	}
	return nil
}

// moveInto moves file into dir under a free name like name, never
// replacing a file there.
func moveInto(file, dir, name string) (string, error) {
	base := filepath.Base(name)
	stem, ext := base, ""
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		stem, ext = base[:i], base[i:]
	}
	for n := 0; n < 1000; n++ {
		target := filepath.Join(dir, base)
		if n > 0 {
			target = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
		}
		err := os.Link(file, target)
		if errors.Is(err, syscall.EXDEV) {
			err = copyExcl(file, target)
		}
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return target, os.Remove(file)
	}
	return "", fmt.Errorf("no free name like %s in %s", base, dir)
}

func copyExcl(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(to)
	}
	return err
}

// save stores a file on Android, where the user picks.
func save(args []string) error {
	fs := flags("save")
	name := fs.String("name", "", "")
	typ := fs.String("type", "", "")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		return errUsage
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	if *name == "" {
		*name = filepath.Base(fs.Arg(0))
	}
	if *typ == "" {
		*typ = mimeType(*name)
	}
	_, err = call(protocol.GuestRequest{Type: protocol.GuestSave, Name: *name, MIME: *typ}, f)
	return err
}

func mimeType(name string) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// typeFlag takes a leading --type <t> (or --type=<t>) off args.
func typeFlag(args []string) (string, []string) {
	if len(args) >= 2 && (args[0] == "--type" || args[0] == "-t") {
		return args[1], args[2:]
	}
	if len(args) >= 1 && strings.HasPrefix(args[0], "--type=") {
		return strings.TrimPrefix(args[0], "--type="), args[1:]
	}
	return "", args
}

func isText(t string) bool {
	m, params, err := mime.ParseMediaType(t)
	return err == nil && m == "text/plain" && (params["charset"] == "" || strings.EqualFold(params["charset"], "utf-8"))
}

// copyTyped hands Android stdin's bytes as a clipboard of type t.
func copyTyped(t string, args []string) error {
	if len(args) != 0 {
		return errUsage
	}
	_, err := call(protocol.GuestRequest{Type: protocol.GuestCopy, MIME: t}, os.Stdin)
	return err
}

// pasteTyped writes the clipboard's data of type t to stdout.
func pasteTyped(t string, args []string) error {
	if len(args) != 0 {
		return errUsage
	}
	_, err := call(protocol.GuestRequest{Type: protocol.GuestPaste, MIME: t}, os.Stdout)
	return err
}

func listTypes() error {
	reply, err := call(protocol.GuestRequest{Type: protocol.GuestClipboardTypes})
	for _, t := range reply.Types {
		fmt.Println(t)
	}
	return err
}
