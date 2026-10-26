package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// Transfers (protocol/transfers.go): guest requests that wait on the app,
// a picker the user answers or bytes it fetches or sends.

const (
	// pickWait bounds a pick or a save: the user may take a while.
	pickWait = 10 * time.Minute
	// copyWait bounds the app's fetch of a typed copy, and pasteWait its send.
	copyWait  = 30 * time.Second
	pasteWait = 2 * time.Minute
	// maxClipData bounds typed clipboard data, which the agent keeps for a guest paste.
	maxClipData = 32 << 20
	// maxGuestFDs bounds the descriptors one guest read takes; the kernel closes the rest.
	maxGuestFDs = 4
)

var errCancelled = errors.New("cancelled")

// transfer is one guest request waiting on the app, named by its ref.
type transfer struct {
	kind       string // the guest request's type
	name, mime string
	// src brings the bytes the app fetches (a save, a typed copy); a deferred save's come late.
	src chan *io.SectionReader
	// sink takes the bytes the app sends for a paste.
	sink io.Writer
	// chosen closes when the app dials for a save's bytes: the user picked where.
	chosen     chan struct{}
	chooseOnce sync.Once
	// ended closes when the transfer is dropped, ending streams that wait on it.
	ended chan struct{}
	done  chan error

	mu           sync.Mutex
	paths, names []string // a pick's delivered files, and the names they came with
}

// finish settles the transfer; only the first outcome counts.
func (t *transfer) finish(err error) {
	select {
	case t.done <- err:
	default:
	}
}

func (t *transfer) choose() { t.chooseOnce.Do(func() { close(t.chosen) }) }

// wait returns the transfer's outcome, or why it ended without one.
func (t *transfer) wait(ctx context.Context) error {
	select {
	case err := <-t.done:
		return err
	case <-ctx.Done():
		return waitError(ctx)
	}
}

func waitError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("the app didn't answer in time")
	}
	return ctx.Err()
}

// transfers is the agent's open transfers and its typed clipboard.
type transfers struct {
	mu sync.Mutex
	m  map[string]*transfer
	// clipTypes are the clipboard's MIME types; clipData is a guest's typed
	// copy (of clipMIME), which a guest paste answers without the app.
	clipTypes []string
	clipMIME  string
	clipData  []byte
}

func (s *Server) newTransfer(kind string) (string, *transfer) {
	var b [8]byte
	rand.Read(b[:])
	ref := hex.EncodeToString(b[:])
	t := &transfer{
		kind: kind, src: make(chan *io.SectionReader, 1),
		chosen: make(chan struct{}), ended: make(chan struct{}), done: make(chan error, 1),
	}
	x := &s.transfers
	x.mu.Lock()
	if x.m == nil {
		x.m = map[string]*transfer{}
	}
	x.m[ref] = t
	x.mu.Unlock()
	return ref, t
}

func (s *Server) transfer(ref string) *transfer {
	x := &s.transfers
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.m[ref]
}

func (s *Server) dropTransfer(ref string) {
	x := &s.transfers
	x.mu.Lock()
	t := x.m[ref]
	delete(x.m, ref)
	x.mu.Unlock()
	if t != nil {
		close(t.ended)
	}
}

// setClipboardTypes records the clipboard's types after a text copy (types
// nil) or Android's clipboard arrived, forgetting a guest's typed copy.
func (s *Server) setClipboardTypes(types []string, text string) {
	if types == nil && text != "" {
		types = []string{"text/plain"}
	}
	x := &s.transfers
	x.mu.Lock()
	x.clipTypes, x.clipMIME, x.clipData = types, "", nil
	x.mu.Unlock()
}

// isTransfer reports whether req is one that runTransfer serves.
func isTransfer(req protocol.GuestRequest) bool {
	switch req.Type {
	case protocol.GuestShare, protocol.GuestPick, protocol.GuestSave, protocol.GuestSaveData, protocol.GuestClipboardTypes:
		return true
	case protocol.GuestCopy, protocol.GuestPaste:
		return req.MIME != "" && !isPlainText(req.MIME)
	}
	return false
}

func isPlainText(t string) bool {
	m, params, err := mime.ParseMediaType(t)
	return err == nil && m == "text/plain" && (params["charset"] == "" || strings.EqualFold(params["charset"], "utf-8"))
}

