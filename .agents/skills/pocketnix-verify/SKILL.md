---
name: pocketnix-verify
description: Pick and run the checks that cover a change in pocketnix — the fast loop per area (Go, compositor, app, NixOS module), the Nix check CI runs for it, the NixOS VM tests interactively, the example guest under QEMU, the app on the emulator, and when to go to a device. Use after editing code and before committing, when asked which tests cover something, or when a check fails.
---

# Checking a change

Iterate with the fast loop, then build the checks CI builds for what you
touched. The checks are `checks` in `flake.nix`; build one with
`nix build -L .#checks.<system>.<name>`. `.github/workflows/ci.yml` says which
system runs which.

## By area

Fast loops run inside `nix develop`.

| changed | while editing | before committing |
|---|---|---|
| `go/` | `go -C go test ./internal/<pkg> -run <Test>` | the `go` check |
| `compositor/` | `cargo test` in `compositor/` | the `compositor` check, and `gui` when the app-facing behaviour changed |
| `app/` | `nix run .#gradle -- testDebugUnitTest --tests '*<Name>*'` | `nix run .#gradle -- testDebugUnitTest lintDebug` |
| `nix/`, `example/` | build the one VM test that covers it | `agent`, `gui`, and `avf-boot` for boot or image changes |
| the wire (`go/internal/protocol`, `docs/gui-protocol.md`) | both sides' tests | the `go` and `compositor` checks, and the app's tests through `nix run .#gradle`, which run them against the real agent and compositor |

Then `nix fmt`.

Each VM test's file under `nix/` opens with what it covers. They need KVM
and run on x86_64; `avf-boot` boots the real image under TCG and on x86_64
needs aarch64 builds.

## What it costs

The example image rebuilds whenever the guest's closure changes: the guest's
Go source (not its tests, not `cmd/pocketnix`; `nix/go.nix` says what it
leaves out), the module, the example, or the AVF kernel. On x86_64 the
aarch64 parts build under binfmt (`go/AGENTS.md` has the budget), so batch
those changes before building the image or `avf-boot`.

## A VM test interactively

```bash
nix run .#checks.x86_64-linux.agent.driverInteractive
```

opens the test driver's Python prompt: `start_all()`, then the test script's
calls one at a time, `run_tests()` for all of it, or
`<node>.shell_interact()` for a shell on a node. Nix caches a passing test;
`nix build --rebuild` reruns one.

## Without a phone

- `nix run .#boot-example` boots the example image under QEMU and prints the
  agent's vsock address for `pocketnix agent --connect`.
- `nix run .#dev-agent` runs the example's agent on this machine for a debug
  app on the Android emulator (`docs/development.md`, "Run the app without a
  phone").

## On a device

Use the pocketnix-control skill: it drives tabs, reads transcripts and GUI
state, and `pocketnix control logcat` prints the app's log. A phone may hold
a working VM: read `AGENTS.md`'s working rules before installing anything.

## When a check fails

- `nix log <drv>` prints a failed build's whole log.
- A `vendorHash` mismatch after changing `go/go.mod`: put the hash Nix printed
  into `nix/go.nix`. The old hash can still name an older store path that Nix
  reuses without building, so set `lib.fakeHash` first if a build fails on
  missing modules.
- `format`: run `nix fmt`.
