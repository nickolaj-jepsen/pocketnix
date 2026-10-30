package agent_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nickolaj-jepsen/pocketnix/go/internal/protocol"
)

// soundPipe makes a FIFO and opens it the way PipeWire's pipe sink does.
func soundPipe(t *testing.T) (string, *os.File) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audio")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return path, w
}

func audioConfig(pipe string) map[string]any {
	cfg := baseConfig()
	cfg["audioPipe"] = pipe
	return cfg
}

func TestAudioStreamsWhatPlaysFromNowOn(t *testing.T) {
	pipe, w := soundPipe(t)
	w.Write([]byte("before"))
	h := start(t, audioConfig(pipe), unixAddr)
	c, err := h.client.Audio()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	w.Write([]byte("sound"))
	if got := readN(t, c, 5); got != "sound" {
		t.Errorf("the stream carried %q, want what played after it opened", got)
	}
}

func TestANewerAudioStreamEndsTheOlder(t *testing.T) {
	pipe, w := soundPipe(t)
	h := start(t, audioConfig(pipe), unixAddr)
	old, err := h.client.Audio()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	c, err := h.client.Audio()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	old.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(old); err != nil {
		t.Errorf("the older stream: %v, want it ended", err)
	}
	w.Write([]byte("x"))
	if got := readN(t, c, 1); got != "x" {
		t.Errorf("the newer stream carried %q", got)
	}
}

func TestAudioRefused(t *testing.T) {
	plain := filepath.Join(t.TempDir(), "plain")
	os.WriteFile(plain, nil, 0o644)
	for name, tc := range map[string]struct {
		cfg map[string]any
		// The agent's wording for audio off; the other refusals' text is the system's.
		want string
	}{
		"off":      {baseConfig(), "audio is not enabled"},
		"missing":  {audioConfig(filepath.Join(t.TempDir(), "missing")), ""},
		"not fifo": {audioConfig(plain), ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := start(t, tc.cfg, unixAddr)
			c, err := h.client.Audio()
			var re *protocol.RemoteError
			if !errors.As(err, &re) {
				if c != nil {
					c.Close()
				}
				t.Fatalf("Audio() = %v, want an agent error", err)
			}
			if !strings.Contains(re.Message, tc.want) {
				t.Errorf("error %q, want it to mention %q", re.Message, tc.want)
			}
		})
	}
}
