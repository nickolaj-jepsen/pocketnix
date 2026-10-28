# GUI apps

Decisions for running the guest's Wayland apps as Android windows, settled
after `wayland-prior-art.md` and `wayland-spikes.md`. The feasibility checks
behind them ran on the desktop and the Android emulator (API 36); their notes
are in `~/.local/share/agents/pocketnix/spikes/` (`notes-ux-android.md`,
`notes-ux-compositor.md`, `notes-audio.md`).

## Scope

- GUI apps support the terminal; they don't replace it as the main use.
- Design for the docked case (external display, keyboard and mouse, or a
  tablet in desktop windowing) and make the bare phone good enough.
- Android is the window manager. Every guest window is an Android task, a
  freeform window when docked, or a tab next to a terminal. pocketnix draws
  no desktop of its own.
- A full Linux desktop is one more app: sway, labwc or Plasma run nested as a
  client of the compositor, with no compositor changes (measured: same bytes
  as running foot directly).

Deferred: per-app stub APKs (#2) and remote machines' apps (#3).

## Enabling

`pocketnix.guest.gui.enable`, off by default. It installs the compositor as a
socket-activated user service, exports `WAYLAND_DISPLAY` to the user session,
and turns on `services.pipewire`, `xdg.portal` (and with it the FileChooser
integration) and a font set with `mkDefault`. It sets `NIXOS_OZONE_WL=1`, so
Electron apps come up as Wayland clients. It keeps the closure down: the GTK
portal without its wallpaper
portal (gnome-desktop, gtk4, GStreamer), xdg-desktop-portal without geoclue,
and fonts without CJK, unifont and freefont, which
`fonts.enableDefaultPackages` brings back.

There is no list of apps in the module. The launcher shows the `.desktop`
files the system and user profiles already have.

## Where windows go

- **Launched from the launcher:** its own Android document task.
- **Started from a terminal tab:** a tab next to that terminal. The agent sets
  `POCKETNIX_TAB=<id>` in every tab, and the compositor reads it from the
  client's environment. Single-instance apps (Firefox, anything
  D-Bus-activated) land wherever their running process is; that's accepted.
- **Mapped while nothing of pocketnix is visible** (started over ssh, by a
  timer, D-Bus-activated): Android doesn't let a background app start an
  activity, so a notification in the "Guest windows" channel opens it. Without
  the notification (denied or dismissed), the window opens the next time
  pocketnix shows.
- A window's menu moves it between a tab and its own task.
- Dialogs and other child toplevels follow their parent and draw as a layer
  inside the parent's window, never as a Recents card. A dialog-themed
  Activity was tried and rejected: Android dims and centres it, and eats
  touches outside it.

## Launching

- The launcher is an `activity-alias` with its own drawer icon, enabled once
  the guest reports GUI on. The terminal screen links to it, and long-pressing
  either icon shows recent apps. Apps can be pinned with `requestPinShortcut`;
  the launcher badges pins with pocketnix's icon.
- The app list, icons (rasterized in the guest) and desktop ids are cached in
  app storage, so the launcher and pins work with the VM off.
- A tap opens the window's Activity at once, on a splash with the app's icon.
  Window Activities use a theme without the system splash, which would show
  pocketnix's icon. If the VM is off, the splash shows it booting, through the
  same foreground service the tile uses.
- The compositor launches the app in a `systemd-run --user --scope` with an
  xdg-activation token and ties the first toplevel to it. All six apps tried
  (foot, GTK3, GTK4, libadwaita, Qt6, Firefox) activated with their token, and
  each app id equals its desktop id. Launch to activation took 1.2 to 13.6 s,
  so the splash covers a real wait.
- A cached app that no longer exists after a switch says so on the splash, and
  its pins are disabled with `disableShortcuts`.
- Stale Recents cards from a dead VM are removed: window cards in
  `Application.onCreate`, launch cards when the process's first activity is
  created (until then they look like a fresh launch). No session restore.
- Stopping the VM with GUI windows open asks first and names them.

## Scale

