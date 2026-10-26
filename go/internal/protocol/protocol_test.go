package protocol

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	WriteFrame(&buf, FrameData, []byte("hi"))
	WriteFrame(&buf, FrameResize, Winsize{Rows: 50, Cols: 132}.Encode())
	WriteFrame(&buf, FrameExit, EncodeExit(-15))

	typ, p, err := ReadFrame(&buf)
	if err != nil || typ != FrameData || string(p) != "hi" {
		t.Fatalf("data frame = %d %q %v", typ, p, err)
	}
	typ, p, _ = ReadFrame(&buf)
	if ws, err := DecodeWinsize(p); typ != FrameResize || err != nil || ws.Rows != 50 || ws.Cols != 132 {
		t.Fatalf("resize frame = %d %+v %v", typ, ws, err)
	}
	typ, p, _ = ReadFrame(&buf)
	if st, err := DecodeExit(p); typ != FrameExit || err != nil || st != -15 {
		t.Fatalf("exit frame = %d %d %v", typ, st, err)
	}
	if _, _, err := ReadFrame(&buf); err != io.EOF {
		t.Fatalf("at end: %v, want io.EOF", err)
	}
}

func TestFrameTruncated(t *testing.T) {
	var buf bytes.Buffer
	WriteFrame(&buf, FrameData, []byte("hello"))
	if _, _, err := ReadFrame(bytes.NewReader(buf.Bytes()[:7])); err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated frame: %v", err)
	}
}

func TestFrameTooLarge(t *testing.T) {
	hdr := []byte{FrameData, 0xff, 0xff, 0xff, 0xff}
	if _, _, err := ReadFrame(bytes.NewReader(hdr)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("huge length: %v", err)
	}
}

func TestDatagramRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	WriteDatagram(&buf, []byte("ping"))
	WriteDatagram(&buf, nil)
	big := bytes.Repeat([]byte{7}, MaxDatagram)
	WriteDatagram(&buf, big)
	p := make([]byte, MaxDatagram)
	if n, err := ReadDatagram(&buf, p); err != nil || string(p[:n]) != "ping" {
		t.Fatalf("first = %q %v", p[:n], err)
	}
	if n, err := ReadDatagram(&buf, p); err != nil || n != 0 {
		t.Fatalf("empty = %d %v", n, err)
	}
	if n, err := ReadDatagram(&buf, p); err != nil || !bytes.Equal(p[:n], big) {
		t.Fatalf("largest = %d %v", n, err)
	}
	if _, err := ReadDatagram(&buf, p); err != io.EOF {
		t.Fatalf("at end: %v, want io.EOF", err)
	}
	if err := WriteDatagram(&buf, make([]byte, MaxDatagram+1)); !errors.Is(err, ErrDatagramTooLarge) {
		t.Fatalf("oversized write: %v", err)
	}
	if _, err := ReadDatagram(bytes.NewReader([]byte{0, 5, 'a'}), p); err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated: %v", err)
	}
}

func TestReadLineLeavesTheRest(t *testing.T) {
	r := bytes.NewReader([]byte("{\"type\":\"hello\",\"version\":1}\nRAW"))
	h, err := ReadHello(r)
	if err != nil || h.Version != 1 {
		t.Fatalf("hello = %+v, %v", h, err)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "RAW" {
		t.Fatalf("rest = %q", rest)
	}
}

func TestReadLineBuffered(t *testing.T) {
	long := bytes.Repeat([]byte("x"), MaxLine)
	in := append(append([]byte("a\n"), long...), "\nb\n"...)
	r := bufio.NewReaderSize(bytes.NewReader(in), 16)
	for _, want := range []string{"a", string(long), "b"} {
		line, err := ReadLine(r)
		if err != nil || string(line) != want {
			t.Fatalf("line of %d bytes: %d bytes, %v", len(want), len(line), err)
		}
	}
	if _, err := ReadLine(r); err != io.EOF {
		t.Fatalf("at end: %v", err)
	}
	if _, err := ReadLine(bufio.NewReader(bytes.NewReader(append(long, "x\n"...)))); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("over MaxLine: %v", err)
	}
	if _, err := ReadLine(bufio.NewReader(bytes.NewReader([]byte("half")))); err != io.ErrUnexpectedEOF {
		t.Fatalf("cut short: %v", err)
	}
}

func TestReadHelloError(t *testing.T) {
	_, err := ReadHello(bytes.NewReader([]byte(`{"type":"error","message":"nope"}` + "\n")))
	var re *RemoteError
	if !errors.As(err, &re) || re.Message != "nope" {
		t.Fatalf("got %v", err)
	}
}
