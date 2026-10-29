package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// The xdg-desktop-portal FileChooser backend: GUI and Flatpak apps' file
// dialogs become Android's document pickers, through pick and save.

const (
	portalBusName = "org.freedesktop.impl.portal.desktop.pocketnix"
	portalPath    = "/org/freedesktop/portal/desktop"
	portalIface   = "org.freedesktop.impl.portal.FileChooser"
	requestIface  = "org.freedesktop.impl.portal.Request"

	// Portal responses.
	responseOK        = 0
	responseCancelled = 1
	responseFailed    = 2

	// saveWait bounds the app's write of a file the user saved; matches the agent's pickWait.
	saveWait = 10 * time.Minute
	// saveSettle is how long a written file must stay untouched before it goes to Android.
	saveSettle = 500 * time.Millisecond
)

func portal(args []string) error {
	if len(args) != 0 {
		return errUsage
	}
	log.SetFlags(0)
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return err
	}
	p := &fileChooser{conn: conn}
	if err := conn.Export(p, portalPath, portalIface); err != nil {
		return err
	}
	r, err := conn.RequestName(portalBusName, dbus.NameFlagDoNotQueue)
	if err != nil {
		return err
	}
	if r != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("%s is taken", portalBusName)
	}
	log.Printf("serving %s on the session bus", portalIface)
	select {}
}

type fileChooser struct{ conn *dbus.Conn }

// request is a dialog's Request object, whose Close cancels it.
type request struct{ cancel context.CancelFunc }

func (r request) Close() *dbus.Error {
	r.cancel()
	return nil
}

// begin exports the dialog's Request object at handle.
func (p *fileChooser) begin(handle dbus.ObjectPath) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	if err := p.conn.Export(request{cancel}, handle, requestIface); err != nil {
		log.Printf("export %s: %v", handle, err)
	}
	return ctx, func() {
		cancel()
		p.conn.Export(nil, handle, requestIface)
	}
}

func (p *fileChooser) OpenFile(handle dbus.ObjectPath, appID, parent, title string, options map[string]dbus.Variant) (uint32, map[string]dbus.Variant, *dbus.Error) {
	results := map[string]dbus.Variant{}
	var multiple, directory bool
	if v, ok := options["multiple"]; ok {
		v.Store(&multiple)
	}
	if v, ok := options["directory"]; ok {
		v.Store(&directory)
	}
	if directory {
		log.Printf("%s asked for a directory, which Android's picker can't hand over", appID)
		return responseFailed, results, nil
	}
	ctx, end := p.begin(handle)
	defer end()
	reply, err := client.GuestCallContext(ctx, socket(), protocol.GuestRequest{Type: protocol.GuestPick, Types: filterTypes(options), Multiple: multiple})
	switch {
	case err != nil && ctx.Err() != nil, err == nil && reply.Cancelled:
		return responseCancelled, results, nil
	case err != nil:
		log.Printf("pick for %s: %v", appID, err)
		return responseFailed, results, nil
	}
	uris := make([]string, len(reply.Paths))
	for i, path := range reply.Paths {
		uris[i] = fileURI(path)
	}
	results["uris"] = dbus.MakeVariant(uris)
	results["writable"] = dbus.MakeVariant(true)
	return responseOK, results, nil
}

// SaveFile asks Android where to save first, then hands the app a path in
// the guest; once the app has written it, the file goes there.
func (p *fileChooser) SaveFile(handle dbus.ObjectPath, appID, parent, title string, options map[string]dbus.Variant) (uint32, map[string]dbus.Variant, *dbus.Error) {
	results := map[string]dbus.Variant{}
	name := saveName(options)
	ctx, end := p.begin(handle)
	defer end()
	reply, err := client.GuestCallContext(ctx, socket(), protocol.GuestRequest{Type: protocol.GuestSave, Name: name, MIME: saveType(name, options)})
	switch {
	case err != nil && ctx.Err() != nil, err == nil && reply.Cancelled:
		return responseCancelled, results, nil
	case err != nil:
		log.Printf("save for %s: %v", appID, err)
		return responseFailed, results, nil
	}
	path, watch, err := stage(reply.Ref, name)
	if err != nil {
		log.Printf("save for %s: %v", appID, err)
		return responseFailed, results, nil
	}
	go deliverSaved(reply.Ref, path, watch)
	results["uris"] = dbus.MakeVariant([]string{fileURI(path)})
	return responseOK, results, nil
}

