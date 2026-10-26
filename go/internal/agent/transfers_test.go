package agent_test

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/client"
	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// connectApp opens a control stream, as the app does, and waits until the agent registered it.
func connectApp(t *testing.T, h *harness) *client.Control {
	t.Helper()
	ctl, err := h.client.Control()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctl.Close() })
	if _, err := ctl.WaitUpdates(func(protocol.Updates) bool { return true }); err != nil {
		t.Fatal(err)
	}
	return ctl
}

type guestResult struct {
	reply protocol.GuestReply
	err   error
}

// guestAsync sends a guest request that waits on the app.
func guestAsync(sock string, req protocol.GuestRequest, files ...*os.File) <-chan guestResult {
	ch := make(chan guestResult, 1)
	go func() {
		r, err := client.GuestCall(sock, req, files...)
		ch <- guestResult{r, err}
	}()
	return ch
}

func result(t *testing.T, ch <-chan guestResult) guestResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no guest reply within 5s")
	}
	panic("unreachable")
}

func eventOf(t *testing.T, ctl *client.Control, typ string) protocol.Event {
	t.Helper()
	ev := nextEvent(t, ctl)
	if ev.Type != typ {
		t.Fatalf("event %+v, want %s", ev, typ)
	}
	return ev
}

func TestShareOut(t *testing.T) {
	h, root, _, sock := startIntegrations(t, nil)
	ctl := connectApp(t, h)
	must(t, os.MkdirAll(filepath.Join(root, "docs"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "docs", "a.pdf"), nil, 0o644))
	must(t, os.WriteFile(filepath.Join(root, "b.txt"), nil, 0o644))

	if _, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestShare, Paths: []string{filepath.Join(root, "docs", "a.pdf"), filepath.Join(root, "b.txt")}}); err != nil {
		t.Fatal(err)
	}
	if ev := eventOf(t, ctl, protocol.TypeShare); !slices.Equal(ev.Paths, []string{"docs/a.pdf", "b.txt"}) {
		t.Errorf("shared %q", ev.Paths)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	must(t, os.WriteFile(outside, nil, 0o644))
	if _, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestShare, Paths: []string{outside}}); err == nil {
		t.Error("shared a file outside the root")
	}
	if _, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestShare, Paths: []string{filepath.Join(root, "docs")}}); err == nil {
		t.Error("shared a directory")
	}
	if _, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestShare, Text: "a link"}); err != nil {
		t.Fatal(err)
	}
	if ev := eventOf(t, ctl, protocol.TypeShare); ev.Text != "a link" {
		t.Errorf("shared text %q", ev.Text)
	}
}