// runTransfer serves a transfer request on a guest connection. It may wait
// minutes on the user, so the idle deadline is off meanwhile, and a guest
// that hangs up cancels it.
func (s *Server) runTransfer(c net.Conn, r *bufio.Reader, req protocol.GuestRequest, fds []*os.File) protocol.GuestReply {
	wait := map[string]time.Duration{
		protocol.GuestPick: pickWait, protocol.GuestSave: pickWait, protocol.GuestSaveData: pickWait,
		protocol.GuestCopy: copyWait, protocol.GuestPaste: pasteWait,
	}[req.Type]
	ctx, cancel := context.WithTimeout(context.Background(), max(wait, time.Second))
	defer cancel()
	c.SetReadDeadline(time.Time{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		// Only an end (EOF, a reset) cancels: the next request's bytes stay buffered.
		if _, err := r.Peek(1); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			cancel()
		}
	}()
	var fd *os.File
	if len(fds) > 0 {
		fd = fds[0]
	}
	reply, err := s.serveTransfer(ctx, req, fd)
	if errors.Is(err, errCancelled) {
		reply.Cancelled = true
	} else if err != nil {
		reply.Error = err.Error()
	}
	c.SetReadDeadline(time.Now())
	<-watched
	return reply
}

func (s *Server) serveTransfer(ctx context.Context, req protocol.GuestRequest, fd *os.File) (protocol.GuestReply, error) {
	var reply protocol.GuestReply
	switch req.Type {
	case protocol.GuestShare:
		return reply, s.shareOut(req)
	case protocol.GuestClipboardTypes:
		x := &s.transfers
		x.mu.Lock()
		reply.Types = append([]string{}, x.clipTypes...)
		x.mu.Unlock()
		return reply, nil
	case protocol.GuestPick:
		ref, t := s.newTransfer(req.Type)
		defer s.dropTransfer(ref)
		if err := s.event(protocol.Event{Type: protocol.TypePick, Ref: ref, Types: req.Types, Multiple: req.Multiple}); err != nil {
			return reply, err
		}
		err := t.wait(ctx)
		t.mu.Lock()
		reply.Paths, reply.Names = t.paths, t.names
		t.mu.Unlock()
		return reply, err
	case protocol.GuestSave:
		return s.save(ctx, req, fd)
	case protocol.GuestSaveData:
		t := s.transfer(req.Ref)
		if t == nil || t.kind != protocol.GuestSave {
			return reply, fmt.Errorf("no save %q waiting for its data", req.Ref)
		}
		src, err := fileSource(fd)
		if err != nil {
			return reply, err
		}
		select {
		case t.src <- src:
		default:
			return reply, fmt.Errorf("save %q has its data", req.Ref)
		}
		defer s.dropTransfer(req.Ref)
		return reply, t.wait(ctx)
	case protocol.GuestCopy:
		if fd == nil {
			return reply, errors.New("a typed copy reads an attached descriptor")
		}
		data, err := readAll(ctx, fd, maxClipData+1)
		if err != nil {
			return reply, err
		}
		if len(data) > maxClipData {
			return reply, fmt.Errorf("clipboard data over %d MiB", maxClipData>>20)
		}
		s.setClipboard("")
		x := &s.transfers
		x.mu.Lock()
		x.clipTypes, x.clipMIME, x.clipData = []string{req.MIME}, req.MIME, data
		x.mu.Unlock()
		ref, t := s.newTransfer(req.Type)
		defer s.dropTransfer(ref)
		t.mime = req.MIME
		t.src <- io.NewSectionReader(bytes.NewReader(data), 0, int64(len(data)))
		if err := s.event(protocol.Event{Type: protocol.TypeGuestClipboardData, Ref: ref, MIME: req.MIME, Size: int64(len(data))}); err != nil {
			return reply, err
		}
		return reply, t.wait(ctx)
	case protocol.GuestPaste:
		if fd == nil {
			return reply, errors.New("a typed paste writes to an attached descriptor")
		}
		x := &s.transfers
		x.mu.Lock()
		data, own, types := x.clipData, x.clipMIME != "" && mimeMatches(req.MIME, x.clipMIME), x.clipTypes
		x.mu.Unlock()
		if own {
			_, err := fd.Write(data)
			return reply, err
		}
		if !matchesAny(req.MIME, types) {
			if len(types) == 0 {
				return reply, errors.New("the clipboard is empty, or Android hasn't handed it over (it does when the app gains focus)")
			}
			return reply, fmt.Errorf("the clipboard has %s, not %s", strings.Join(types, ", "), req.MIME)
		}
		ref, t := s.newTransfer(req.Type)
		defer s.dropTransfer(ref)
		t.sink = fd
		if err := s.event(protocol.Event{Type: protocol.TypeClipboardRequest, Ref: ref, MIME: req.MIME}); err != nil {
			return reply, err
		}
		return reply, t.wait(ctx)
	}
	return reply, fmt.Errorf("unknown request %q", req.Type)
}

