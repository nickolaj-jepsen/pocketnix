---
name: pocketnix-control
description: Drive pocketnix's debug app on a connected phone or emulator over adb without looking at the screen — open terminal tabs, type commands into the guest, wait for their output, and read the VM's state, tab transcripts and GUI windows. Use when checking a change in the real app on a device, running a command in the guest through a tab, or inspecting what the app shows while the phone is locked.
---

# Driving the app with `pocketnix control`

Debug builds (`dev.nickolaj.pocketnix.debug`) carry a `ControlProvider` that
only the adb shell may call. `pocketnix control` wraps it. Nothing needs the
screen, so all of this works with the phone locked, where activities never
lay out and the UI shows you nothing.

A phone's debug app may hold a working VM: don't open tabs or type there
unless the user asked for it.

## Running it

Inside `nix develop`, which puts `adb` and this checkout's `pocketnix` on
PATH (rebuilt from source on each run):

```bash
pocketnix control <command> …
```

Add `--serial <serial>` before the command when more than one device is
attached (`adb devices`). `pocketnix control -h` lists the commands.

## Commands

| command | does |
|---|---|
| `dump [--tab <id>] [--lines <n>]` | Prints the VM state (`state=… phase=… link=… host=…`), the chrome's colours (`theme background=#rrggbb source=<default\|saved\|module>`: `saved` is the last module theme, applied before the agent answers), then each tab as `tab <id> <kind> '<title>' running=<bool>` followed by its last lines as `\|<id>\| <line>`, then `gui …` lines for the GUI connection and each window. `--tab` limits it to one tab and leaves out the GUI. |
| `tab [--profile <name>] [--timeout <dur>] [<text>]` | Waits for the agent, opens a terminal tab, types `<text>` and Enter, and prints the new tab's id. |
| `type --tab <id> <text>` | Types `<text>` and Enter into an open tab. |
| `wait --tab <id> [--timeout <dur>] [--lines <n>] <regexp>` | Polls the tab until one of its transcript lines matches, then prints the tab's dump. Exits 1 with the last dump if nothing matches in time. |
| `menu --tab <id> [<label>…]` | Lists the tab's context menu (the one More… and a right-click open, with no selection) as `<label><TAB><enabled\|disabled>` lines, or the page a path of labels leads to (`menu --tab 1 "Open link…"`). A path ending at an action runs it: `menu --tab 1 "Open link…" https://example.org`, `menu --tab 1 "Reset terminal"`. |
| `menu --chip <id> [<label>…]` | The same for the tab's long-press menu: `menu --chip 2 "New tab"`. Rename… opens a dialog, so it answers an error here: use `rename`. |
| `menu --main [<label>…]` | The same for the ⋮ menu: `menu --main "Reopen ended tabs"`. Entries that open a dialog or ask first answer an error. |
| `links --tab <id>` | Prints the tab's Open link… list, one URL per line, newest first. |
| `rename --tab <id> <title>` | Gives a terminal tab its own title, which `dump` and the notifications show; `""` goes back to the shell's title. |
| `reopen --tab <id>` | Starts an ended terminal tab again in a new session, keeping its id, place and title; the old transcript is gone. |
| `logcat [--follow]` | Prints the log (main, system and crash buffers) of every process running as the app's uid, so it keeps lines from before a crash or restart. `--follow` streams it. Works on release builds too (`--package`). |

`tab` and `type` take `--no-enter` to type without pressing Enter. Tab 0 is
the hvc0 console: it always exists and shows boot and login output.

## Running a command and reading its result

A tab runs the guest user's login shell, which may not be sh (phone-vm's is
fish, which rejects `$((…))` and `$?`). Wrap commands in `sh -c '…'`
so they mean the same everywhere.

The typed command line stays in the transcript, so a pattern that also
matches the command would match right away. Wait for a marker that only the
output contains, built by the shell:

```bash
id=$(pocketnix control tab)
pocketnix control type --tab "$id" "sh -c 'systemctl is-system-running; echo status-\$?'"
pocketnix control wait --tab "$id" --lines 80 '^status-[0-9]+$'
```

Here the command line shows `status-$?` and only the output shows
`status-0`. `wait` prints the transcript, and the lines just above the
marker are the output. For long output, raise `--lines`, or redirect it to a
file in the guest and `cat` just the part you need.

Typing goes into whatever is on the shell's command line. fish keeps a
command it rejected there, and `--no-enter` leaves text behind, so the next
`type` adds to it. When a tab's line is in an unknown state, open a new tab.

## Reading the state

- A tab's `running=false` means its session ended (shell exited, agent gone).
- On the state line, `phase=` is what the app's status surfaces show
  (`VmPhase`, from `state=`, `link=` and the app's own flags): wait for
  `phase=Running` rather than reading `state=` and `link=` apart.
- On the state line, `link=` is the agent control channel. `tab` waits until
  it is up, so it fails with "the agent wasn't up within …" while the VM is
  still booting or stopped.
- The `gui connected=… ready=…` line and the `gui window …` lines show what
  the app knows about guest windows: title, app, size, scale and surfaces.
  Use them to check GUI changes without screenshots.

## Without the CLI

The provider can also be called directly. `adb shell` runs its arguments
through the phone's shell again, so quote the whole command and quote the
values inside it once more. `content` reads a backslash in an `--extra`
value as escaping the next character, and splits the binding at unescaped
colons, so the text goes over with `\\` for each backslash and `\:` for each
colon. `input` is typed exactly as it arrives, and `enter:b:true` presses
Enter after it.

```bash
adb shell "content read --uri 'content://dev.nickolaj.pocketnix.debug.control/dump?tab=1&lines=80'"
adb shell "content call --uri content://dev.nickolaj.pocketnix.debug.control --method tab --extra 'input:s:uname -a' --extra enter:b:true"
adb shell "content call --uri content://dev.nickolaj.pocketnix.debug.control --method type --arg 1 --extra 'input:s:date +%H\:%M' --extra enter:b:true"
```

A call answers `Result: Bundle[{…}]`, carrying an `error` key when it failed.
A failed dump reads `error: <message>`.

## Checking what the app opened or shared

An open or a share starts another app's activity, which system_server logs
as `ActivityTaskManager: START u0 {act=… dat=…}`. That isn't the app's uid,
so read it with plain `adb logcat -d`, not `pocketnix control logcat`.

## Reading the app's log

`pocketnix control logcat` is the app's side: engine and VM state changes,
the agent link, and the stack trace when the app dies (which also kills the
VM). Pipe it through `grep` for a tag (`Vm`, `PortForwards`, …) rather than
reading it all.

## When it fails

- `Error while accessing provider` / "Unknown authority": the installed app is
  a release build or predates the provider, or another package is installed
  (pass `--package`).
- "only adb may control the app": the caller isn't the adb shell.
- "debug builds only": a release build answered.
- Nothing typed shows up: check `running=` on the tab. Input goes to the
  session even when the tab isn't selected.
