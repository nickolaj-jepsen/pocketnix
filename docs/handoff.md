# Handoff: M0 and M1, overnight 2026-10-26

Branch `m0-m1`, draft PR #1. Everything below was built and run on the desktop
first; the phone was used last, with networking off, next to phone-vm.

## What works, and how it was verified

| Part | Verified by |
|---|---|
| Agent: control stream, config on connect, ping, poweroff/reboot, PTY sessions (profile as the configured user, resize, exit status, hangup), TCP forwards with half-close, font delivery | Go tests over unix and TCP sockets (`go test ./...`, also run with `-race`, and in the package's checkPhase) |
| Live config: a switch reloads the agent (SIGHUP) and pushes the new config to open control streams; the agent isn't restarted | NixOS VM test (`checks.x86_64-linux.agent`): specialisation switch, same MainPID, new colors arrive on a watching stream |
| Guest module on any hypervisor | NixOS VM test: config, a PTY running a profile as `nixos` with resize, exit status, ssh through a forward stream, the live reload above, poweroff. Passes in CI too |
| AVF image: Google's kernel, ESP with both systemd-boot paths, repart root, first-boot registration, root grows to fill the disk | `nix build .#example-image` on this x86_64 machine; ESP and loader entry inspected; booted under `qemu-system-aarch64` (`nix run .#boot-example`): first boot, `systemctl is-system-running` = running, root grown to 7.4G |
| Agent over real vsock, aarch64 | The QEMU boot above with vhost-vsock: config, PTY (as `nixos`, resize), forward to sshd, graceful poweroff, all from `pocketnix agent --connect vsock:4242:9100` |
| `provision --inject` | Go tests on a synthetic GPT + ext4 image, and `TestRealImage` on the built image (`POCKETNIX_TEST_IMAGE=…`); on the phone the injected authorized key landed with mode 0644, owned by root |
| `provision` adb steps | Go tests against a fake adb on PATH (install, grants, chunked push that never exceeds a push limit, existing-disk and running-VM refusals, start intent); then for real on the phone, twice (once with `--replace`) |
| App protocol, frames, extra-keys names, fn (volume-up) combinations, modifier states, bracketed paste, theme filtering, forward port errors | JVM unit tests (`./gradlew testDebugUnitTest`) |
| App ↔ real agent: control channel, PTY backend (exit status, resize, refused profile), port forwards | JVM integration tests against the Go agent over TCP (`POCKETNIX_AGENT=…`; CI sets it) |
| Terminal UI: tabs (console + several PTY tabs, profile menu), resize when the keyboard opens, extra keys (tap, swipe-up popup, one-shot CTRL), volume-down as Ctrl and volume-up as fn, composer as bracketed paste, module colors and font, live re-theme and new extra-keys rows on reload, the ssh forward | Android emulator (API 36, x86_64) running the debug APK against a desktop agent through the TCP backend; checked with screenshots and transcript dumps |
| On the phone (network off, 768 MB, 1 vCPU): `provision` end to end (install, grants, inject, 1.8 GB pushed in chunks without a USB drop, `--replace`, start), boot via u-boot (agent up 8 s after start), console tab with the whole boot and the hvc0 autologin, control channel over AVF vsock, several PTY tabs at once (`shell` and `htop` profiles, as `nixos`, on Google's kernel), the foreground service, ssh from the desktop through the app's forward (`adb forward tcp:12022 tcp:2022`, authenticated by the injected key), graceful Stop (`STOP_REASON_SHUTDOWN`, service gone) | adb and logcat only (the phone stayed locked), through debug-build intents that open, type into and dump tabs |

## What's left for M0/M1, and why

- **Anything networked** waits for you: only one networked VM runs at a time,
  and phone-vm holds it. Untested: the guest's network, and the headless-GPU
  `crosvm_minimal` workaround (only used with networking).
- **The phone's UI by hand**: the phone was locked all night, so tabs, extra
  keys, the composer and volume keys were checked on the emulator, not on the
  phone's screen. The same APK is installed there.
- **A guest `reboot`**: the app restarts the VM on `STOP_REASON_REBOOT` (ported
  from Podroid), but no run exercised it.
- **Release APK download** in `provision`: there is no release yet, so it
  always needs `--apk`.
- **Resize on the phone**: the locked phone never laid the terminal out, so its
  tabs stayed 80x24; resize was checked in the VM test, under QEMU and on the
  emulator.
- Known limits, not regressions: PTY sessions end if the agent itself
  restarts; tabs don't survive the app process; and an app crash kills the VM
  with it (virtmgr ties the VM to the app's process), so crashes are hard
  power-offs of the guest. One such crash happened on the phone tonight (an
  empty terminal write; fixed).

## Phone steps for you, in order

0. First check phone-vm: Podroid was uninstalled from the phone at 02:57 (see
   Incidents). The steps below assume you've sorted that out.
1. Unlock the phone. pocketnix (debug) is installed with its permissions, and
   its disk holds the example image (ssh forward on 2022) with your desktop key
   at `/etc/ssh/authorized_keys.d/nixos`. Its VM is stopped.
2. To try the UI: open pocketnix, Start (my run left Settings at network off,
   768 MB, 1 vCPU, 4 GB disk; change them there). Check the console tab, open tabs with "+"
   (long-press for the profile menu), extra keys, Compose, volume keys.
3. Stop it from the app (or the notification) before anything networked.
4. Networked test: stop phone-vm in Podroid, then in pocketnix's Settings turn
   Network on and Start; check `ip a` and `ping` in a tab. Stop, and start
   phone-vm again.
5. From the desktop, ssh over adb:
   `adb forward tcp:12022 tcp:2022 && ssh -p 12022 nixos@127.0.0.1`.
6. To provision again from scratch:
   `nix run .#provision -- --serial 3A111FDJG0065F --debug --apk app/build/outputs/apk/debug/app-debug.apk --image .#example-image --inject /etc/ssh/authorized_keys.d/nixos=$HOME/.ssh/id_ed25519.pub --mode 0644 --replace --no-network`

## Decisions I made, for you to review

- Every stream opens with a Hello naming it (control, pty, forward, font); the
  agent answers Hello or Error. PTY streams are binary frames
  ([type][len][payload]: data, resize, exit), so tabs don't depend on the
  control stream.
- The agent's config is a JSON file the module writes to
  `/etc/pocketnix/agent.json`; a switch reloads it (`reloadTriggers`, SIGHUP) and
  never restarts the agent (`restartIfChanged = false`), because tabs are its
  children and a switch usually runs in one. A new agent binary waits for the
  next boot.
- Tabs run `$SHELL -l [-c command]` as `pocketnix.guest.user` (default root),
  with credentials switched by the agent: no PAM session. The pty is chowned to
  the user (group tty, 0620) as login does.
- The vsock listener only accepts the host (CID 2), so guest processes can't
  reach the agent. `agent.listen` can add TCP, documented as test-only.
- The module splits into the agent half and `pocketnix.guest.avf.enable` (on by
  default) for kernel, image, boot loader, first boot and hvc0, so the NixOS
  test can boot the agent half under QEMU.
- `boot.loader.timeout` defaults to 0, as design.md's boot-menu plan wants.
- `provision` pushes through a ContentProvider that only the adb shell user may
  call (`content call` / `content write`), in 32 MiB chunks, skipping all-zero
  ones. It refuses to replace an existing disk without `--replace`, or to write
  while the VM runs. Injected files default to mode 0600.
- The image's `--inject` uses `debugfs image?offset=<root partition>` on a sparse
  copy, finding the root by its GPT/ext4 label `nixos`.
- `TerminalSession` now runs on a `TerminalBackend` (PTY stream or console); the
  JNI subprocess code is gone. The console log (256 KiB) outlives VM runs, so
  the recovery tab shows the previous boot too.
- Debug builds have a TCP agent backend (Settings, or `--es tcp host:port`).
  Scripted control goes through an invisible `ControlActivity` (START with
  `network`, `memoryMb`, `diskGb`, `cpus` extras; STOP; debug-only tab/dump
  hooks), never through the main activity, whose intent the system replays.
- Stop is graceful: the agent's `systemctl poweroff`, then a hard stop after
  30 s. No wake lock: the foreground service only keeps the process alive.
- Volume-down held is Ctrl; volume-up held is Termux's fn (W/A/S/D arrows,
  1–0 F1–F10, …). Extra-key modifiers: tap arms one key, second tap locks.
- The composer's Send pastes (bracketed when the app enabled DECSET 2004) and
  presses Enter; Paste only pastes. ESC and C1 controls are stripped.
- The example forwards ssh on 2022, not 2222: Podroid's forward holds 2222.

## Design doc changes

`docs/design.md` gained bullets (no decision was reversed): the Hello-per-stream
protocol and its listeners, how tabs run, reload-not-restart on switch, the
module's `avf.enable` split, font delivery, the debug TCP backend, the provision
provider, and "no wake lock, 30 s hard stop" under Lifecycle. `AGENTS.md` has
the new commands and desktop-verified traps.

## Incidents

- The first forward check on the phone went to the wrong sshd: Podroid's ssh
  forward holds `[::]:2222`, our app's `127.0.0.1:2222` bind failed silently,
  and my `adb forward` reached phone-vm's sshd, which refused my key. Nothing
  else happened (one failed publickey login in phone-vm's log). The app now
  shows forwards that couldn't listen, and the example uses 2022.
- After an app update, SystemUI restored pocketnix's task from its base intent
  (provision's START) and booted the test VM unasked (network off, 768 MB). I
  stopped it gracefully; ControlActivity fixes the cause.
- **At 02:57:13–02:57:36, Podroid was uninstalled from the phone's own UI**:
  the launcher (Lawnchair) opened the system uninstall dialog for
  `com.excp.podroid`, then for `com.excp.podroid.debug`, and both were
  confirmed (logcat: `UninstallRepository: Uninstalling … com.excp.podroid.debug,
  EXTRA_TARGET_USER_ID=0`, then `Killing 12869:com.excp.podroid.debug … due to
  deletePackageX`). That stopped phone-vm. None of my adb commands did this: my
  last phone command before it was the pocketnix Stop at 02:55, and nothing I
  ran uninstalls anything. Someone was using the phone, so I stopped touching it
  then. Afterwards `pm list packages` still listed both Podroid packages (maybe
  for another Android user). Please check phone-vm's state.
- Until then Podroid was never stopped, restarted, uninstalled or cleared by me;
  its crosvm ran through all my tests. Free space on `/data` went from 12 GB to
  about 10 GB (the pocketnix disk image).
