# Traps in the compositor

The repository's root `AGENTS.md` has the working rules and the traps
that cross directories.

## Traps (verified on the desktop)

- `systemd-run --user` finds the user manager through `XDG_RUNTIME_DIR`
  (`DBUS_SESSION_BUS_ADDRESS` alone isn't enough): a desktop compositor
  started with a private runtime dir can't launch anything ("Failed to
  connect to user scope bus via local transport").