// save sends fd's file to Android, or with fd nil asks only where to, and
// leaves the transfer for a save-data (until pickWait passes).
func (s *Server) save(ctx context.Context, req protocol.GuestRequest, fd *os.File) (protocol.GuestReply, error) {
	var reply protocol.GuestReply
	name := filepath.Base(req.Name)
	if validName(name) != nil {
		name = "untitled"
	}
	var src *io.SectionReader
	if fd != nil {
		var err error
		if src, err = fileSource(fd); err != nil {
			return reply, err
		}
	}
	ref, t := s.newTransfer(req.Type)
	t.name, t.mime = name, req.MIME
	ev := protocol.Event{Type: protocol.TypeSave, Ref: ref, Name: name, MIME: req.MIME}
	if src != nil {
		t.src <- src
		ev.Size = src.Size()
	}
	if err := s.event(ev); err != nil {
		s.dropTransfer(ref)
		return reply, err
	}
	if src != nil {
		defer s.dropTransfer(ref)
		return reply, t.wait(ctx)
	}
	select {
	case <-t.chosen:
		time.AfterFunc(pickWait, func() {
			t.finish(errors.New("no data came for the save"))
			s.dropTransfer(ref)
		})
		reply.Ref = ref
		return reply, nil
	case err := <-t.done:
		s.dropTransfer(ref)
		if err == nil {
			err = errors.New("the app ended the save without fetching it")
		}
		return reply, err
	case <-ctx.Done():
		s.dropTransfer(ref)
		return reply, waitError(ctx)
	}
}

// readAll reads up to limit bytes of r until ctx ends. A pipe whose writer
// never closes leaves its read behind rather than the guest's request.
func readAll(ctx context.Context, r io.Reader, limit int64) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(r, limit))
		ch <- result{b, err}
	}()
	select {
	case res := <-ch:
		return res.b, res.err
	case <-ctx.Done():
		return nil, fmt.Errorf("the data didn't end in time: %w", ctx.Err())
	}
}

// fileSource reads an attached regular file from its start.
func fileSource(fd *os.File) (*io.SectionReader, error) {
	if fd == nil {
		return nil, errors.New("no file attached")
	}
	fi, err := fd.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("the attached file isn't a regular file")
	}
	return io.NewSectionReader(fd, 0, fi.Size()), nil
}

// shareOut offers files under the file provider's root, or text, to Android's share sheet.
func (s *Server) shareOut(req protocol.GuestRequest) error {
	if len(req.Paths) == 0 {
		if req.Text == "" {
			return errors.New("nothing to share")
		}
		if len(req.Text) > maxClipboard {
			return fmt.Errorf("shared text over %d KiB", maxClipboard>>10)
		}
		return s.event(protocol.Event{Type: protocol.TypeShare, Text: req.Text})
	}
	rels := make([]string, 0, len(req.Paths))
	for _, p := range req.Paths {
		rel, err := s.underFilesRoot(p)
		if err != nil {
			return err
		}
		if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is not a file", p)
		}
		rels = append(rels, rel)
	}
	return s.event(protocol.Event{Type: protocol.TypeShare, Paths: rels})
}

// mimeMatches reports whether a requested type (maybe type/*) covers t.
func mimeMatches(want, t string) bool {
	w, _, err1 := mime.ParseMediaType(want)
	m, _, err2 := mime.ParseMediaType(t)
	if err1 != nil || err2 != nil {
		return false
	}
	if w == m || w == "*/*" {
		return true
	}
	major, sub, _ := strings.Cut(w, "/")
	return sub == "*" && strings.HasPrefix(m, major+"/")
}

func matchesAny(want string, types []string) bool {
	for _, t := range types {
		if mimeMatches(want, t) {
			return true
		}
	}
	return false
}