func fileURI(path string) string { return (&url.URL{Scheme: "file", Path: path}).String() }

// filterTypes turns the dialog's filters (the current one, else all) into
// MIME types; none when a pattern has no type, which shows everything.
func filterTypes(options map[string]dbus.Variant) []string {
	type rule struct {
		Kind    uint32
		Pattern string
	}
	type filter struct {
		Name  string
		Rules []rule
	}
	var filters []filter
	var current filter
	if v, ok := options["current_filter"]; ok && v.Store(&current) == nil {
		filters = []filter{current}
	} else if v, ok := options["filters"]; ok {
		v.Store(&filters)
	}
	var types []string
	for _, f := range filters {
		for _, r := range f.Rules {
			t := r.Pattern
			if r.Kind == 0 {
				t = mime.TypeByExtension(strings.TrimPrefix(r.Pattern, "*"))
				if !strings.HasPrefix(r.Pattern, "*.") || t == "" {
					return nil
				}
			}
			if t, _, err := mime.ParseMediaType(t); err == nil {
				types = append(types, t)
			}
		}
	}
	return types
}

func saveName(options map[string]dbus.Variant) string {
	var name string
	if v, ok := options["current_name"]; ok {
		v.Store(&name)
	}
	var file []byte
	if v, ok := options["current_file"]; name == "" && ok && v.Store(&file) == nil {
		name = filepath.Base(string(bytes.TrimRight(file, "\x00")))
	}
	if name == "" || name == "." || name == "/" {
		name = "untitled"
	}
	return name
}

func saveType(name string, options map[string]dbus.Variant) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	if t := filterTypes(options); len(t) == 1 {
		return t[0]
	}
	return "application/octet-stream"
}

// stage makes a fresh directory for a save's file and watches it.
func stage(ref, name string) (string, *os.File, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Join(cache, "pocketnix", "saves", ref)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		return "", nil, err
	}
	watch := os.NewFile(uintptr(fd), "inotify")
	if _, err := syscall.InotifyAddWatch(fd, dir, syscall.IN_CLOSE_WRITE|syscall.IN_MOVED_TO); err != nil {
		watch.Close()
		return "", nil, err
	}
	return filepath.Join(dir, name), watch, nil
}

// deliverSaved waits for the app to write path, then sends it to the save
// ref. A file that doesn't make it stays where it is.
func deliverSaved(ref, path string, watch *os.File) {
	err := waitWritten(watch, filepath.Base(path))
	watch.Close()
	if err == nil {
		var f *os.File
		if f, err = os.Open(path); err == nil {
			_, err = client.GuestCall(socket(), protocol.GuestRequest{Type: protocol.GuestSaveData, Ref: ref}, f)
			f.Close()
		}
	}
	if err != nil {
		log.Printf("save %s: %v", path, err)
		return
	}
	os.RemoveAll(filepath.Dir(path))
}

// waitWritten returns once name was closed after a write, or renamed into
// place, and then left alone for saveSettle.
func waitWritten(watch *os.File, name string) error {
	deadline := time.Now().Add(saveWait)
	written := false
	buf := make([]byte, 64<<10)
	for {
		until := deadline
		if written {
			until = time.Now().Add(saveSettle)
		}
		watch.SetReadDeadline(until)
		n, err := watch.Read(buf)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			if written {
				return nil
			}
			return errors.New("the app never wrote it")
		}
		if err != nil {
			return err
		}
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			nameLen := int(binary.NativeEndian.Uint32(buf[off+12:]))
			start := off + syscall.SizeofInotifyEvent
			if start+nameLen > n {
				break
			}
			if string(bytes.TrimRight(buf[start:start+nameLen], "\x00")) == name {
				written = true
			}
			off = start + nameLen
		}
	}
}
