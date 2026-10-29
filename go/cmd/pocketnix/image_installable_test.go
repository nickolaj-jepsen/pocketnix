package main

import (
	"runtime"
	"testing"
)

func TestImageInstallable(t *testing.T) {
	pinned := ".#nixosConfigurations.phone.config.system.build.pocketnixImage.aarch64-linux"
	for _, c := range []struct{ in, goos, goarch, want string }{
		{".#nixosConfigurations.phone", "linux", "amd64", ".#nixosConfigurations.phone.config.system.build.pocketnixImage.x86_64-linux"},
		{`.#nixosConfigurations."phone-vm"`, "linux", "arm64", `.#nixosConfigurations."phone-vm".config.system.build.pocketnixImage.aarch64-linux`},
		{".#example-image", "linux", "amd64", ".#example-image"},
		{pinned, "linux", "amd64", pinned},
		// No pocketnixImage.aarch64-darwin exists; a Mac's Linux builder assembles it.
		{".#nixosConfigurations.phone", "darwin", "arm64", ".#nixosConfigurations.phone.config.system.build.pocketnixImage.aarch64-linux"},
	} {
		if got := imageInstallableOn(c.in, c.goos, c.goarch); got != c.want {
			t.Errorf("%s on %s/%s → %s, want %s", c.in, c.goos, c.goarch, got, c.want)
		}
	}
	if got, want := imageInstallable(".#nixosConfigurations.phone"), imageInstallableOn(".#nixosConfigurations.phone", runtime.GOOS, runtime.GOARCH); got != want {
		t.Errorf("on this host: %s, want %s", got, want)
	}
}
