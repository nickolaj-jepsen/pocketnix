# Agent notes

Read `docs/design.md` first: the decisions there are settled.

## Layout

- `app/`: the Android app (`dev.nickolaj.pocketnix`, `.debug` suffix on debug builds).
- `terminal-emulator/`, `terminal-view/`: Termux's terminal libraries, vendored
  from Podroid's copy. Edit them freely; there is no upstream sync.
- `go/`: one Go module, `cmd/pocketnix-agent` (guest) and `cmd/pocketnix`
  (host CLI), sharing `internal/protocol`.
- `compositor/`: the guest's Wayland compositor (Rust, from wprs's server
  half, Apache-2.0), speaking `docs/gui-protocol.md` to the app.
- `nix/`: the NixOS module (`module.nix`), the Go and compositor packages, the devshell.

## Commands

```bash
nix develop                      # JDK, Android SDK/NDK, adb, Go, e2fsprogs
./gradlew assembleDebug          # → app/build/outputs/apk/debug/app-debug.apk
./gradlew testDebugUnitTest      # JVM tests; POCKETNIX_AGENT=<…/bin/pocketnix-agent> adds the ones against a real agent,
                                 # POCKETNIX_COMPOSITOR=<…/bin/pocketnix-compositor> the GUI decoder against a real compositor and foot
nix build .#pocketnix            # agent + CLI; bump vendorHash in nix/go.nix after go.mod changes
nix build .#pocketnix-compositor # the compositor, with its cargo tests
nix build .#checks.x86_64-linux.gui     # NixOS VM test: gui.enable, socket activation, the agent's gui stream
go -C go vet ./... && go -C go test ./...
nix build .#checks.x86_64-linux.agent   # NixOS VM test: the example guest's agent, over TCP
nix build .#checks.x86_64-linux.avf-boot  # the example image (avf.enable on) booted and switched under QEMU TCG
nix build .#example-image        # the AVF disk image (aarch64 through binfmt, assembled natively)
nix run .#boot-example           # that image under qemu-system-aarch64, agent on vsock:4242:9100
nix fmt .                        # alejandra, before committing Nix
```

