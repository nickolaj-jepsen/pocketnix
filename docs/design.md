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
  per PTY session, per port forward, per file operation.
- The protocol is versioned (`go/internal/protocol`) and negotiated in Hello, so
  app and module versions may drift a little.

## The NixOS module owns the guest's whole contract

- Kernel: Google's prebuilt AVF guest kernel by default, overridable.
- Image: a repart raw disk (ESP + ext4 root) that u-boot → systemd-boot boots.
- Agent service, hvc0 console login, first-boot setup.

## Configuration

- The module owns what changes with the system: port forwards, terminal
  profiles (tab commands), extra-keys rows, theme colors and font file,
  notification rules, command-palette snippets, the file-provider root. The
  agent sends them on connect and they apply live.
- The app UI owns set-once hardware needed before boot: RAM, vCPUs, disk size, image.

## Provisioning

- `nix run <pocketnix>#provision` over adb: install the release APK matching the
  flake's version, `pm grant` both AVF permissions, build and push the image,
  start the VM. No in-app wizard; the first-run screen only lists what's missing.
- Secrets: `--inject <guest path>=<file|-> --mode <octal>` writes files into
  the ext4 root of a copy of the image (debugfs, unprivileged, never in the
  store) before pushing it, so e.g. agenix has its host key on the first boot.

## Durability

- The VM is cattle: rebuildable from the flake and a binary cache. Consumers
  back up their own state (e.g. /home) from inside the guest.
- One stable release signing key: a signature change forces an uninstall, and
  uninstalling deletes the disk image.

## Updates and recovery

- Pull: module option for the flake ref to follow. The agent checks whether the
  new toplevel is fully substitutable (`--max-jobs 0`, the phone never builds);
  the app shows update available, Update, Roll back and the generation list.
- Push from a desktop keeps working (`nixos-rebuild --target-host`); the CLI
  offers an ssh ProxyCommand over adb → forward → guest sshd.
- A generation that won't boot: `boot.loader.timeout = 0`; the app's "Start
  with boot menu" taps a key on hvc0 so systemd-boot shows its menu in the
  console tab. Offered automatically when no ready signal arrives in time.
  (nixpkgs' systemd-boot has no boot counting.)

## Exposure

- Agent endpoints (PTY, files, control) never leave the app process.
- Port forwards bind 127.0.0.1; a per-forward `lan = true` opts into the LAN.

## Lifecycle

- Runs until stopped: started from the app, tile or a terminal; a foreground
  service keeps it up. Stop is graceful via the agent (`systemctl poweroff`)
  with a hard stop after a timeout. Opt-in start on phone boot. A memory
  balloon returns free guest RAM to Android.

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
  notification).

## Distribution

- GitHub Actions builds with Gradle (Nix devshell toolchain), signs with the
  release key from repo secrets, publishes tagged releases; phones update via
  Obtainium.

## Milestones

- **M0 boot parity**: module (kernel, image, first boot, agent), the app boots
  the image, console tab, control channel, graceful stop, port forwards,
  foreground service, `provision --inject`.
- **M1 terminal**: PTY tabs over vsock, module theme/font, extra keys, composer.
  The first real deployment cuts over here.
- **M2 integrations**: notifications/open URL, clipboard, share sheet, QS tile,
  DocumentsProvider.
- **M3 NixOS ops**: pull updates, rollback and generations, boot-menu start,
  command palette, session-aware tabs.
