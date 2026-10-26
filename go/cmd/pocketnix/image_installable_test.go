package main

import (
	"runtime"
	"testing"
)

func TestImageInstallable(t *testing.T) {
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	pinned := ".#nixosConfigurations.phone.config.system.build.pocketnixImage.aarch64-linux"
	for in, want := range map[string]string{
		".#nixosConfigurations.phone":      ".#nixosConfigurations.phone.config.system.build.pocketnixImage." + arch + "-linux",
		`.#nixosConfigurations."phone-vm"`: `.#nixosConfigurations."phone-vm".config.system.build.pocketnixImage.` + arch + "-linux",
		".#example-image":                  ".#example-image",
		pinned:                             pinned,
	} {
		if got := imageInstallable(in); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
	// No pocketnixImage.aarch64-darwin exists; a Mac's Linux builder assembles it.
	if got, want := imageInstallableOn(".#nixosConfigurations.phone", "darwin", "arm64"), ".#nixosConfigurations.phone.config.system.build.pocketnixImage.aarch64-linux"; got != want {
		t.Errorf("on a Mac: %s, want %s", got, want)
	}
}