func TestPick(t *testing.T) {
	h, _, inbox, sock := startIntegrations(t, nil)
	ctl := connectApp(t, h)

	done := guestAsync(sock, protocol.GuestRequest{Type: protocol.GuestPick, Types: []string{"image/*"}, Multiple: true})
	ev := eventOf(t, ctl, protocol.TypePick)
	if ev.Ref == "" || !ev.Multiple || !slices.Equal(ev.Types, []string{"image/*"}) {
		t.Fatalf("pick event %+v", ev)
	}
	p1, err := h.client.ShareFor(ev.Ref, "a.png", []byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := h.client.ShareFor(ev.Ref, "a.png", []byte("two"))
	if err != nil {
		t.Fatal(err)
	}
	must(t, ctl.GuestDone(ev.Ref, false, ""))
	r := result(t, done)
	if r.err != nil || r.reply.Cancelled || !slices.Equal(r.reply.Paths, []string{filepath.Join(inbox, "a.png"), filepath.Join(inbox, "a (1).png")}) || !slices.Equal(r.reply.Names, []string{"a.png", "a.png"}) {
		t.Fatalf("pick = %+v, %v", r.reply, r.err)
	}
	if b, _ := os.ReadFile(p2); string(b) != "two" || p1 == p2 {
		t.Errorf("second file %s: %q", p2, b)
	}
	// Over, the pick takes no more files.
	if _, err := h.client.ShareFor(ev.Ref, "late.png", []byte("x")); err == nil {
		t.Error("a finished pick took a file")
	}

	done = guestAsync(sock, protocol.GuestRequest{Type: protocol.GuestPick})
	ev = eventOf(t, ctl, protocol.TypePick)
	must(t, ctl.GuestDone(ev.Ref, true, ""))
	if r := result(t, done); r.err != nil || !r.reply.Cancelled || len(r.reply.Paths) != 0 {
		t.Errorf("cancelled pick = %+v, %v", r.reply, r.err)
	}

	// A guest that hangs up ends its pick: the picker's late file is refused.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	must(t, protocol.WriteJSON(conn, protocol.GuestRequest{Type: protocol.GuestPick}))
	ev = eventOf(t, ctl, protocol.TypePick)
	conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for ctl.GuestDone(ev.Ref, false, "") == nil {
		if time.Now().After(deadline) {
			t.Fatal("the pick outlived its guest")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPickWithoutTheApp(t *testing.T) {
	_, _, _, sock := startIntegrations(t, nil)
	if _, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestPick}); err == nil || !strings.Contains(err.Error(), "isn't connected") {
		t.Errorf("pick without the app: %v", err)
	}
}

func TestSave(t *testing.T) {
	h, _, _, sock := startIntegrations(t, nil)
	ctl := connectApp(t, h)
	// Outside the file provider's root: the guest program opened it.
	src := filepath.Join(t.TempDir(), "report.pdf")
	must(t, os.WriteFile(src, []byte("PDF bytes"), 0o600))
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	done := guestAsync(sock, protocol.GuestRequest{Type: protocol.GuestSave, Name: "out.pdf", MIME: "application/pdf"}, f)
	ev := eventOf(t, ctl, protocol.TypeSave)
	if ev.Name != "out.pdf" || ev.MIME != "application/pdf" || ev.Size != 9 {
		t.Fatalf("save event %+v", ev)
	}
	var got bytes.Buffer
	if err := h.client.Fetch(ev.Ref, func(name string, r io.Reader) error {
		if name != "out.pdf" {
			t.Errorf("name %q", name)
		}
		_, err := io.Copy(&got, r)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := result(t, done); r.err != nil || r.reply.Cancelled || got.String() != "PDF bytes" {
		t.Errorf("save = %+v, %v; stored %q", r.reply, r.err, got.String())
	}

	// The app's failure to store is the guest's error.
	done = guestAsync(sock, protocol.GuestRequest{Type: protocol.GuestSave, Name: "out.pdf"}, f)
	ev = eventOf(t, ctl, protocol.TypeSave)
	h.client.Fetch(ev.Ref, func(string, io.Reader) error { return io.ErrShortWrite })
	if r := result(t, done); r.err == nil || !strings.Contains(r.err.Error(), "short write") {
		t.Errorf("failed save = %+v, %v", r.reply, r.err)
	}

	done = guestAsync(sock, protocol.GuestRequest{Type: protocol.GuestSave, Name: "out.pdf"}, f)
	ev = eventOf(t, ctl, protocol.TypeSave)
	must(t, ctl.GuestDone(ev.Ref, true, ""))
	if r := result(t, done); r.err != nil || !r.reply.Cancelled {
		t.Errorf("cancelled save = %+v, %v", r.reply, r.err)
	}

	pipeR, pipeW, _ := os.Pipe()
	defer pipeR.Close()
	defer pipeW.Close()
	if _, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestSave, Name: "x"}, pipeR); err == nil {
		t.Error("saved a pipe")
	}
}

func TestDeferredSave(t *testing.T) {
	h, _, _, sock := startIntegrations(t, nil)
	ctl := connectApp(t, h)

	chosen := guestAsync(sock, protocol.GuestRequest{Type: protocol.GuestSave, Name: "notes.txt", MIME: "text/plain"})
	ev := eventOf(t, ctl, protocol.TypeSave)
	if ev.Size != 0 {
		t.Errorf("deferred save announced %d bytes", ev.Size)
	}
	fetched := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		err := h.client.Fetch(ev.Ref, func(_ string, r io.Reader) error { _, err := io.Copy(&b, r); return err })
		if err != nil {
			fetched <- "error: " + err.Error()
		}
		fetched <- b.String()
	}()
	r := result(t, chosen)
	if r.err != nil || r.reply.Ref != ev.Ref {
		t.Fatalf("save = %+v, %v", r.reply, r.err)
	}
	// The user picked; the guest writes the file only now.
	p := filepath.Join(t.TempDir(), "notes.txt")
	must(t, os.WriteFile(p, []byte("written later"), 0o644))
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestSaveData, Ref: r.reply.Ref}, f); err != nil {
		t.Fatal(err)
	}
	if got := <-fetched; got != "written later" {
		t.Errorf("fetched %q", got)
	}
}