// serveTransferShare takes bytes the app sends for a transfer: a picked file
// into the share directory, or a paste's data into its descriptor.
func (s *Server) serveTransferShare(c net.Conn, h protocol.Hello) {
	t := s.transfer(h.Ref)
	if t == nil || (t.kind != protocol.GuestPick && t.kind != protocol.GuestPaste) {
		reject(c, fmt.Sprintf("no request %q waits for a file (it may have timed out)", h.Ref))
		return
	}
	if t.kind == protocol.GuestPaste {
		if protocol.WriteJSON(c, s.hello()) != nil {
			return
		}
		n, err := io.Copy(t.sink, io.LimitReader(c, h.Size))
		if err == nil && n != h.Size {
			err = io.ErrUnexpectedEOF
		}
		t.finish(err)
		if err != nil {
			reject(c, err.Error())
			return
		}
		protocol.WriteJSON(c, protocol.Done{Type: protocol.TypeDone})
		return
	}
	name := filepath.Base(h.Name)
	if validName(name) != nil {
		name = "picked"
	}
	ready := func() error { return protocol.WriteJSON(c, s.hello()) }
	p, err := s.store(name, ready, io.LimitReader(c, h.Size), h.Size)
	if err != nil {
		reject(c, err.Error())
		return
	}
	t.mu.Lock()
	t.paths, t.names = append(t.paths, p), append(t.names, name)
	t.mu.Unlock()
	protocol.WriteJSON(c, protocol.Done{Type: protocol.TypeDone, Path: p})
}

// serveSave hands the app a save's or a typed copy's bytes, once they're
// there, and settles the transfer with the app's answer.
func (s *Server) serveSave(c net.Conn, h protocol.Hello) {
	t := s.transfer(h.Ref)
	if t == nil || (t.kind != protocol.GuestSave && t.kind != protocol.GuestCopy) {
		reject(c, fmt.Sprintf("no request %q has bytes to fetch (it may have timed out)", h.Ref))
		return
	}
	t.choose()
	var src *io.SectionReader
	select {
	case src = <-t.src:
	case <-t.ended:
		reject(c, "the guest gave up on the save")
		return
	}
	reply := s.hello()
	reply.Name, reply.Size = t.name, src.Size()
	if protocol.WriteJSON(c, reply) != nil {
		t.finish(errors.New("the app's stream closed"))
		return
	}
	if _, err := io.Copy(c, src); err != nil {
		t.finish(err)
		return
	}
	line, err := protocol.ReadLine(c)
	if err != nil {
		t.finish(fmt.Errorf("the app didn't confirm: %w", err))
		return
	}
	var msg protocol.Error
	if err := json.Unmarshal(line, &msg); err != nil {
		t.finish(err)
	} else if msg.Type == protocol.TypeError {
		t.finish(errors.New(msg.Message))
	} else {
		t.finish(nil)
	}
}

// guestDone ends a transfer as the app says.
func (s *Server) guestDone(req protocol.Request) error {
	t := s.transfer(req.Ref)
	if t == nil {
		return fmt.Errorf("no request %q (it may have timed out)", req.Ref)
	}
	switch {
	case req.Cancelled:
		t.finish(errCancelled)
	case req.Error != "":
		t.finish(errors.New(req.Error))
	default:
		t.finish(nil)
	}
	return nil
}

// fdConn reads a guest connection, keeping the descriptors that come with its bytes.
type fdConn struct {
	net.Conn
	mu  sync.Mutex
	fds []*os.File
}

func (f *fdConn) Read(b []byte) (int, error) {
	uc, ok := f.Conn.(*net.UnixConn)
	if !ok {
		return f.Conn.Read(b)
	}
	oob := make([]byte, syscall.CmsgSpace(4*maxGuestFDs))
	n, oobn, _, _, err := uc.ReadMsgUnix(b, oob)
	// It reports -1 with some errors (a deadline), which bufio refuses.
	n = max(n, 0)
	if oobn > 0 {
		msgs, perr := syscall.ParseSocketControlMessage(oob[:oobn])
		if perr != nil {
			log.Printf("guest socket: %v", perr)
		}
		for _, m := range msgs {
			fds, err := syscall.ParseUnixRights(&m)
			if err != nil {
				continue
			}
			f.mu.Lock()
			for _, fd := range fds {
				f.fds = append(f.fds, os.NewFile(uintptr(fd), "guest"))
			}
			f.mu.Unlock()
		}
	}
	return n, err
}

// take returns the descriptors received so far.
func (f *fdConn) take() []*os.File {
	f.mu.Lock()
	defer f.mu.Unlock()
	fds := f.fds
	f.fds = nil
	return fds
}

func closeFiles(fs []*os.File) {
	for _, f := range fs {
		f.Close()
	}
}
