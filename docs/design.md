# Design

pocketnix runs NixOS as a real `class = "nixos"` host on Android, under the
Android Virtualization Framework (pKVM), with no root. It is a kit any flake
user can consume: an Android app, a Go guest agent, a NixOS module, and a CLI
that provisions a phone over adb. Settled decisions; change them deliberately.

## Shape

- Main use: a terminal on the phone first (mostly touch input), and SSH from
  other machines (e.g. over Tailscale) second. No graphical guest.
- The app is new and small (Compose, no DI framework, no setup wizard). Its AVF
  engine is ported from [Podroid](https://github.com/ExTV/Podroid) (GPL-2.0+),
  hence GPL-3.0+ here. The terminal widget is Termux's terminal-emulator and
  terminal-view (Apache-2.0), vendored from Podroid's copy because a vsock
  session needs changes to Termux's subprocess-bound `TerminalSession`.

## Guest agent

- Go (mdlayher/vsock, creack/pty), built by this flake, run by `nixosModules.default`.
- The app always dials; an AVF guest can't dial an app. One persistent control
  stream (newline-delimited JSON, guest pushes events over it), plus one stream
  per PTY session, per forwarded TCP connection or UDP client, per file operation.
- The protocol is versioned (`go/internal/protocol`) and negotiated in Hello, so
  app and module versions may drift a little.
- Every connection opens with the app's Hello naming its stream (control, pty,
  forward, udp, font); the agent answers with its own Hello or an Error. PTY streams
  carry small binary frames (data, resize, exit status), so a tab needs nothing
  from the control stream; forwards are raw bytes, and UDP streams
  length-prefixed datagrams.
- The agent serves any listener: vsock in the guest, which only admits the
  host (CID 2), and TCP or a unix socket for tests and the debug TCP backend.
- Tabs run `$SHELL -l [-c <profile command>]` as `pocketnix.guest.user`, the
  agent switching credentials itself (no PAM session; the console autologin and
  lingering provide the user manager).
- A switch reloads the agent (SIGHUP) instead of restarting it: tabs are its
  children, and a switch usually runs in one. Config changes reach the app live;
  a new agent binary takes over at the next boot.

## The NixOS module owns the guest's whole contract

- Kernel: Google's prebuilt AVF guest kernel by default, overridable.
- Image: a repart raw disk (ESP + ext4 root) that u-boot → systemd-boot boots.
- Agent service, hvc0 console login, first-boot setup.
- `pocketnix.guest.avf.enable` (on by default) holds the platform half: kernel,
  image, boot loader, first boot, hvc0. Off, the agent half runs under any
  hypervisor, which is how the NixOS test boots it.
- On AVF the module trims the closure by default (flake registry, manuals,
  default packages; dbus, lvm and tpm2 in the initrd), turns zram on and oomd
  off, and sets kernel parameters that let idle vCPUs sleep: every guest
  wakeup is a phone wakeup.
- Configured paths the root agent opens (`files.root`, `share.directory`,
  `terminal.historyFile`, the font) are confined to the user's home when
  inside it, and must be root-owned all the way down otherwise.

## Configuration

- The module owns what changes with the system: port forwards, terminal
  profiles (tab commands), extra-keys rows, theme colors and font file,
  command-palette snippets, the file-provider root. The agent sends them on
  connect and they apply live. Tab notifications follow fixed rules (below).
- The app UI owns set-once hardware needed before boot: RAM, vCPUs, disk size, image.
- The font file is served over its own stream and cached by the app by hash;
  colors use Termux's property names.
- Debug builds can run against an agent over TCP instead of AVF, so the terminal
  UI works on the Android emulator against a desktop agent.

## Provisioning

- `nix run <pocketnix>#provision` over adb: install the release APK matching the
  flake's version, `pm grant` both AVF permissions, build and push the image,
  start the VM. No in-app wizard; the first-run screen only lists what's missing.
- Secrets: `--inject <guest path>=<file|-> --mode <octal>` writes files into
  the ext4 root of a copy of the image (debugfs, unprivileged, never in the
  store) before pushing it, so e.g. agenix has its host key on the first boot.
- The image reaches app-private storage through the app's provision provider
  (`adb shell content call` / `content write`, admitting only the shell user),
  so release builds work without `run-as`. It goes in zstd-compressed chunks
  (`adb push -z`), skipping all-zero ones, and never replaces an existing disk without `--replace`, nor while the
  VM runs.

