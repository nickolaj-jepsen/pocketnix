package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func u32(n int) []byte { return binary.LittleEndian.AppendUint32(nil, uint32(n)) }

func prefixed(parts ...[]byte) []byte {
	b := bytes.Join(parts, nil)
	return append(u32(len(b)), b...)
}

// v2Signer is a v2 signer whose signed data names cert (signatures and key are dummies).
func v2Signer(cert []byte) []byte {
	signedData := bytes.Join([][]byte{prefixed(), prefixed(prefixed(cert)), prefixed()}, nil)
	return prefixed(prefixed(signedData), prefixed(), prefixed([]byte("key")))
}

// v3Signer has the v3 layout: SDK bounds after the certificates and after the signed data.
func v3Signer(cert []byte) []byte {
	signedData := bytes.Join([][]byte{prefixed(), prefixed(prefixed(cert)), u32(24), u32(0x7fffffff), prefixed()}, nil)
	return prefixed(prefixed(signedData), u32(24), u32(0x7fffffff), prefixed(), prefixed([]byte("key")))
}

type pair struct {
	id    uint32
	value []byte
}

// writeAPK writes a ZIP whose central directory is preceded by an APK
// signing block holding pairs (none: no block).
func writeAPK(t *testing.T, pairs ...pair) string {
	t.Helper()
	var z bytes.Buffer
	w := zip.NewWriter(&z)
	f, _ := w.Create("AndroidManifest.xml")
	f.Write([]byte("manifest"))
	w.Close()
	data := z.Bytes()
	eocd := bytes.LastIndex(data, []byte{0x50, 0x4b, 0x05, 0x06})
	cd := int(binary.LittleEndian.Uint32(data[eocd+16:]))
	if len(pairs) == 0 {
		return writeFile(t, data)
	}
	var entries []byte
	for _, p := range pairs {
		entries = binary.LittleEndian.AppendUint64(entries, uint64(4+len(p.value)))
		entries = binary.LittleEndian.AppendUint32(entries, p.id)
		entries = append(entries, p.value...)
	}
	size := uint64(len(entries) + 8 + 16)
	block := binary.LittleEndian.AppendUint64(nil, size)
	block = append(block, entries...)
	block = binary.LittleEndian.AppendUint64(block, size)
	block = append(block, apkSigMagic...)
	out := append(append(append([]byte{}, data[:cd]...), block...), data[cd:]...)
	binary.LittleEndian.PutUint32(out[eocd+len(block)+16:], uint32(cd+len(block)))
	return writeFile(t, out)
}

func writeFile(t *testing.T, b []byte) string {
	p := filepath.Join(t.TempDir(), "app.apk")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func certSum(c []byte) string {
	s := sha256.Sum256(c)
	return hex.EncodeToString(s[:])
}

func TestReleaseSigner(t *testing.T) {
	ours, theirs := []byte("our certificate"), []byte("their certificate")
	pin := certSum(ours)
	cases := []struct {
		name  string
		apk   string
		valid bool
	}{
		{"v2 by us", writeAPK(t, pair{0x7109871a, prefixed(v2Signer(ours))}), true},
		{"v2 and v3 by us, padding between", writeAPK(t, pair{0x7109871a, prefixed(v2Signer(ours))}, pair{0x42726577, make([]byte, 100)}, pair{0xf05368c0, prefixed(v3Signer(ours))}), true},
		{"v2 by someone else", writeAPK(t, pair{0x7109871a, prefixed(v2Signer(theirs))}), false},
		// Android would verify the v3 block and ignore our v2 one.
		{"our v2, their v3", writeAPK(t, pair{0x7109871a, prefixed(v2Signer(ours))}, pair{0xf05368c0, prefixed(v3Signer(theirs))}), false},
		{"a second signer", writeAPK(t, pair{0xf05368c0, prefixed(v3Signer(ours), v3Signer(theirs))}), false},
		{"their v3.1", writeAPK(t, pair{0x7109871a, prefixed(v2Signer(ours))}, pair{0x1b93ad61, prefixed(v3Signer(theirs))}), false},
		{"only v1 (no block)", writeAPK(t), false},
		{"a block with no scheme", writeAPK(t, pair{0x42726577, make([]byte, 8)}), false},
		{"truncated signer", writeAPK(t, pair{0x7109871a, prefixed(u32(1000))}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkReleaseSigner(c.apk, pin)
			if (err == nil) != c.valid {
				t.Fatalf("checkReleaseSigner: %v, want valid=%v", err, c.valid)
			}
		})
	}
}

func TestReleaseSignerNotZip(t *testing.T) {
	err := checkReleaseSigner(writeFile(t, []byte("<html>not found</html>")), releaseCertSHA256)
	if err == nil || !strings.Contains(err.Error(), "ZIP") {
		t.Fatalf("err = %v", err)
	}
}

// POCKETNIX_RELEASE_APK checks a real signed APK against the pin.
func TestReleaseSignerRealAPK(t *testing.T) {
	p := os.Getenv("POCKETNIX_RELEASE_APK")
	if p == "" {
		t.Skip("POCKETNIX_RELEASE_APK not set")
	}
	if err := checkReleaseSigner(p, releaseCertSHA256); err != nil {
		t.Fatal(err)
	}
}
