# Agent notes

Read `docs/architecture.md` first: its design rules are settled. The docs
index is in `README.md`; building, testing and the app's scripted control are
in `docs/development.md`.

## Layout

`docs/architecture.md` maps the directories. Beyond it:

- The app is `dev.nickolaj.pocketnix`; debug builds add `.debug`.
- `terminal-emulator/`, `terminal-view/` are vendored (Termux, by way of
  Podroid): edit them freely; there is no upstream sync.
- The source directories' own `AGENTS.md` files hold the traps that bite
  there: read the one for the directory you change.

## Working rules

- CI builds the flake's checks, per system (`.github/workflows/ci.yml`); `nix flake check` runs them all.
- Run `nix fmt` before committing: it formats Nix, Go and Kotlin (`nix/treefmt.nix`;
  ktlint's rules are in `.editorconfig`). The compositor isn't rustfmt'ed, so
  don't reformat it wholesale.
- Never run a build of your own inside a guest: an unthrottled build pegs the
  whole phone. A managed guest's own rebuild is the product's, throttled
  (`pocketnix.guest.managed.cpuQuota`); start one only through the app.
- Uninstalling the app, clearing its data or `provision --replace` deletes
  the VM's disk: never do it to a phone's working VM without asking.

## Keeping docs and tests from drifting

Copies of code in docs and tests have drifted here repeatedly; keep one source.

- Docs explain why and how to use; they name the source of truth instead of
  copying it. Module options and their defaults live in `nix/module.nix`'s
  descriptions (`docs/options.md` is generated from them by
  `nix run .#options-doc`; the `options-doc` check fails when it's stale),
  stream kinds and message fields in `go/internal/protocol`,
  checks and packages in `flake.nix`, `ControlActivity`'s extras in its
  `EXTRA_*` constants. No timeouts, limits or counts that a constant holds.
- `docs/gui-protocol.md` is the exception: it is the wire spec, and code
  matches it. Code cites its headings (and `docs/architecture.md`'s) by name,
  so grep before renaming one.
- Traps live in the `AGENTS.md` of the directory they bite in, and here
  when they cross directories or come from handling a device; other docs
  point at them.
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

- Bulk adb transfers over USB drop the device after ~100–200 MB: push in chunks.
  An uncompressed `adb exec-in` drops it even at 64 MiB (`UsbFfs: Error while
  reading USB block`), and a reconnect loses every `adb forward`.
- The `pm grant`s are per Android user and lost on uninstall.
- The VM dies with the app's process (virtmgr ties it to the app): an app crash
  is a hard power-off of the guest.
- Restoring a task relaunches it from its base intent: an adb `am start` with an
  action, sent to the main activity, gets replayed (it booted the VM after an
  app update). Scripted actions go to `ControlActivity`, which has no history.
- With the phone locked, an activity started over adb is never laid out, so a
  TerminalView never sizes its session, and nothing on screen shows the app.
  `pocketnix control` (debug builds' `ControlProvider`; how to use it:
  `.agents/skills/pocketnix-control/SKILL.md`) opens tabs, types into them and
  reads back their transcripts and the GUI state.
- GrapheneOS refuses perf events for app processes, from adb shell and under
  `run-as` alike: simpleperf can't profile the app (`--app` waits forever for
  its process). Per-thread `/proc` stat is what there is.

## Traps (verified on the desktop)

- A hidden app doesn't redraw tabs (`Tabs.visible`): anything that inspects
  the view rather than the transcript sees stale output.
- An agent push with a top-level `"id"` never reaches `GuestBridge`: the
  app's `ControlChannel` and the Go client take it for a late answer to a
  request. Name event ids otherwise (notify uses `notification`).