`pocketnix agent --connect <tcp:…|unix:…|vsock:<cid>:9100> config|exec|forward|udp|update|switch-generation|history|…`
talks to an agent the way the app does; the update commands wait for the
operation and print its log and final state. A desktop agent for the app's debug TCP
backend: `pocketnix-agent --config <file> --listen tcp:0.0.0.0:9100 --no-power`,
then start the app with `--es tcp 10.0.2.2:9100` (the emulator's host).

Grant a debug install its AVF permissions (`provision` will do this):
`adb shell pm grant dev.nickolaj.pocketnix.debug android.permission.MANAGE_VIRTUAL_MACHINE`
and the same for `android.permission.USE_CUSTOM_VIRTUAL_MACHINE`.

## The test phone

Pixel 8 Pro, GrapheneOS, Android 17. Its real NixOS host (phone-vm) lives in
pocketnix's own debug app (`dev.nickolaj.pocketnix.debug`), running the
`wayland-poc` build: never uninstall it, clear its data or provision over it,
as that deletes the VM's disk. To test a build there, back up the installed
APK (`pm path` + `adb pull`), stop the VM through `ControlActivity`, `adb
install -r -d` (`-d`: versionCode now comes from VERSION), and reinstall the
backup the same way afterwards.

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
  An uncompressed `adb exec-in` drops it even at 64 MiB (`UsbFfs: Error while
  reading USB block`), and a reconnect loses every `adb forward`.
- Taking a guest vCPU offline works, but bringing it back can fail under pKVM
  ("CPU4: died during early boot") until the VM restarts.
- The balloon needs `useAutoMemoryBalloon` in the custom-image config;
  virtmgr also requires `hypervisor.memory_reclaim.supported` (the test
  phone has it), and logs "Memory balloon not enabled" otherwise.
- The `pm grant`s are per Android user and lost on uninstall.
- The AVF API is `@SystemApi` and `VirtualMachineCustomImageConfig` is `@hide`:
  reach them by reflection with in-process HiddenApiBypass. Its
  `GpuConfig.Builder` takes boxed `Boolean`s on GrapheneOS 17, so a
  `boolean.class` lookup throws (`AvfReflect.set` tries both). Before that
  fix the networked default's surfaceless setter never applied, so the
  default stays `backend=2d` alone, the config the phone ran.
- A third-party VM can have the phone's GPU, but nothing yet composites on it.
  `GpuConfig(backend=gfxstream, vulkan=true, egl=false, gles=false, glx=false,
  surfaceless=true, context-types=[gfxstream-vulkan, gfxstream-composer])`
  (the Terminal app's own config, the debug build's `--es gpu gfxstream`)
  reaches crosvm as `--gpu=…`, and the guest's Mesa gfxstream ICD then runs
  `vulkaninfo` against the real GPU ("Virtio-GPU GFXStream (Mali-G715)").
  Leaving `egl`/`gles` unset is not equivalent: virtmgr defaults them true and
  the guest's ASG stream then dies on `mmap64: Invalid argument`. Two blockers
  stop a compositor anyway: the ICD lacks `VK_EXT_physical_device_drm`
  (wlroots' Vulkan renderer refuses the device) and zink needs robustness2's
  `nullDescriptor` (also absent). `backend=virglrenderer` answers "No virgl
  contexts available on host", and adding `venus` panics crosvm's `v_gpu`
  thread (SIGABRT).
- Only the display *surface* handoff is sealed to `vmlauncher_app` (sepolicy),
  which is why the guest gets a render node but no scanout. GrapheneOS's own
  Terminal app is not a better reference: its `GraphicsManager` offers only
  Lavapipe (software) and Gfxstream, and `gfxstream_supported` is a device
  overlay. On the Pixel 8 Pro its GUI renders in software, while our app still
  gets a working gfxstream device.
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
- An app can't `FIONREAD` a vsock fd, so `available()` on its stream throws,
  and a `BufferedInputStream` calls it after every short read: big PTY output
  and forwards died. `AgentLink`'s buffer hides it; TCP tests can't catch it.

## Traps (verified on the desktop)

- Google's AVF kernel has no ACPI: under QEMU it needs `-M virt,acpi=off`, or
  edk2 boots it with an empty device tree and it hangs silently.
- The Nix build sandbox has no `/dev/vhost-vsock`, so the NixOS test reaches the
  agent over TCP; `nix run .#boot-example` (outside the sandbox) covers vsock.
- Go defers closing a plain `*os.File` while another goroutine is blocked
  reading it, so the agent moves the PTY master into Go's poller (`pollable`):
  only then does closing it end the read and hang up background children.
- The CLI's pure-Go resolver asks DNS before /etc/hosts under NixOS's nsswitch
  order: in NixOS tests, connect by IP.
- Android creates content providers before `Application.onCreate`: anything the
  provision provider reaches must not depend on onCreate having run.
- `nix fmt` with no path formats stdin; pass `.`.
- Any change under `go/`, tests included, rebuilds the aarch64 agent under
  binfmt and so the whole example image: budget ~15 minutes.
- The Nix-built QEMU breaks if the shell leaks an `LD_LIBRARY_PATH` from a
  newer glibc; `boot-example` unsets it.
- A NixOS test node that really switches (`switch`, not `test`) needs
  `boot.loader.grub.enable = false`: the test VM's GRUB can't install. Test
  VMs also boot without a system profile; create one before testing generations.
- A guest system for a test's later generation can't be named in that guest's
  own config (its path would depend on itself): build it with `extendModules`
  into `system.build`, and put it on another node.
- With a substituter that doesn't answer, Nix retries 5 times with backoff
  (~5 s) before giving up; the update log shows the warnings.
- The agent is root and touches user-owned paths (history, the share
  directory, the file provider): go through an `os.Root`, which also refuses
  absolute symlinks even when they point back inside. A configured path is
  opened with `configuredDir`/`configuredFile`, never `os.OpenRoot` on it.
- systemd 260's autologin no longer prints "(automatic login)": a console
  script (`avf-boot`) waits for the shell prompt instead. Console lines ending
  in `\r` show up blank in Nix logs, and expect's `log_user 0` also stops
  `log_file`.
- `/boot` is mounted umask 0077: checking it in a guest needs root.
- A host-folder mount (FUSE, no `allow_other`) admits only the session user,
  not root: check it with `runuser`, and `findmnt` rather than `mountpoint`.
  go-fuse's own mounting probes the mount from its process, so the agent
  mounts itself and hands go-fuse the descriptor as `/dev/fd/N`.
- An unsigned release build must have no `release` signing config at all;
  `build.gradle.kts` only makes one when a keystore is given.
- A hidden app doesn't redraw tabs (`Tabs.visible`): anything that inspects
  the view rather than the transcript sees stale output.
- Any uncaught exception on an app thread kills the process and so the VM;
  `PocketnixApp` logs worker-thread ones instead, and guest output goes
  through a fuzzed emulator (`FuzzTest`).
- An agent push with a top-level `"id"` never reaches `GuestBridge`: the
  app's `ControlChannel` and the Go client take it for a late answer to a
  request. Name event ids otherwise (notify uses `notification`).
- Tabs have no login session, so without lingering there is no user manager
  and no `/run/user/<uid>/bus`; the module lingers the user, which brings the
  session bus up with no login (the NixOS test checks it).
- NixOS gives every unit, user units too, a minimal default `PATH` that
  replaces the user manager's: a unit that must find the profiles' programs
  (the compositor's `Exec`/`TryExec`) needs `enableDefaultPath = false`.
- `systemd-run --user` finds the user manager through `XDG_RUNTIME_DIR`
  (`DBUS_SESSION_BUS_ADDRESS` alone isn't enough): a desktop compositor
  started with a private runtime dir can't launch anything ("Failed to
  connect to user scope bus via local transport").
- NixOS leaves polkit off, and without it logind refuses every sleep
  inhibitor but root's ("Access denied"). With it, a user without a logind
  session (a tab, `runuser`, a unit's `User=`) still needs authentication
  for `inhibit-block-sleep`: `wakeLocks.enable` turns polkit on and allows
  `pocketnix.guest.user`. logind (systemd 260) does signal `BlockInhibited`
  and `NCurrentInhibitors` changes.
- "The default network is validated" overreports: a VPN that never goes away
  (Tailscale) stays the app's validated default network with no underlying
  link (`UnderlyingNetworks: []`), so with Wi-Fi and mobile data off
  `host-state` keeps saying online (and metered, from the last network) and
  `pocketnix-online.target` stays active until the VPN app is stopped. The
  VM's own `avf_tap_fixed` is a validated ethernet network without INTERNET
  too.
