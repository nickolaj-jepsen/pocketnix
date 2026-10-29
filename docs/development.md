# Development

## Setup

```bash
nix develop   # JDK, Android SDK and NDK, adb, Go, alejandra, e2fsprogs (x86_64-linux only)
```

The guest is `aarch64-linux`. On x86_64, building the example image needs
`boot.binfmt.emulatedSystems = ["aarch64-linux"]` or an aarch64 builder.

## Build

| Command | Builds |
|---|---|
| `./gradlew assembleDebug` | `app/build/outputs/apk/debug/app-debug.apk` |
| `nix build .#pocketnix` | agent, guest CLI and host CLI, with the Go tests |
| `nix build .#pocketnix-cli` | the host CLI with `adb` and `debugfs` on its PATH |
| `nix build .#pocketnix-compositor` | the compositor, with its cargo tests |
| `nix build .#example-image` | the example guest's AVF disk image |
| `nix run .#boot-example` | that image under `qemu-system-aarch64`, agent on `vsock:4242:9100` |

After changing `go/go.mod`, update `vendorHash` in `nix/go.nix`.

## Test

```bash
nix flake check                       # every check for this system (avf-boot on x86_64 needs aarch64 builds)
go -C go vet ./... && go -C go test ./...
./gradlew testDebugUnitTest
nix fmt .                             # alejandra; Go code is gofmt'ed
```

| Check | What it covers |
|---|---|
| `format` | alejandra and gofmt |
| `pocketnix`, `pocketnix-compositor` | the Go and cargo test suites |
| `agent` (x86_64) | NixOS VM test: the agent over TCP, updates from a build host and a published path, integrations |
| `gui` (x86_64) | NixOS VM test: `gui.enable`, socket activation, the gui stream, launches, text input |
| `avf-boot` | the real AVF image booted under QEMU TCG, then switched |

- The JVM tests also run against real binaries when given them:
  `POCKETNIX_AGENT=<...>/bin/pocketnix-agent` and
  `POCKETNIX_COMPOSITOR=<...>/bin/pocketnix-compositor` (with foot on PATH).
- Anything under `go/` rebuilds the aarch64 agent, and so the example image.
  Under binfmt that takes a while.
- The Nix sandbox has no `/dev/vhost-vsock`, so the VM tests reach the agent
  over TCP. `nix run .#boot-example` covers vsock.

## Run the app without a phone

Debug builds can talk to an agent over TCP instead of booting a VM, so the UI
runs on the Android emulator against a desktop agent:

```bash
nix build .#pocketnix
./result/bin/pocketnix-agent --config agent.json --listen tcp:0.0.0.0:9100 --no-power
adb shell am start -n dev.nickolaj.pocketnix.debug/dev.nickolaj.pocketnix.ControlActivity \
  -a dev.nickolaj.pocketnix.action.START --es tcp 10.0.2.2:9100
```

`agent.json` is the file the module writes to `/etc/pocketnix/agent.json`;
copy one from a built guest. Never expose a TCP listener beyond a test: it
hands a shell to anyone who reaches it.

`pocketnix agent --connect <tcp:...|unix:...|vsock:<cid>:9100> <command>`
talks to an agent the way the app does (`pocketnix agent -h`).

## Scripted control

`ControlActivity` takes intents over adb (it needs the `DUMP` permission,
which the adb shell has). Send scripted actions there, never to
`MainActivity`, whose launch intent Android replays when it restores the
task.

| Action | Extras |
|---|---|
| `dev.nickolaj.pocketnix.action.START` | `--ez network`, `--ei memoryMb`, `--ei diskGb`, `--ei cpus 1\|0`, `--ez open false` |
| `dev.nickolaj.pocketnix.action.STOP` | |
| debug only: `DEBUG_TAB`, `DEBUG_DUMP`, `DEBUG_LAUNCH` | open and type into a tab, dump tabs to logcat, launch a GUI app |

Debug-only START extras: `--es tcp <host:port>` (`""` for AVF again),
`--ez audio true` (a virtio-snd speaker from the next start), `--ez
guiHashes true` (check every GUI commit's content hash).

With the phone locked, an activity started over adb is never laid out, and
logcat is the only way to see the app; the debug actions exist for that.

## Permissions by hand

`provision` grants these, per Android user; uninstalling loses them:

```bash
adb shell pm grant dev.nickolaj.pocketnix.debug android.permission.MANAGE_VIRTUAL_MACHINE
adb shell pm grant dev.nickolaj.pocketnix.debug android.permission.USE_CUSTOM_VIRTUAL_MACHINE
```

## Releasing

```mermaid
flowchart LR
  bump[bump VERSION] --> tag["git tag v$VERSION"] --> wf[release workflow]
  wf --> sign[build and sign the APK] --> verify[verify against the CLI's pin] --> gh[GitHub release]
  gh --> provision[pocketnix provision]
  gh --> obtainium[Obtainium]
```

- `VERSION` sets the app's `versionName` and `versionCode` and the CLI's
  version. `provision` installs the release of its own version.
- The workflow refuses a tag that doesn't match `VERSION`.
- Secrets: `POCKETNIX_RELEASE_KEYSTORE` (base64 keystore),
  `POCKETNIX_RELEASE_STORE_PASSWORD`, `POCKETNIX_RELEASE_KEY_ALIAS`,
  `POCKETNIX_RELEASE_KEY_PASSWORD`.
- Keep one signing key forever. A new key forces an uninstall, which deletes
  every user's VM disk.

## Releasing from a fork

A fork signs with its own key, so `provision` needs to know it:

- `releaseCertSHA256` in `go/cmd/pocketnix/apksig.go`: the SHA-256 of your
  signing certificate (DER).
- `downloadRelease` in `go/cmd/pocketnix/provision.go`: your repo's release URL.
- Consider your own `applicationId` in `app/build.gradle.kts`, so your build
  and upstream's don't collide on a phone.

Without a release, `provision --apk <file>` installs any APK you built.

## Pitfalls

`AGENTS.md` lists traps verified on real hardware and on the desktop (pKVM
quirks, AVF reflection, adb transfer limits, NixOS test details). Read it
before touching the engine, the image or the module.
