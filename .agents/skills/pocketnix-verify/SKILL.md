---
name: pocketnix-verify
description: Pick and run the checks that cover a change in pocketnix — the fast loop per area (Go, compositor, app, NixOS module), the Nix check CI runs for it, the NixOS VM tests interactively, the example guest under QEMU, the app on the emulator, and when to go to a device. Use after editing code and before committing, when asked which tests cover something, or when a check fails.
---

# Checking a change

Iterate with the fast loop, then build the checks CI builds for what you
touched. The checks are `checks` in `flake.nix`; build one with
`nix build -L .#checks.<system>.<name>`. `.github/workflows/ci.yml` says which
system runs which.

## Fast first

Start at the cheapest rung that sees the change, and go up only when that rung
can't see it. Cheapest first:

1. One Go test or `cargo test`, in seconds.
2. One filtered gradle test, in about 10 s once warm.
3. One Nix check; VM tests take 1–3 min.
4. `bench --local`, for speed measured on the desktop's CPU.
5. An `avf-boot`-style boot, in minutes.
6. The emulator with `dev-agent`: the real UI against an agent on this
   machine, with no VM.
7. `pocketnix deploy` to a phone guest: this checkout's agent or compositor in
   a VM that keeps its disk. The first cross-built compositor compiles rustc
   from source, so expect a long wait once.
8. A full install or provision on a phone, the one rung that changes the app
   on a device. `pocketnix install` stops the VM to do it.

The "By area" table has the command for each area, and `checks` in `flake.nix`
has every name. The table's "before committing" column still applies once the
fast rungs pass.

Go past the cheapest rung for:

- The wire: a test of one side can't see the other, so run both sides' tests.
- Boot or image changes: `avf-boot` boots the real disk image and installs its
  boot loader, and the NixOS VM tests use neither. Batch aarch64 changes first
  ("What it costs").
- The GPU: QEMU's VM has no gfxstream, so a GPU change needs a phone.
- Anything AVF-specific (crosvm, virtmgr, u-boot, the `pm grant`s): the emulator
  has no VM at all and the boot check runs QEMU with other firmware, so these
  need a phone too.
- What only a phone shows (touch, SurfaceFlinger, heat, throttling, real frame
  rates through `pocketnix bench run`): use `pocketnix deploy` when the agent or
  compositor changed, and a full install when the app did.

## By area

Fast loops run inside `nix develop`.

| changed | while editing | before committing |
|---|---|---|
| `go/` | `go -C go test ./internal/<pkg> -run <Test>`, after `go/AGENTS.md`'s `go generate` on a fresh checkout | the `go` check |
| `compositor/` | `cargo test` in `compositor/`; for speed, `bench --local` with and without `--guest-ram` (`docs/development.md`, "Benchmarks") | the `compositor` check, and `gui` when the app-facing behaviour changed |
| `app/` | `nix run .#gradle -- :app:testDebugUnitTest --tests '*<Name>*'` (without `:app:`, the terminal modules fail on the filter) | `nix run .#gradle -- testDebugUnitTest lintDebug` |
| `nix/`, `example/` | build the one VM test that covers it | `agent`, `gui`, and `avf-boot` for boot or image changes |
| a module option or its description | `nix run .#options-doc` | the `options-doc` check |
| `nix/managed/`, the module's `managed` options | the `managed` check (evaluation only) | `managed`, `managed-boot` (the base image's first boot, as a VM test), and `managed-all` (aarch64, CI builds it) |
| the base image users download (`nix/managed/`, `nix/module.nix`'s image) | `managed-boot`, as a VM test | `base-boot`, which boots the real image: aarch64 only, so CI's aarch64 job runs it (an x86_64 host can't assemble that image, `nix/AGENTS.md`) |
| `nix/vgpu-relinquish/`, `nix/avf-kernel/` | `nix build -L .#checks.aarch64-linux.vgpu-relinquish` | the same |
| the wire (`go/internal/protocol`, `docs/gui-protocol.md`) | both sides' tests | the `go` and `compositor` checks, and the app's tests through `nix run .#gradle`, which run them against the real agent and compositor |

Then `nix fmt`.

On x86_64, `nix flake check` skips the aarch64-only checks; `nix flake check
--no-build --all-systems` (CI's first step) still evaluates them. Building one
there goes through binfmt, except `base-boot`, which can't build there.

Each VM test's file under `nix/` opens with what it covers. They need KVM
and run on x86_64; `avf-boot` boots the real image under TCG and on x86_64
needs aarch64 builds.

## What it costs

The example image rebuilds whenever the guest's closure changes: the guest's
Go source (not its tests, not `cmd/pocketnix`; `nix/go.nix` says what it
leaves out), the module, the example, or the AVF kernel. On x86_64 the
aarch64 parts build under binfmt (`go/AGENTS.md` has the budget) or on a
remote builder that bills per CPU-hour (`AGENTS.md`'s working rules), so batch
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

`docs/development.md` has the rest: "Deploying a dev build to a phone guest"
for an agent or compositor, and "Testing on a rented phone" for a phone you
don't own.

## When a check fails

- `nix log <drv>` prints a failed build's whole log.
- A `vendorHash` mismatch after changing `go/go.mod`: put the hash Nix printed
  into `nix/go.nix`. The old hash can still name an older store path that Nix
  reuses without building, so set `lib.fakeHash` first if a build fails on
  missing modules.
- `format`: run `nix fmt`.
