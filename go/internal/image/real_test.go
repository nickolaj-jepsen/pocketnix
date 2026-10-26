package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealImage injects into a copy of a built image:
// POCKETNIX_TEST_IMAGE=$(nix build --print-out-paths .#example-image)/pocketnix-example-pocketnix.raw go test ./internal/image -run TestRealImage
func TestRealImage(t *testing.T) {
	src := os.Getenv("POCKETNIX_TEST_IMAGE")
	if src == "" {
		t.Skip("POCKETNIX_TEST_IMAGE not set")
	}
	root, err := RootPartition(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("root partition %q at %d MiB, %d MiB", root.Name, root.Offset>>20, root.Size>>20)

	disk := filepath.Join(t.TempDir(), "disk.img")
	if err := CopySparse(disk, src); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "key")
	os.WriteFile(key, []byte("host key\n"), 0o600)
	err = Inject(disk, []Injection{
		{Guest: "/etc/ssh/ssh_host_ed25519_key", Source: key, Mode: 0o600},
		{Guest: "/etc/ssh/authorized_keys.d/nixos", Source: "-", Mode: 0o644},
	}, strings.NewReader("ssh-ed25519 AAAA test\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := debugfsCat(t, disk, "/etc/ssh/ssh_host_ed25519_key"); got != "host key\n" {
		t.Errorf("key = %q", got)
	}
	// The store and the first-boot registration are still there.
	for _, p := range []string{"/nix/store", "/nix-path-registration"} {
		if st := debugfsStat(t, disk, p); !strings.Contains(st, "Inode:") {
			t.Errorf("%s missing after inject:\n%s", p, st)
		}
	}
}
