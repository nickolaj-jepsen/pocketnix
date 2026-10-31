package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestProvisionReleaseImage(t *testing.T) {
	d := newFakeDevice(t)
	img, want := testImage(t)
	apk := filepath.Join(t.TempDir(), "app-debug.apk")
	os.WriteFile(apk, []byte("apk"), 0o644)
	o, err := parseProvision([]string{"--apk", apk, "--debug", "--no-start"})
	if err != nil {
		t.Fatal(err)
	}
	o.log = io.Discard
	o.downloadImage = func(_, dst string) error {
		b, err := os.ReadFile(img)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	}
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d.disk(), want) {
		t.Error("the phone's disk isn't the release image")
	}
}

func TestGunzipSparse(t *testing.T) {
	// Data, a zero MiB, data, and a zero tail.
	want := bytes.Repeat([]byte("a"), 1<<20)
	want = append(want, make([]byte, 1<<20)...)
	want = append(want, "data"...)
	want = append(want, make([]byte, 3<<20)...)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(want)
	zw.Close()
	dst := filepath.Join(t.TempDir(), "disk.img")
	if err := gunzipSparse(dst, &gz); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("unpacked %d bytes, differing from the %d packed", len(got), len(want))
	}
}
