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
nix develop                      # JDK, Android SDK/NDK, adb, Go, e2fsprogs
./gradlew assembleDebug          # → app/build/outputs/apk/debug/app-debug.apk
./gradlew testDebugUnitTest      # JVM tests; POCKETNIX_AGENT=<…/bin/pocketnix-agent> adds the ones against a real agent
nix build .#pocketnix            # agent + CLI; bump vendorHash in nix/go.nix after go.mod changes
go -C go vet ./... && go -C go test ./...
nix build .#checks.x86_64-linux.agent   # NixOS VM test: the example guest's agent, over TCP
nix build .#example-image        # the AVF disk image (aarch64 through binfmt, assembled natively)
nix run .#boot-example           # that image under qemu-system-aarch64, agent on vsock:4242:9100
nix fmt .                        # alejandra, before committing Nix
```

`pocketnix agent --connect <tcp:…|unix:…|vsock:<cid>:9100> config|exec|forward|…`
talks to an agent the way the app does. A desktop agent for the app's debug TCP
backend: `pocketnix-agent --config <file> --listen tcp:0.0.0.0:9100 --no-power`,
then start the app with `--es tcp 10.0.2.2:9100` (the emulator's host).

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
- The VM dies with the app's process (virtmgr ties it to the app): an app crash
  is a hard power-off of the guest.
- Podroid's ssh forward holds `[::]:2222`, so a pocketnix forward on 2222 can't
  listen, and `adb forward … tcp:2222` reaches phone-vm instead.
- Restoring a task relaunches it from its base intent: an adb `am start` with an
  action, sent to the main activity, gets replayed (it booted the VM after an
  app update). Scripted actions go to `ControlActivity`, which has no history.
- With the phone locked, an activity started over adb is never laid out, so a
  TerminalView never sizes its session; and logcat (`-c` too) is the only way to
  see the app. Debug builds' `DEBUG_TAB` / `DEBUG_DUMP` actions are for this.
- Termux's `ByteQueue` throws on a zero-length write; keep empty writes out.
- Google's kernel has no efivarfs but has `/sys/firmware/efi`: `bootctl status`
  (the version check of a plain `switch`) exits 1 without a word, while
  `bootctl --no-variables …` works. The module makes every switch install.

## Traps (verified on the desktop)

- Google's AVF kernel has no ACPI: under QEMU it needs `-M virt,acpi=off`, or
  edk2 boots it with an empty device tree and it hangs silently.
- The Nix build sandbox has no `/dev/vhost-vsock`, so the NixOS test reaches the
  agent over TCP; `nix run .#boot-example` (outside the sandbox) covers vsock.
- Go defers closing a PTY master while another goroutine is blocked reading it,
  so closing it doesn't hang the session up: the agent signals SIGHUP itself.
- The CLI's pure-Go resolver asks DNS before /etc/hosts under NixOS's nsswitch
  order: in NixOS tests, connect by IP.
- Android creates content providers before `Application.onCreate`: anything the
  provision provider reaches must not depend on onCreate having run.
- `nix fmt` with no path formats stdin; pass `.`.
- Any change under `go/`, tests included, rebuilds the aarch64 agent under
  binfmt and so the whole example image: budget ~15 minutes.
- The Nix-built QEMU breaks if the shell leaks an `LD_LIBRARY_PATH` from a
  newer glibc; `boot-example` unsets it.