func TestTypedClipboard(t *testing.T) {
	h, _, _, sock := startIntegrations(t, nil)
	ctl := connectApp(t, h)
	png := []byte("\x89PNG fake image")

	in, w, _ := os.Pipe()
	w.Write(png)
	w.Close()
	done := guestAsync(sock, protocol.GuestRequest{Type: protocol.GuestCopy, MIME: "image/png"}, in)
	ev := eventOf(t, ctl, protocol.TypeGuestClipboardData)
	if ev.MIME != "image/png" || ev.Size != int64(len(png)) {
		t.Fatalf("copy event %+v", ev)
	}
	var got []byte
	must(t, h.client.Fetch(ev.Ref, func(_ string, r io.Reader) (err error) { got, err = io.ReadAll(r); return err }))
	if r := result(t, done); r.err != nil || !bytes.Equal(got, png) {
		t.Fatalf("copy = %v, app got %q", r.err, got)
	}
	in.Close()

	// The guest's own copy pastes without the app.
	if types, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestClipboardTypes}); err != nil || !slices.Equal(types.Types, []string{"image/png"}) {
		t.Errorf("types = %+v, %v", types, err)
	}
	if got := paste(t, sock, "image/*", nil); !bytes.Equal(got, png) {
		t.Errorf("own paste = %q", got)
	}

	// Android's clipboard: only its types until a paste asks.
	must(t, ctl.SetClipboardTypes("", []string{"image/jpeg"}))
	if types, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestClipboardTypes}); err != nil || !slices.Equal(types.Types, []string{"image/jpeg"}) {
		t.Errorf("types = %+v, %v", types, err)
	}
	r, w, _ := os.Pipe()
	defer r.Close()
	if _, err := client.GuestCall(sock, protocol.GuestRequest{Type: protocol.GuestPaste, MIME: "image/png"}, w); err == nil || !strings.Contains(err.Error(), "image/jpeg") {
		t.Errorf("paste of a missing type: %v", err)
	}
	w.Close()
	jpeg := []byte("JFIF data")
	if got := paste(t, sock, "image/*", func() {
		ev := eventOf(t, ctl, protocol.TypeClipboardRequest)
		if ev.MIME != "image/*" {
			t.Errorf("request %+v", ev)
		}
		if _, err := h.client.ShareFor(ev.Ref, "", jpeg); err != nil {
			t.Error(err)
		}
	}); !bytes.Equal(got, jpeg) {
		t.Errorf("paste = %q", got)
	}
	// Text still pastes as before, and a text copy drops the typed data.
	must(t, ctl.SetClipboardTypes("plain", nil))
	if got, err := client.Guest(sock, protocol.GuestRequest{Type: protocol.GuestPaste}); err != nil || got != "plain" {
		t.Errorf("text paste = %q, %v", got, err)
	}
}

// paste runs a typed paste into a pipe, with app answering the agent, and returns what arrived.
func paste(t *testing.T, sock, mime string, app func()) []byte {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	done := guestAsync(sock, protocol.GuestRequest{Type: protocol.GuestPaste, MIME: mime}, w)
	if app != nil {
		app()
	}
	res := result(t, done)
	w.Close()
	if res.err != nil {
		t.Fatalf("paste %s: %v", mime, res.err)
	}
	b, _ := io.ReadAll(r)
	return b
}