- Windows default to Android's density, including the user's display size.
- Per-app size presets: Phone (Android's density), Compact (about 0.75x) and
  Desktop (about 0.5x). The compositor gives each window its own scale, and
  opens a new window at its app's preset.
- Automatic fit: when a window's minimum size at its scale is larger than
  the window, the app lowers that window's scale until the minimum fits, but
  never below the Desktop preset's factor of the display's density, on any
  display. It goes back up to the chosen preset as far as the minimum allows
  whenever the window or the minimum changes. The fit measures the window
  without the phone keyboard, so the keyboard never changes the scale. A
  window that doesn't fit at the floor is clipped, and zoom pans it. Dialogs
  take their window's scale and aren't fitted.
- Pinch zoom is visual only: it magnifies and pans the window's layers without
  changing the app's layout. One transform on the root SurfaceControl moves
  every layer, at about 0.4 ms per update.
- Docked, a window on the external display takes that display's density; the
  presets apply on the phone screen only.

## Input

- Touch modes per app: Touch (Wayland touch, the default), Pointer (tap is a
  click, long press a right-click, two-finger drag scrolls) and Trackpad (a
  relative cursor).
- In Touch mode every touch goes to the app, pinch included, and our zoom is a
  toggle in the window menu. In Pointer and Trackpad modes pinch and pan are
  ours.
- Android's system gestures win. Back moves the window to the back; it sends
  nothing to the app.
- A mouse, touchpad or physical keyboard goes through as Wayland input in any
  mode.
- The phone keyboard comes up when the app focuses a text field
  (text-input-v3: autocorrect, swipe and voice typing, composition). Smithay
  only serves text-input to a Linux input method, so the compositor gets its
  own handler.
- The keyboard button opens the phone keyboard on any window. Where no text
  field is focused (apps without text-input included), committed text
  becomes key events, with the keymap swapped for missing characters as
  `wtype` does. Nested desktops ignore keymaps they're sent, so there it's
  limited to the layout's own characters.
- The terminal's extra keys row shows above the keyboard in GUI windows too.
- The guest's keyboard layout follows Android's physical keyboard layout, and
  nested desktops get it as `XKB_DEFAULT_LAYOUT`.

## Window controls

- On the phone, a slim bar along the bottom of each window: the keyboard
  button, the title, and a menu (size, input mode, zoom, move, close). While
  typing, the extra keys row takes its place. Fullscreen apps hide it until a
  swipe up from the bottom edge. Windows in tabs use the terminal's tab bar
  and menu.
- Docked, Android's freeform caption bar stays. GTK's button layout is set to
  empty in the guest, so GTK header bars keep their controls but drop their own
  window buttons. Apps that accept server-side decorations (foot, Qt) get none
  drawn by us. The bottom bar hides while a mouse or keyboard is in use and
  shows on hovering the bottom edge or Meta+Esc.
- The caption shows pocketnix's name and icon for every window, whatever the
  task description says. That stays until #2.

## Per-app settings

Module defaults (`pocketnix.guest.gui.defaults`, and
`pocketnix.guest.gui.appDefaults.<app-id>` with `size`, `input` and so on)
reach the app with the rest of the config. Changes made on the phone override
them per app id, marked in the window menu with a "Reset to config". The app
never writes Nix. The compositor gets only each app's size factor, so new
windows open at their preset.

## Audio

- The local VM uses AVF's own virtio-snd device, which the guest kernel already
  supports. Confirmed on the phone: virtmgr runs `crosvm_vu_snd`, which plays
  through AAudio. The debug build's `--ez audio true` turns it on for now.
- The microphone is enabled at VM start only once `RECORD_AUDIO` is granted;
  a GUI setting asks for it and applies at the next boot.
- An MPRIS to MediaSession bridge in the agent gives lock-screen and headset
  controls, and lets the app pause guest playback for a call.
- A PCM stream through the agent (25 to 44 ms on the desktop) waits for #3.

## X11

In scope after the first version: `pocketnix.guest.gui.xwayland`, on by
default once done, using wprs's Xwayland bridge with Xwayland started on
demand. X11 windows get integer scale by default and the keyboard fallback
instead of text-input.

## Clipboard

One clipboard path. The compositor offers data-control, and a
`pocketnix-guest` bridge syncs the Wayland selection with the existing
clipboard integration. The GUI protocol carries no clipboard.

## Architecture

- **Compositor:** a Rust crate in this repo (`compositor/`), started from
  wprs's server half with the fixes from the spikes, edited freely with no
  upstream sync. wprs's Apache-2.0 licence and notice stay in the crate. The
  small upstreamable fixes go to wprs first. It reads `.desktop` files and
  icons, launches apps, ties windows to launches and tabs, applies per-app
  settings and speaks the GUI protocol.
- **Agent:** a GUI stream kind spliced to the compositor's socket; it never
  parses the protocol. It also sets `POCKETNIX_TAB` and carries the module's
  GUI defaults in the config.
- **Startup:** the compositor's GUI and Wayland sockets are systemd user socket
  units, so the app connecting or a terminal starting an app brings it up.
- **App:** one GUI stream while the guest has GUI on, shared by every window
  Activity and tab. Kotlin decodes the protocol and drives the Java
  SurfaceControl API (`minSdk` is 34): a child layer per guest surface, two or
  three HardwareBuffers each with damage tracking, `setBuffer` and
  `setDamageRegion`. A small C library unpacks zstd (the vendored single-file
  decoder) and copies damaged rects into a locked `AHardwareBuffer`, which
  Java can't lock.
- **Protocol:** framed like PTY streams. JSON frames for the app list, launch
  requests, window metadata and roles, popup positions, settings, IME state and
  cursor names; binary frames for icons, surface commits (surface, size,
  format, scale, optional scroll, damage rects and zstd pixels together, so
  buffer and commit never separate), input, and frame callbacks so apps draw at
  Android's pace. Large commits are split under `MaxFramePayload`. The spec is
  `docs/gui-protocol.md`, with no generated code; JVM tests run the decoder
  against a real compositor and compare content hashes.

## Build order

0. On main: the GPU probe from `display-slice`, the prior-art and spikes docs,
   these decisions, and `design.md` no longer saying "No graphical guest".
   When the phone is free: the phone-vm measurements and a virtio-snd tone.
1. Foundation: the compositor crate, the protocol, the stream kind,
   `gui.enable` with socket activation, one task per window, the Kotlin decoder
   and native copy, mouse and hardware keyboard. Done when foot and
   gtk3-widget-factory work from a desktop agent on the emulator and then on
   phone-vm, survive an agent restart and match hashes.
2. The window model: launcher, cache, pins, splash and boot on tap, activation
   matching, tabs and moving windows, dialogs and popups as layers. Usable
   docked from here.
3. Phone input: touch, text-input-v3 and extra keys, the bottom bar, size
   presets, zoom and per-app settings.
4. Docked polish: external display density, the decoration policy and the
   hiding bar, cursor-shape and toplevel icons.
5. Audio and X11.

Compositor work measured in the spikes, on top of the fork: activation about
50 lines, per-client scale 130 (popup offsets still wrong), touch 200,
text-input 300, decoration policy 10, cursor shapes and toplevel icons 60.
