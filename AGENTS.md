# Agent notes

Read `docs/architecture.md` first: its design rules are settled. The docs
index is in `README.md`; building, testing and the app's scripted control are
in `docs/development.md`.

## Layout

`docs/architecture.md` maps the directories. Beyond it:

- The app is `dev.nickolaj.pocketnix`; debug builds add `.debug`.
- `terminal-emulator/`, `terminal-view/` are vendored (Termux, by way of
  Podroid): edit them freely; there is no upstream sync.

## Working rules

- CI builds the flake's checks, per system (`.github/workflows/ci.yml`); `nix flake check` runs them all.
- Run `nix fmt` before committing: it formats Nix, Go and Kotlin (`nix/treefmt.nix`;
  ktlint's rules are in `.editorconfig`). The compositor isn't rustfmt'ed, so
  don't reformat it wholesale.
- Never build anything inside a guest: an unthrottled build pegs the whole phone.
- Uninstalling the app, clearing its data or `provision --replace` deletes
  the VM's disk: never do it to a phone's working VM without asking.

## Keeping docs and tests from drifting

Copies of code in docs and tests have drifted here repeatedly; keep one source.

- Docs explain why and how to use; they name the source of truth instead of
  copying it. Module options and their defaults live in `nix/module.nix`'s
  descriptions, stream kinds and message fields in `go/internal/protocol`,
  checks and packages in `flake.nix`, `ControlActivity`'s extras in its
  `EXTRA_*` constants. No timeouts, limits or counts that a constant holds.
- `docs/gui-protocol.md` is the exception: it is the wire spec, and code
  matches it. Code cites its headings (and `docs/architecture.md`'s) by name,
  so grep before renaming one.
- Traps live in this file only; other docs point here.
- Tests assert the facet they're named for: an error's type or key token, not
  its wording; fixture entries by name, not position or count; a condition
  waited on, not a sleep. Expected values are literal examples, not
  recomputed the way the code computes them.
- NixOS tests read expected config from `nodes.<node>.pocketnix.*`, or set
  their own, instead of copying the module's defaults or `example/configuration.nix`.

## Comments

- A comment holds what the code can't: a gotcha, constraint, tradeoff or
  unit. One line where it fits; never narrate the line below it.
- Point at other code by symbol (`[TouchMode]`, `RESCAN_DEBOUNCE`), not by
  file, line, "below" or "above". Name a constant instead of copying its value.
- No lists of callers, cases or options kept elsewhere; they drift.
- No history: plans, steps, spikes, "now", "no longer". Git holds it; keep
  only the lasting why, in the present tense.
- A value hand-copied across languages names its other side in a comment.
- Doc comments state the contract (behaviour, errors, invariants), not the
  steps that implement it.

## Traps (verified on a Pixel 8 Pro, GrapheneOS, Android 17)

- Stock nixpkgs kernels (6.1, 6.12, 6.18) return corrupted virtio-blk reads
  under Pixel pKVM (ext4 checksum errors, random SEGVs). Use Google's prebuilt
  guest kernel (`nix/avf-kernel`).
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
  virtmgr also requires `hypervisor.memory_reclaim.supported` (the Pixel 8
  Pro has it), and logs "Memory balloon not enabled" otherwise.
- The `pm grant`s are per Android user and lost on uninstall.
- The AVF API is `@SystemApi` and `VirtualMachineCustomImageConfig` is `@hide`:
  reach them by reflection with in-process HiddenApiBypass. Its
  `GpuConfig.Builder` takes boxed `Boolean`s on GrapheneOS 17, so a
  `boolean.class` lookup throws (`AvfReflect.set` tries both).
- A third-party VM can have the phone's GPU, but nothing yet composites on it.
  `GpuConfig(backend=gfxstream, vulkan=true, egl=false, gles=false, glx=false,
  surfaceless=true, context-types=[gfxstream-vulkan, gfxstream-composer])`
  (the Terminal app's own config) reaches crosvm as `--gpu=…`, and the guest's Mesa gfxstream ICD then runs
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
  paths, so rewrite those `CopyFiles=` lines to `MakeSymlinks=` (see `nix/module.nix`).
- First-boot setup must order like sd-image (`DefaultDependencies=false`, before
  `sysinit.target`), or it cycles with `nix-daemon.socket`.
- A guest `reboot` stops crosvm with `STOP_REASON_REBOOT`; the app must start the
  VM again then, and leave it stopped on `poweroff`.
- The VM dies with the app's process (virtmgr ties it to the app): an app crash
  is a hard power-off of the guest.
- Forwards share the phone's ports with other apps: another app holding
  `[::]:<port>` makes ours fail to listen, and `adb forward` reaches theirs.
- Restoring a task relaunches it from its base intent: an adb `am start` with an
  action, sent to the main activity, gets replayed (it booted the VM after an
  app update). Scripted actions go to `ControlActivity`, which has no history.
- With the phone locked, an activity started over adb is never laid out, so a
  TerminalView never sizes its session, and nothing on screen shows the app.
  `pocketnix control` (debug builds' `ControlProvider`; how to use it:
  `.agents/skills/pocketnix-control/SKILL.md`) opens tabs, types into them and
  reads back their transcripts and the GUI state.
- Termux's `ByteQueue` throws on a zero-length write; keep empty writes out.
- Google's kernel has no efivarfs but has `/sys/firmware/efi`: `bootctl status`
  (the version check of a plain `switch`) exits 1 without a word, while
  `bootctl --no-variables …` works. The module makes every switch install.
- An app can't `FIONREAD` a vsock fd, so `available()` on its stream throws,
  and a `BufferedInputStream` calls it after every short read: big PTY output
  and forwards died. `AgentLink`'s buffer hides it; TCP tests can't catch it.
- GrapheneOS refuses perf events for app processes, from adb shell and under
  `run-as` alike: simpleperf can't profile the app (`--app` waits forever for
  its process). Per-thread `/proc` stat is what there is.
- logind grants `/dev/snd` only to sessions on a seat; the console on hvc0,
  tabs and the lingering user manager (PipeWire's) have none, so without the
  `audio` group PipeWire sees no card and only root can play.
- crosvm hands AAudio S32LE as 24-bit packed, so every sample misaligns into
  static: the module pins virtio_snd to S16LE.

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
- A change to the guest's Go source (which files that leaves out: `nix/go.nix`)
  rebuilds the aarch64 agent under binfmt and so the whole example image:
  budget ~15 minutes.
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
- `gui.enable` sets `nixpkgs.overlays`, which a NixOS test's read-only pkgs
  refuse: such a test needs `node.pkgsReadOnly = false`.
