package image

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

const mib = 1 << 20

// makeDisk builds a GPT image with an empty "ESP" and an ext4 "nixos" root,
// as the module's repart image has.
func makeDisk(t *testing.T) string {
	t.Helper()
	for _, tool := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "root.img")
	if err := os.WriteFile(root, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(root, 16*mib); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-L", "nixos", root).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v\n%s", err, out)
	}
	rootData, _ := os.ReadFile(root)

	disk := filepath.Join(dir, "disk.img")
	img := make([]byte, 1*mib+8*mib+16*mib+1*mib)
	writeGPT(img, []gptEntry{
		{name: "ESP", first: 1 * mib / sector, last: 9*mib/sector - 1},
		{name: "nixos", first: 9 * mib / sector, last: 25*mib/sector - 1},
	})
	copy(img[9*mib:], rootData)
	if err := os.WriteFile(disk, img, 0o644); err != nil {
		t.Fatal(err)
	}
	return disk
}

type gptEntry struct {
	name        string
	first, last int64
}

func writeGPT(img []byte, entries []gptEntry) {
	hdr := img[sector : 2*sector]
	copy(hdr, "EFI PART")
	binary.LittleEndian.PutUint64(hdr[72:], 2) // entries at LBA 2
	binary.LittleEndian.PutUint32(hdr[80:], 128)
	binary.LittleEndian.PutUint32(hdr[84:], 128)
	for i, e := range entries {
		ent := img[2*sector+i*128 : 2*sector+(i+1)*128]
		ent[0] = byte(i + 1) // any non-zero type GUID
		binary.LittleEndian.PutUint64(ent[32:], uint64(e.first))
		binary.LittleEndian.PutUint64(ent[40:], uint64(e.last))
		for j, c := range utf16.Encode([]rune(e.name)) {
			binary.LittleEndian.PutUint16(ent[56+2*j:], c)
		}
	}
}

func TestRootPartition(t *testing.T) {
	disk := makeDisk(t)
	p, err := RootPartition(disk)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "nixos" || p.Offset != 9*mib || p.Size != 16*mib {
		t.Errorf("root = %+v", p)
	}
}

func debugfsCat(t *testing.T, disk, guest string) string {
	t.Helper()
	root, _ := RootPartition(disk)
	out, err := exec.Command("debugfs", "-R", "cat "+guest, device(disk, root)).Output()
	if err != nil {
		t.Fatalf("debugfs cat %s: %v", guest, err)
	}
	return string(out)
}

func debugfsStat(t *testing.T, disk, guest string) string {
	t.Helper()
	root, _ := RootPartition(disk)
	out, _ := exec.Command("debugfs", "-R", "stat "+guest, device(disk, root)).CombinedOutput()
	return string(out)
}

func TestInject(t *testing.T) {
	disk := makeDisk(t)
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("secret key\n"), 0o644)

	err := Inject(disk, []Injection{
		{Guest: "/etc/ssh/ssh_host_ed25519_key", Source: key, Mode: 0o600},
		{Guest: "/etc/ssh/authorized_keys.d/nixos", Source: "-", Mode: 0o644},
	}, strings.NewReader("ssh-ed25519 AAAA test\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := debugfsCat(t, disk, "/etc/ssh/ssh_host_ed25519_key"); got != "secret key\n" {
		t.Errorf("key = %q", got)
	}
	if got := debugfsCat(t, disk, "/etc/ssh/authorized_keys.d/nixos"); got != "ssh-ed25519 AAAA test\n" {
		t.Errorf("authorized keys = %q", got)
	}
	st := debugfsStat(t, disk, "/etc/ssh/ssh_host_ed25519_key")
	for _, want := range []string{"Mode:  0600", "User:     0   Group:     0"} {
		if !strings.Contains(st, want) {
			t.Errorf("key stat lacks %q:\n%s", want, st)
		}
	}
	if st := debugfsStat(t, disk, "/etc/ssh"); !strings.Contains(st, "Type: directory") || !strings.Contains(st, "Mode:  0755") {
		t.Errorf("/etc/ssh:\n%s", st)
	}

	// Injecting again replaces the file and leaves existing directories alone.
	os.WriteFile(key, []byte("new key\n"), 0o644)
	if err := Inject(disk, []Injection{{Guest: "/etc/ssh/ssh_host_ed25519_key", Source: key, Mode: 0o400}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := debugfsCat(t, disk, "/etc/ssh/ssh_host_ed25519_key"); got != "new key\n" {
		t.Errorf("re-injected key = %q", got)
	}
}

func TestInjectRejectsRelativePath(t *testing.T) {
	disk := makeDisk(t)
	if err := Inject(disk, []Injection{{Guest: "etc/x", Source: "/dev/null", Mode: 0o600}}, nil); err == nil {
		t.Error("relative guest path accepted")
	}
}

func TestCopySparse(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	data := make([]byte, 5*mib+123)
	copy(data[2*mib:], "middle")
	copy(data[len(data)-3:], "end")
	os.WriteFile(src, data, 0o644)
	dst := filepath.Join(dir, "dst")
	if err := CopySparse(dst, src); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, data) {
		t.Error("copy differs from source")
	}
}
