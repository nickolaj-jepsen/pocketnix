---
name: pocketnix-control
description: Drive pocketnix's debug app on a connected phone or emulator over adb without looking at the screen — open terminal tabs, type commands into the guest, wait for their output, and read the VM's state, tab transcripts and GUI windows. Use when checking a change in the real app on a device, running a command in the guest through a tab, or inspecting what the app shows while the phone is locked.
---

# Driving the app with `pocketnix control`

Debug builds (`dev.nickolaj.pocketnix.debug`) carry a `ControlProvider` that
only the adb shell may call. `pocketnix control` wraps it. Nothing needs the
screen, so all of this works with the phone locked, where activities never
lay out and the UI shows you nothing.

Before touching the test phone, reread "The test phone" in `AGENTS.md`: its
debug app holds the real phone-vm. Don't open tabs or type there unless the
user asked for it.

## Running it

From the repo, inside `nix develop` (which puts `adb` on PATH):

```bash
go -C go run ./cmd/pocketnix control <command> …
```

Add `--serial <serial>` before the command when more than one device is
attached (`adb devices`). `pocketnix control -h` lists the commands.

## Commands

| command | does |
|---|---|
| `dump [--tab <id>] [--lines <n>]` | Prints the VM state (`state=… link=… host=…`), then each tab as `tab <id> <kind> '<title>' running=<bool>` followed by its last lines (40) as `\|<id>\| <line>`, then `gui …` lines for the GUI connection and each window. `--tab` limits it to one tab and leaves out the GUI. |
| `tab [--profile <name>] [--timeout 60s] [<text>]` | Waits for the agent, opens a terminal tab, types `<text>` and Enter, and prints the new tab's id. |
| `type --tab <id> <text>` | Types `<text>` and Enter into an open tab. |
| `wait --tab <id> [--timeout 30s] [--lines <n>] <regexp>` | Polls the tab until one of its transcript lines matches, then prints the tab's dump. Exits 1 with the last dump if nothing matches in time. |

`tab` and `type` take `--no-enter` to type without pressing Enter. Tab 0 is
the hvc0 console: it always exists and shows boot and login output.

## Running a command and reading its result

The typed command line stays in the transcript, so a pattern that also
matches the command would match right away. Wait for a marker that only the
output contains, built by the shell:

```bash
id=$(go -C go run ./cmd/pocketnix control tab)
go -C go run ./cmd/pocketnix control type --tab "$id" 'systemctl is-system-running; echo "done-$((40+2))"'
go -C go run ./cmd/pocketnix control wait --tab "$id" --lines 80 '^done-42$'
```

`wait` prints the transcript. The lines just above the marker are the
output. For long output, raise `--lines`, or redirect it to a file in the
guest and `cat` just the part you need. Check an exit status the same way:
`cmd; echo "status-$?"`.

## Reading the state

- A tab's `running=false` means its session ended (shell exited, agent gone).
- On the state line, `link=` is the agent control channel. `tab` waits until
  it is up, so it fails with "the agent wasn't up within …" while the VM is
  still booting or stopped.
- The `gui connected=… ready=…` line and the `gui window …` lines show what
  the app knows about guest windows: title, app, size, scale and surfaces.
  Use them to check GUI changes without screenshots.

## Without the CLI

The provider can also be called directly. `adb shell` runs its arguments
through the phone's shell again, so quote the whole command and quote the
values inside it once more. In `--extra` values, escape every `:` as `\:`,
since `content` splits bindings at unescaped colons. A literal `\r` in
`input` is Enter.

```bash
adb shell "content read --uri 'content://dev.nickolaj.pocketnix.debug.control/dump?tab=1&lines=80'"
adb shell "content call --uri content://dev.nickolaj.pocketnix.debug.control --method tab --extra 'input:s:uname -a\r'"
adb shell "content call --uri content://dev.nickolaj.pocketnix.debug.control --method type --arg 1 --extra 'input:s:ls\r'"
```

A call answers `Result: Bundle[{…}]`, carrying an `error` key when it failed.
A failed dump reads `error: <message>`.

## When it fails

- `Error while accessing provider` / "Unknown authority": the installed app is
  a release build or predates the provider, or another package is installed
  (pass `--package`).
- "only adb may control the app": the caller isn't the adb shell.
- "debug builds only": a release build answered.
- Nothing typed shows up: check `running=` on the tab. Input goes to the
  session even when the tab isn't selected.
