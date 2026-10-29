# Traps in the agent and the CLI

The repository's root `AGENTS.md` has the working rules and the traps
that cross directories.

## Traps (verified on the desktop)

- Go defers closing a plain `*os.File` while another goroutine is blocked
  reading it, so the agent moves the PTY master into Go's poller (`pollable`):
  only then does closing it end the read and hang up background children.
- A change to the guest's Go source (which files that leaves out: `nix/go.nix`)
  rebuilds the aarch64 agent under binfmt and so the whole example image:
  budget ~15 minutes.
- The agent is root and touches user-owned paths (history, the share
  directory, the file provider): go through an `os.Root`, which also refuses
  absolute symlinks even when they point back inside. A configured path is
  opened with `configuredDir`/`configuredFile`, never `os.OpenRoot` on it.
- A host-folder mount (FUSE, no `allow_other`) admits only the session user,
  not root: check it with `runuser`, and `findmnt` rather than `mountpoint`.
  go-fuse's own mounting probes the mount from its process, so the agent
  mounts itself and hands go-fuse the descriptor as `/dev/fd/N`.
