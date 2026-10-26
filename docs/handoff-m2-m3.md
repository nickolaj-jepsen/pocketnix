# Handoff: M3 and M2, 2026-10-26

Branch `m3-m2`, stacked on `m0-m1` (it carries a cherry-pick of m0-m1's
bootctl fix). The decisions are in `docs/design.md` (Updates and recovery,
Integrations and UX); this lists what was verified and what wasn't.

## Module options added

- `pocketnix.guest.updates.buildHost.{host, flake, installable, sshKey}`,
  `updates.published.url`, `updates.checkInterval` (default `6h`).
- `pocketnix.guest.terminal.snippets` (command palette), `terminal.historyFile`.
- `pocketnix.guest.integrations.{xdgOpen, notifySend}` (both on),
  `share.{directory, command}`, `files.root`.

## What works, and how it was verified

| Part | Verified by |
|---|---|
| Updates: check, fetch and switch from a build host over ssh (copied when the guest's caches lack it), the published path substituted from a cache, the build host first and the published path as fallback, a rejected non-store answer, rollback by switching generations | Go tests against fake ssh/nix/nix-env/switch-to-configuration; the NixOS VM test with a builder node (sshd, nix-serve, nginx publishing the path): a real update over ssh, the fallback with sshd stopped, a rollback, each checking the switched system's marker file |
| Command palette history (bash, zsh with metafied bytes and continuations, fish) and snippets | Go tests; the VM test reads a real `.bash_history`; JVM tests for the filter; on the emulator: snippets first, history deduplicated, filtering, insert as bracketed paste |
| Session-aware tabs: OSC 9 / OSC 777;notify and bells → badge and notification | Emulator tests for the OSC parsing; JVM tests for the notify policy; on the emulator: a background tab's OSC 777 badged it and posted "build: all targets built"; its bell, with the app open, only badged |
| Guest socket: notify-send, xdg-open, copy/paste; only root and the session user | Go tests; the VM test (as `nixos`, and `nobody` refused); JVM integration test against the real agent; on the emulator: the notification posted, a URL opened in Chrome, a guest copy reached Android's clipboard |
| Share sheet → `~/Inbox`, share command, text shares | Go tests (free names, the command runs as the user); VM test (owned by the user); emulator: a text share landed as a `.txt` |
| File provider: list, read, write (mode kept), create (free names), rename, delete; symlinks and `..` can't leave the root | Go tests; VM test (`put` owned by `nixos`, `../../etc/shadow` refused); JVM integration test; emulator: the Files app lists the root and `docs`, New folder made it on the guest side |
| Quick Settings tile | Emulator: stop, and start without opening the app (the service runs in the foreground) |
| The bootctl fix (every switch installs with `--no-variables`) | Under QEMU on Google's kernel: plain `bootctl status` exits 1, a real `switch-to-configuration boot` exits 0 with both systemd-boot paths and the entry in place; on phone-vm by the nixos session |

## Not verified

- Nothing in M2/M3 ran on the phone: phone-vm now lives in the same debug
  app, and installing an APK restarts the app, which kills its VM.
- **Boot-menu start** taps space on the console until the kernel's EFI stub
  speaks. AVF's u-boot has a virtio-console driver, so the keys likely reach
  systemd-boot, but whether they do, and whether u-boot's own autoboot check
  swallows them, is untested. The QEMU path (edk2) differs from u-boot.
- A real build host or CI publishing for phone-vm: the VM test covers the
  mechanics; your build host's ssh trust and known-hosts entry are config.
- `xdg-open <file>` from the background (a notification to tap) and opening
  a guest file in an Android app through the provider.

## The nixos session's review

`~/.local/share/agents/pocketnix/review-2026-10-26.md`, 33 items. Done on this
branch, each with a test where one fits (Go, JVM, emulator fuzz, NixOS VM test):

- P0 crashes (1–6): OSC 104 and a wide character on a one-column screen
  (found by the new fuzz test); main-thread input never blocks; worker
  threads and coroutines log instead of crashing; clipboard cap; FGS order.
- P1 security (7–11, 13, 14): ControlActivity needs DUMP; settings bounded by
  free space; shares only from other apps' `content:` URIs, once; history,
  shares and file writes confined with `os.Root`, owners kept; published path
  https-only, `--max-jobs 0`, this host's toplevel name only; the tile asks
  for an unlock; open notifications keep their URL.
- P1 robustness (15–23): updater timeouts, keepalives, cancel, and rollback
  preempting a hung check; provision checks a running VM before installing,
  verifies the image's sha256 at commit, and cleans up on Ctrl-C; sized file
  writes; failed saves kept and retried; refused FGS shown and retried;
  control writes time out; hung-up sessions killed after 10 s; Hello carries
  a version range (`Negotiate`) and requests an id.
- Phone (25–28): dhcpcd skips dummy/ifb, no tty1 getty, version 0.1.0 from
  `VERSION`, a reboot reads the current settings.
- UX and deploy (31–33 and the last section): provision `--memory/--cpus/--disk`
  and a disk at least the image's size; `--image .#nixosConfigurations.<host>`
  picks this machine's `pocketnixImage.<system>`; `theme.fontFamily`;
  confirmations for closing a running tab and a boot-menu restart; a bigger ×;
  the late-agent banner only for an agent that's down; failed forwards retried;
  opt-in start on phone boot; the release build's R8 rules (it now minifies).

Not done:
- 12, a restricted build-host key (`command=` in authorized_keys): a design
  change for you to decide; the build host still trusts the guest's host key.
- 24, the memory balloon: AVF's custom-image config may have a switch for it,
  which needs a look on the phone.
- 29, the image size: the slack is repart's `Minimize=guess` (~30% free in
  the example's root). The phone's copy stays sparse and zero chunks aren't
  pushed, so it costs push time and desktop store space, not phone storage.
- 31, `keepScreenOn` is still always on while the app is visible.
- CI for a switch with `avf.enable` on (it would have caught bootctl); a CI
  APK or tagged pre-release.
- The minified release build hasn't run on a phone, and none of this batch
  ran on the phone at all.

## Known gaps

- "Notification rules" from design.md's Configuration list aren't defined;
  tab notifications follow fixed rules (design.md).
- The update log is the last 60 lines; there's no cancel and no timeout on
  the build host's command (see the review below).
- The nixos session's review (`~/.local/share/agents/pocketnix/review-2026-10-26.md`)
  lists crashes and hardening items, which come next.