## Durability

- The VM is cattle: rebuildable from the flake and a binary cache. Consumers
  back up their own state (e.g. /home) from inside the guest.
- One stable release signing key: a signature change forces an uninstall, and
  uninstalling deletes the disk image. The CLI pins its certificate: provision
  refuses a downloaded release any other v2/v3 signer signed.

## Updates and recovery

- The phone never evaluates a flake, not only never builds: evaluating a real
  config needs more memory and time than the guest has. Plain remote builders
  (`nix.buildMachines`) don't help here, since they move builds, never
  evaluation. So every update arrives as a finished toplevel store path, from
  one of two sources (module options; one or both):
  - **Build host (the standard).** The agent SSHes to a build host (any machine
    with Nix that trusts the guest's key, e.g. over Tailscale). The build host
    evaluates and builds the configured flake ref there and prints the toplevel
    path. The agent then copies the closure (`nix copy --from ssh-ng://<host>`,
    or substitutes it when the host pushes to a cache the guest uses).
    `nixosModules.buildHost` is that host's half: an untrusted user whose
    forced command per guest key builds one pinned installable or runs
    `nix-daemon --stdio`, and nothing else.
  - **Published path (cache).** CI evaluates and builds, pushes to a binary
    cache, and publishes the toplevel path as one line of text at a URL. The
    agent fetches the path and realises it with `--max-jobs 0`, substituting only.
    No build host needs to be up.
- With either source, the agent switches to the path
  (`nix-env -p /nix/var/nix/profiles/system --set` + `switch-to-configuration`).
  The app shows update available, Update, Roll back and the generation list.
- Push from a desktop keeps working (`nixos-rebuild --target-host`); the CLI
  offers an ssh ProxyCommand over adb → forward → guest sshd.
- A generation that won't boot: `boot.loader.timeout = 0`; the app's "Start
  with boot menu" taps a key on hvc0 so systemd-boot shows its menu in the
  console tab. Offered automatically when no ready signal arrives in time.
  (nixpkgs' systemd-boot has no boot counting.)
- A check asks the build host first and falls back to the published path, so
  CI covers for a desktop that's off. The build host runs
  `nix build --refresh --no-link --print-out-paths <installable>` over ssh as
  root with the guest's host key, in BatchMode: its host key must be in
  `programs.ssh.knownHosts`. A build host's path is substituted first and
  copied (`--no-check-sigs`: the guest already trusts that host with its whole
  system) only when the caches don't have it, and then through the daemon
  with `--substitute-on-destination`, so only what no cache holds crosses ssh.
- A source's answer must be named `nixos-system-<host>-…`, and the fetched
  system's own `etc/hostname` must be this host before it becomes a generation.
- Update is check, fetch and switch in one; the switch runs
  `switch-to-configuration` in its own unit through `systemd-run`, as
  nixos-rebuild does. Background checks (`updates.checkInterval`, 6 h) only
  report "update available"; nothing applies on its own.
- Generations come from the system profile; rolling back is switching to an
  older one (`nix-env --switch-generation` + `switch`). The app offers a reboot
  when the running system isn't the booted one.
- The update state (sources, operation, available path, log tail,
  generations) is pushed on the control stream; requests are answered in
  order, so an unknown request gets an Error rather than silence.
- The boot menu: the app taps space on the console from the VM's start until
  the kernel's EFI stub speaks (at most 20 s). Space is no entry's hotkey, so
  it only shows the menu. "Offered automatically" is a banner after 2 min
  running without the agent.

## Exposure

- Agent endpoints (PTY, files, control) never leave the app process.
- Port forwards bind 127.0.0.1; a per-forward `lan = true` opts into the LAN.
- A forward is TCP unless `protocol = "udp"`. A UDP forward opens one stream
  per phone-side client address, dropped after 2 minutes without a datagram
  and capped at 64 (UDP sources are trivially forged on a LAN). It's its own
  stream kind, so an agent without it refuses instead of mixing up protocols.

## Lifecycle

- Runs until stopped: started from the app, tile or a terminal; a foreground
  service keeps it up (no wake lock). Stop is graceful via the agent (`systemctl poweroff`)
  with a hard stop after a timeout (30 s). Stop and Reboot ask first. Opt-in
  start on phone boot. A memory balloon returns free guest RAM to Android.
- The app pings a quiet control stream every 15 s and drops it after 45 s of
  silence, so a hung guest shows as an absent agent (and so the boot-menu
  banner), never as Running. Control requests must answer well within that.
- The screen stays on only when asked (a menu toggle, off by default).

## Integrations and UX

- Guest notifications and `xdg-open` reach Android.
- Clipboard: OSC 52 plus an agent CLI; Android → guest on paste or app focus.
- Share sheet → guest inbox or a configured command; a Quick Settings tile.
- A DocumentsProvider exposes a guest directory (default: the user's home)
  read-write in the Files app and every file picker; shows "start the VM"
  while stopped.
- Touch: a native IME composer (autocorrect, dictation; sent as bracketed
  paste), module-defined extra keys with swipe alternates and volume-key
  modifiers, a command palette (module snippets + shell history), and
  session-aware tabs (BEL / OSC 9 / OSC 777 in a background tab → badge +
  notification). When the agent comes up with no running shell tab, the
  default profile's tab opens.
- Guest programs reach the agent on a guest-local unix socket
  (`/run/pocketnix/guest.sock`), which admits root and the session user by
  peer credentials. `pocketnix-guest` speaks it, and the module installs it as
  `xdg-open` and `notify-send` (both on by default, above xdg-utils and
  libnotify). With no app connected a request fails rather than queues.
- `xdg-open <url>` opens on Android; `xdg-open <file>` opens the file through
  the file provider, so only files under its root. From the background
  Android won't start an activity, so both become a notification to tap.
- Clipboard: a guest copy (`pocketnix-guest copy`, or OSC 52 in a tab) sets
  Android's. Android's goes to the agent whenever the app gains focus (the
  only time Android lets it read), and `pocketnix-guest paste` prints it.
- Shares land in `share.directory` (default `~/Inbox`) under a free name,
  owned by the user; `share.command` then runs through /bin/sh on each, the
  path its last argument. Text arrives as a `.txt` file.
- The file provider (`files.root`, default the user's home) goes through an
  `os.Root`: no path or symlink leaves it, and new files belong to the user.
  Opened documents are seekable proxy descriptors served by ranged reads
  (with read-ahead) over one stream per descriptor, so a big file streams
  rather than copies. Writes go to a temporary file next to the target (an
  edit starts from a copy made in the guest at its first write), which the
  writer's close fsyncs and renames over it: readers see the old file or the
  new one, an open that changed nothing changes nothing, and a dropped stream
  discards the writes. The agent records each temporary file under its state
  directory and removes leftovers when it starts. Agents without ranged
  access get local copies, uploaded when a writer closes them (kept for a
  retry when that fails); a copy cut short is an error, never a shorter file.
- The Quick Settings tile starts the VM through ControlActivity (a foreground
  service may only start from the foreground) and stops it gracefully.
- The palette's history is the user's shell history file, read by the agent
  (bash, zsh or fish format, or `terminal.historyFile`), newest first, each
  command once; snippets come first.
- A notification (OSC 9 not starting with a number, which is ConEmu's
  progress; OSC 777;notify) posts unless its tab is in view. A bell only badges
  while the app is open, and posts from the background at most every 30 s a
  tab, since every failed completion rings.

## Distribution

- GitHub Actions builds with Gradle (Nix devshell toolchain), signs with the
  release key from repo secrets, publishes tagged releases; phones update via
  Obtainium. A `v<VERSION>` tag runs the release workflow, which refuses a tag
  that isn't `VERSION`; `versionCode` derives from `VERSION`.

## Milestones

- **M0 boot parity**: module (kernel, image, first boot, agent), the app boots
  the image, console tab, control channel, graceful stop, port forwards,
  foreground service, `provision --inject`.
- **M1 terminal**: PTY tabs over vsock, module theme/font, extra keys, composer.
  The first real deployment cuts over here.
- **M2 integrations**: notifications/open URL, clipboard, share sheet, QS tile,
  DocumentsProvider.
- **M3 NixOS ops**: pull updates (build host first, then published path),
  rollback and generations, boot-menu start,
  command palette, session-aware tabs.
