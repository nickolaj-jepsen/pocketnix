# Agent notes

Read `docs/design.md` first: the decisions there are settled.

## Layout

- `app/`: the Android app (`dev.nickolaj.pocketnix`, `.debug` suffix on debug builds).
- `terminal-emulator/`, `terminal-view/`: Termux's terminal libraries, vendored
  from Podroid's copy. Edit them freely; there is no upstream sync.
- `go/`: one Go module, `cmd/pocketnix-agent` (guest) and `cmd/pocketnix`
  (host CLI), sharing `internal/protocol`.
- `nix/`: the NixOS module (`module.nix`), the Go package, the devshell.

## Commands

```bash
nix develop                      # JDK, Android SDK/NDK, adb, Go
./gradlew assembleDebug          # → app/build/outputs/apk/debug/app-debug.apk
nix build .#pocketnix            # agent + CLI; bump vendorHash in nix/go.nix after go.mod changes
go -C go vet ./... && go -C go test ./...
nix fmt                          # alejandra, before committing Nix
```

Grant a debug install its AVF permissions (`provision` will do this):
`adb shell pm grant dev.nickolaj.pocketnix.debug android.permission.MANAGE_VIRTUAL_MACHINE`
and the same for `android.permission.USE_CUSTOM_VIRTUAL_MACHINE`.

## The test phone

Pixel 8 Pro, GrapheneOS, Android 17. Its real NixOS host (phone-vm) currently
lives in the Podroid fork's debug app (`com.excp.podroid.debug`): never
uninstall it or clear its data, as that deletes the VM's disk. Only one
networked VM can run on the device at a time, so stop Podroid's VM before
testing a networked pocketnix VM, and start it again afterwards.

Never build anything inside a guest: an unthrottled build pegs the whole phone.

## Where M0 code comes from

- AVF engine: Podroid fork, checkout `~/worktrees/Podroid/nixos-image`
  (github.com/nickolaj-jepsen/Podroid, branch `nixos-image`; the disk-image mode
  is its top commit). Port from `app/src/main/java/com/excp/podroid/engine/avf/`
  (`AvfReflect`, `AvfEngine` config building, `VsockControlChannel`,
  `VsockPortForwarder`, `AvfTcpRelay`, `VsockUdpForwarder`, `ConsoleFanout`) and
  `service/PodroidService`. Leave its QEMU, X11, 9p and setup-wizard code behind.
- Image, kernel, first boot: the nixos repo's PR #475 (github.com/nickolaj-jepsen/nixos,
  branch `feat/phone-vm`, checkout `~/worktrees/nixos/phone-vm-nixos`):
  `modules/system/avf.nix` and `pkgs/avf-kernel/`. They move here; the nixos
  repo will consume this flake instead.
- Research behind the decisions, including AOSP/GrapheneOS internals:
  `~/.local/share/agents/phone-vm/termux-avf-fork-research.md`.

## Traps (all verified on the test phone)

- Stock nixpkgs kernels (6.1, 6.12, 6.18) return corrupted virtio-blk reads
  under Pixel pKVM (ext4 checksum errors, random SEGVs). Use Google's prebuilt
  guest kernel (`pkgs/avf-kernel` in the nixos repo).
- Multi-vCPU (`match_host`) guests panic on MPAM without `arm64.nompam` on the
  kernel command line.
- u-boot looks for `/EFI/BOOT/BOOTAA64.EFI`, but `bootctl` (and so every
  `switch`) wants `/EFI/systemd/systemd-bootaa64.efi`; the ESP needs both.
- Raw VM configs need `bootloader` = `/apex/com.android.virt/etc/u-boot.bin`.
- virtmgr can't open files on FUSE (`/sdcard`): disk images live in app-private storage.
- Bulk adb transfers over USB drop the device after ~100–200 MB: push in chunks.
- The `pm grant`s are per Android user and lost on uninstall.
- The AVF API is `@SystemApi` and `VirtualMachineCustomImageConfig` is `@hide`:
  reach them by reflection with in-process HiddenApiBypass.
- Building the image on x86_64 with the guest's own repart tools fails; assemble
  it with the build host's tools but keep the guest's `systemdRepartFlags`
  (`--architecture`). Host-tool repart dereferences top-level symlink store
  paths, so rewrite those `CopyFiles=` lines to `MakeSymlinks=` (see `avf.nix`).
- First-boot setup must order like sd-image (`DefaultDependencies=false`, before
  `sysinit.target`), or it cycles with `nix-daemon.socket`.
- A guest `reboot` stops crosvm with `STOP_REASON_REBOOT`; the app must start the
  VM again then, and leave it stopped on `poweroff`.
