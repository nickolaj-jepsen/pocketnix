# GUI protocol

The wire between the guest compositor (`pocketnix-compositor`) and the app,
for the decisions in `gui-apps.md`. The agent only carries it. This page
specifies what step 1 needs and reserves names for later steps, so that
those steps only add things. The last section is the contract between the
compositor, the agent, the NixOS module and the app.

## Transport

- The app opens an agent connection whose Hello names stream `gui`
  (`protocol.StreamGUI`). After the agent's Hello line, the agent splices the
  connection to the compositor's GUI socket byte for byte. From there on,
  every byte belongs to this protocol.
- Both directions carry frames framed like PTY frames: `[type u8][length u32
  BE][payload]`, payload at most `MaxFramePayload` (1 MiB). A longer length
  is a protocol error.
- Integers are big-endian. `f32` is an IEEE 754 single, big-endian. Sizes and
  positions are in **window pixels** (Android physical pixels of the window
  the surface draws in) unless a field says buffer pixels.
- Type `0x00` is JSON: the payload is one UTF-8 JSON object with a `"type"`
  string. Field names are camelCase, as on the control stream. Every other
  type is binary.
- A receiver skips frame types it doesn't know, ignores JSON messages with an
  unknown `type`, and ignores unknown JSON fields and trailing bytes after the
  binary fields it knows. That is how later steps extend the protocol. A type
  number or message name never changes meaning; an incompatible change bumps
  the version.
- Any other violation (bad length, malformed JSON, a commit that doesn't
  decode, an unknown surface in a commit) is fatal: the receiver closes the
  connection. The app then reconnects and gets a replay, so an error costs a
  redraw and never an app.

### Frame types

| type | name | direction | step |
|---|---|---|---|
| `0x00` | JSON | both | 1 |
| `0x01` | COMMIT | compositor → app | 1 |
| `0x02` | COMMIT_DATA | compositor → app | 1 |
| `0x03` | ICON | compositor → app | reserved, 2 |
| `0x04` | CURSOR_IMAGE | compositor → app | reserved, 4 |
| `0x10` | PRESENTED | app → compositor | 1 |
| `0x20` | POINTER_MOTION | app → compositor | 1 |
| `0x21` | POINTER_BUTTON | app → compositor | 1 |
| `0x22` | POINTER_AXIS | app → compositor | 1 |
| `0x23` | POINTER_LEAVE | app → compositor | 1 |
| `0x24` | POINTER_RELATIVE | app → compositor | reserved, 3 |
| `0x30` | KEY | app → compositor | 1 |
| `0x31` | MODIFIERS | app → compositor | 1 |
| `0x40`–`0x44` | TOUCH_DOWN, TOUCH_UP, TOUCH_MOTION, TOUCH_CANCEL, TOUCH_FRAME | app → compositor | reserved, 3 |

## Session

### Handshake

1. The app sends `hello`:
   `{"type":"hello","version":1,"minVersion":1,"app":"<build>","hashes":false}`.
   `hashes: true` asks for a content hash on every commit that carries pixels.
2. The compositor answers with
   `{"type":"hello","version":<v>,"compositor":"<build>","instance":"<id>"}`,
   where `v` is the highest version both speak (the agent's `Negotiate` rule),
   or with `{"type":"error","message":"…"}` and closes.
   `instance` is a random string fixed for the compositor process's lifetime.
3. The compositor starts the replay at once. The app, after reading the hello,
   sends `output` and then a `window` for each window it still shows from the
   same `instance`.

Surface ids are `u32`, assigned by the compositor, unique within an `instance`
and never reused in it. Ids from another instance mean nothing. When the
instance differs from the one the app last saw, the compositor restarted and
its clients died with it: the app closes every window it had.

The protocol version is the GUI protocol's own, independent of the agent's
protocol version. This page is version 1.

### Replay

On every connection, after its `hello`, the compositor sends its whole state:

- for each mapped toplevel, parents before children: `toplevel`, a `surface`
  for each of its surfaces, then one group of FULL commits for every surface
  of the window that has a buffer
- `cursor`
- `ready`

After `ready` the app closes any window it still had that wasn't replayed.
Commits that follow are relative to the replayed content.

### One connection at a time

The compositor serves one app connection. When a new one finishes its hello,
the compositor closes the old one and discards everything queued for it, so
no half-sent commit is replayed. The new connection gets a full replay from
the compositor's per-surface mirror. Wayland clients never see a reconnect,
other than frame callbacks pausing while no app is connected.

## Windows and surfaces

- A **window** is a mapped xdg toplevel, shown as one Android window. Its id
  is its root `wl_surface`'s id.
- Every surface the app draws (the toplevel's root surface, its subsurfaces,
  popups and their subsurfaces) belongs to exactly one window and is a layer
  in that window's stack. The compositor constrains popups inside their
  window, since Android clips child layers to the task.
- A window's coordinate origin is the top-left of the toplevel's window
  geometry (`xdg_surface.set_window_geometry`, or the whole root surface when
  it's unset). The Android window shows `(0, 0)` to `(width, height)` of that
  space. A root surface with client-side shadows has a negative position.
- The compositor converts logical coordinates to window pixels with the
  window's `scale`: an edge at logical `l` is at `round(l × scale)`, and a
  size is the difference of its rounded edges. The app does no scale
  arithmetic for placement.
- Cursor surfaces aren't sent in step 1 (see `cursor`).

### Compositor → app JSON

| message | fields | meaning |
|---|---|---|
| `toplevel` | `id`, `parent`, `title`, `appId`, `width`, `height`, `minWidth`, `minHeight`, `maxWidth`, `maxHeight`, `decoration` | A toplevel mapped, or its metadata changed. Sent whole every time. `parent`: the parent toplevel's id (`xdg_toplevel.set_parent`), 0 for none. `width`/`height`: the window geometry's current size. Min/max: 0 for none. `decoration`: `"client"` or `"server"` (the xdg-decoration mode in effect). Strings may be empty. |
| `toplevel-removed` | `id` | The toplevel was unmapped or destroyed. The app forgets the window and every surface in it. If it maps again it's announced again, with the same id, followed by FULL commits. |
| `toplevel-request` | `id`, `request` | The client asked for `minimize`, `maximize`, `unmaximize`, `fullscreen`, `unfullscreen`, `move` or `resize`. Advisory; the app may ignore any of them. |
| `surface` | `id`, `window`, `role`, `parent` | A surface joined a window, before its first commit. `role`: `"toplevel"`, `"subsurface"` or `"popup"`. `parent`: the parent surface's id (0 for a toplevel root). |
| `surface-removed` | `id` | Drop the layer and its content. |
| `cursor` | `window`, `name` | The pointer's cursor over `window`. `name` is a cursor-shape-v1 name (`"default"`, `"text"`, `"pointer"`, `"grab"`, `"ew-resize"`, …) or `"hidden"`. A client-drawn cursor surface comes as `"default"` until CURSOR_IMAGE (step 4). |
| `ready` | | End of the replay. |
| `error` | `message` | Sent before the compositor closes the connection over a protocol error. |

### App → compositor JSON

| message | fields | meaning |
|---|---|---|
| `output` | `width`, `height`, `scale`, `refresh` | The display a new window opens on: the size in pixels a new window would get, the density (`densityDpi / 160`, a float) and the refresh rate in mHz. Sent after the hello and whenever it changes. The compositor's `wl_output` and the initial configure of new toplevels follow it. |
| `window` | `id`, `width`, `height`, `scale`, `focused`, `visible`, `maximized`, `fullscreen` | The Android window's state for a toplevel. `width`/`height` in pixels (0: let the client choose). `scale`: pixels per logical pixel for this window (density times any size preset); the compositor gives the client this as its preferred fractional scale (and `ceil` of it as the integer buffer scale) and configures it at `round(width / scale)` × `round(height / scale)`. `focused` maps to the `activated` state and keyboard focus, `visible: false` to `suspended`, and the other two to the xdg states of the same names. Debounce while a resize is in progress. |
| `close` | `id` | `xdg_toplevel.close`, e.g. from `onDestroy` of a window the user closed. |

## Commits

A COMMIT carries one surface's committed state: placement, buffer metadata,
damage and pixels, all in one message, so a buffer and its commit can't come
apart.

### COMMIT (`0x01`)

| offset | type | field |
|---|---|---|
| 0 | u16 | `headerSize`: offset of the rects; 88 in version 1 |
| 2 | u16 | `flags` |
| 4 | u32 | `surface` |
| 8 | u32 | `window` |
| 12 | u32 | `seq` |
| 16 | u16 | `z` |
| 18 | u8 | `format` |
| 19 | u8 | `encoding` |
| 20 | u8 | `transform` |
| 21 | u8 | reserved, 0 |
| 22 | u16 | reserved, 0 |
| 24 | i32 | `x` |
| 28 | i32 | `y` |
| 32 | u32 | `dstWidth` |
| 36 | u32 | `dstHeight` |
| 40 | u32 | `width` |
| 44 | u32 | `height` |
| 48 | u32 | `srcX` |
| 52 | u32 | `srcY` |
| 56 | u32 | `srcWidth` |
| 60 | u32 | `srcHeight` |
| 64 | i32 | `scroll` |
| 68 | u32 | `rectCount` |
| 72 | u32 | `dataLength` |
| 76 | u32 | reserved, 0 |
| 80 | u64 | `hash` |
| `headerSize` | 16 × `rectCount` | rects: `x u32, y u32, w u32, h u32` |
| after the rects | bytes | the first bytes of the pixel data |

Fields:

- `seq`: per connection, starting at 1, strictly increasing across all
  surfaces. PRESENTED refers to it.
- `z`: the surface's place in its window's stack, 0 at the bottom. Unique
  within a window once a group has been applied.
- `x`, `y`, `dstWidth`, `dstHeight`: where the surface shows, in window
  pixels.
- `width`, `height`: the buffer, in buffer pixels. At most 16384 each, and
  `width × height × 4` at most 64 MiB: the compositor refuses a bigger
  `wl_buffer` with `wl_surface`'s `invalid_size` error, so the app can treat
  one as a protocol error.
- `srcX`, `srcY`, `srcWidth`, `srcHeight`: the part of the transformed buffer
  that's shown (`wp_viewport` source), in buffer pixels; the whole buffer
  when the client set none. The surface shows that part of the buffer,
  transformed by `transform` and scaled to `dstWidth` × `dstHeight` at
  (`x`, `y`).
- `format`: `1` RGBA8888 (bytes R, G, B, A in memory, premultiplied alpha),
  `2` RGBX8888 (the fourth byte is undefined; the surface is opaque). These
  match `HardwareBuffer.RGBA_8888` and `RGBX_8888`, so the app copies without
  converting. The compositor converts from the client's `wl_shm` format while
  packing.
- `encoding`: `0` raw, `1` zstd.
- `transform`: a `wl_output.transform` value, with `wl_surface.set_buffer_transform`'s
  meaning. 0 for nearly every client.
- `rects`: damage, in buffer pixels of the untransformed buffer, clipped to
  it, applied in order. At most 4096; the compositor merges beyond that.
  They may overlap, but their areas sum to at most `width × height`: past
  that the compositor sends the buffer whole.
- `dataLength`: the total pixel data length across this COMMIT and its
  COMMIT_DATA frames.
- `hash`: see Content hash. Zero unless the HASH flag is set.

Flags:

| bit | name | meaning |
|---|---|---|
| 0 | BUFFER | The commit carries pixels: `rects` and data. Without it, only placement, size or stacking changed (or the client only asked for a frame callback), and `rectCount` and `dataLength` are 0. |
| 1 | FULL | With BUFFER: the single rect is the whole buffer, and the previous content is discarded. |
| 2 | SCROLL | With BUFFER and without FULL: `scroll` is valid (see below). |
| 3 | HASH | `hash` is valid. |
| 4 | GROUPED | More commits of this group follow. |
| 5 | HIDDEN | The surface has no buffer (attached null). The app hides the layer and drops its content; size fields are 0. |

The compositor sends FULL on a surface's first commit on a connection, after
HIDDEN, when the buffer's size or format changes, and whenever else it
chooses. A non-FULL BUFFER commit for a surface without content, or whose
size or format differs from that content, is a protocol error.

### Pixel data

- The data is each rect's rows in rect order, top to bottom, `w × 4` bytes per
  row with no padding. Decompressed, it is exactly `Σ w × h × 4` bytes, or
  it's a protocol error.
- With zstd it's one or more concatenated zstd frames; the decompressed
  concatenation is the data. The compositor may compress shards in parallel.
- Data that doesn't fit in the COMMIT frame continues in COMMIT_DATA frames.

### COMMIT_DATA (`0x02`)

`[seq u32][bytes]`: the next bytes of the pixel data of the commit with that
`seq`. A commit's frames are consecutive: nothing else is sent between a
COMMIT and the COMMIT_DATA frame that completes its `dataLength`. The split
is at arbitrary byte offsets, so a decoder may decompress as frames arrive or
after collecting them.

### Applying a commit

The app keeps a **reference copy** of each surface's content: the result of
applying that surface's commits in order. For a BUFFER commit:

1. FULL: the data replaces the reference copy.
2. SCROLL: first, row `y` of the new content is row `y + scroll` of the
   previous content, for every `y` with `0 ≤ y + scroll < height`. Rows
   without a source are covered by the rects.
3. The rects' pixels are written over the result.

The reference copy is what hashes are computed over. The app may keep it as
a CPU copy (the spike did) or rebuild it from its buffers.

### Groups

A group is a run of consecutive COMMITs where all but the last have GROUPED
set. The app applies a group atomically: every commit in it lands in one
`SurfaceControl.Transaction`, never across two. A surface appears at most
once in a group, and no JSON or other frame comes between a group's commits.
The compositor groups synchronized subsurfaces with their parent's commit
(children first), every restack or move that one Wayland commit causes, and
each window's replay.

### Buffer rotation

The app shows a surface from 2 or 3 HardwareBuffers in rotation. A buffer
that last held the content of commit `N − k` and now gets commit `N` must
receive every pixel that changed since then:

- Let `C(i)` be commit `i`'s changed region for that surface: the whole
  buffer if `i` was FULL or SCROLL, otherwise the union of its rects.
- Write `C(N−k+1) ∪ … ∪ C(N)` from the reference copy into the buffer. A
  buffer never filled, or a history the app no longer has, means the whole
  buffer.
- Pass `C(N)` to `setDamageRegion`.

Wayland damage describes what changed since the surface's previous content,
whichever buffer held it. That is why one commit's rects aren't enough for
a buffer that's two or three commits behind.

## Frame pacing

- Clients draw at Android's pace: the compositor fires a surface's
  `wl_surface.frame` callbacks only when the app reports that a commit
  requesting them has been shown.
- Every Wayland commit that requested a frame callback produces a COMMIT,
  even when nothing visible changed, so the app has something to acknowledge.
- **PRESENTED (`0x10`)**, `[window u32][seq u32]`: the app's commits for
  `window` up to `seq` are on screen. The app sends it once the transaction
  carrying a group has been committed by SurfaceFlinger. A group with nothing
  to show is acknowledged at the next Choreographer frame. The compositor then
  fires the callbacks of every commit in that window with a `seq` up to that
  number.
- For a window the app isn't showing (`visible: false`, not yet open, or
  closed but still mapped), the app still applies commits but sends no
  PRESENTED. The compositor holds that window's callbacks and tells the client
  it's `suspended`. With no app connected, all callbacks are held.
- If commits back up for a surface, the compositor may replace that surface's
  unsent commits with one FULL commit from its mirror.

## Input

The app reports input in window pixels, relative to the window's origin. The
compositor divides by the window's scale, hit-tests the window's surfaces
(input regions, popups, subsurfaces) and sends the Wayland events. The app
does no hit-testing. Event times come from the compositor's clock on receipt.

| type | payload |
|---|---|
| POINTER_MOTION `0x20` | `[window u32][x f32][y f32]`. Implies pointer enter. |
| POINTER_BUTTON `0x21` | `[window u32][x f32][y f32][button u32][state u8]`: `button` a Linux `BTN_*` code (`BTN_LEFT` 0x110, `BTN_RIGHT` 0x111, `BTN_MIDDLE` 0x112, `BTN_SIDE` 0x113, `BTN_EXTRA` 0x114); `state` 1 pressed, 0 released. |
| POINTER_AXIS `0x22` | `[window u32][x f32][y f32][source u8][stop u8][dx f32][dy f32][v120x i32][v120y i32]`: `source` 0 wheel, 1 finger, 2 continuous, 3 wheel tilt; `stop` bit 0 horizontal, bit 1 vertical (finger lifted); `dx`/`dy` in window pixels; `v120x`/`v120y` in 1/120 notches, 0 when not a wheel. Positive is right and down, Wayland's direction, so Android's `AXIS_VSCROLL` is negated. |
| POINTER_LEAVE `0x23` | `[window u32]`. |
| KEY `0x30` | `[window u32][key u32][state u8]`: `key` a Linux evdev `KEY_*` code (XKB keycode − 8); `state` 1 pressed, 0 released. |
| MODIFIERS `0x31` | `[window u32][mods u8]`: bit 0 shift, 1 ctrl, 2 alt, 3 logo, 4 caps lock, 5 num lock. |

- KEY and MODIFIERS move keyboard focus to `window` first if it's elsewhere.
  The app sends MODIFIERS when a window gains focus and whenever the meta
  state changes, including modifier-only changes: Android's `metaState` as
  is.
- Clients repeat keys themselves, so the app drops events with
  `getRepeatCount() > 0`.
- When keyboard focus leaves a window, the compositor releases the keys it
  holds pressed. When the pointer leaves, it releases held buttons unless a
  drag is in progress.
- Keymap: the compositor's own XKB keymap interprets the codes, from its
  environment (`XKB_DEFAULT_LAYOUT`, …) and `us` otherwise. The app sends
  `KeyEvent.getScanCode()` for hardware keyboards, which is the evdev code,
  and maps `keyCode` to evdev for events without one. Following Android's
  keyboard layout is step 3 (`keyboard-layout`).

## Content hash

For tests, and for `--log-hashes` in the compositor. It is computed over the
reference copy after the commit, as `width × 4` bytes per row, top to bottom,
in wire byte order:

```
h = 0xcbf29ce484222325
for each whole 8-byte chunk c, read as a little-endian u64:
    h = rotate_left((h ^ c) × 0x100000001b3, 29)
for each remaining byte b:
    h = (h ^ b) × 0x100000001b3
```

All arithmetic wraps at 64 bits. This is the spikes' `row_hash`, with rows
packed so stride padding doesn't count. JVM tests run the decoder against a
real compositor with `hashes: true` and compare every hash.

## Reserved for later steps

Names only. Each step specifies its own when it lands, and none of these
change step 1's messages.

- **Step 2, the window model:**
  - `apps` (compositor → app): the app list
  - ICON `0x03`: an icon by desktop id
  - `launch` (app → compositor) and `launch-failed` (compositor → app)
  - `activate` (compositor → app): an xdg-activation request to raise a window
  - new `toplevel` fields: `launch` (the launch it fulfils) and `tab`
    (`POCKETNIX_TAB`)
  - a dialog's surfaces may come with its parent's `window`, so dialogs draw
    as layers in the parent with no new message
- **Step 3, phone input:**
  - TOUCH_* `0x40`–`0x44` and POINTER_RELATIVE `0x24`
  - `text-input` (compositor → app)
  - `ime-commit`, `ime-preedit` and `ime-delete` (app → compositor)
  - `keyboard-layout` and `settings` (app → compositor; per-app size and
    input mode)
- **Step 4, docked polish:** CURSOR_IMAGE `0x04`, toplevel icons through ICON,
  and a `decoration` field in `window`.

No clipboard, ever: the data-control bridge handles it (`gui-apps.md`).

## Contract

Normative for step 1. Paths are under the session user's runtime directory,
`$XDG_RUNTIME_DIR` (`/run/user/<uid>`).

### Compositor

- Crate `compositor/`, binary `pocketnix-compositor`. wprs's Apache-2.0
  `LICENSE` and `NOTICE` stay in the crate.
- `pocketnix-compositor [--gui-socket PATH] [--wayland-display NAME] [--log-hashes]`
- Sockets: it takes listening `AF_UNIX` stream sockets from systemd socket
  activation (`LISTEN_PID`, `LISTEN_FDS`, `LISTEN_FDNAMES`): the one named
  `gui` is the GUI socket, `wayland` the Wayland socket. It unsets the
  `LISTEN_*` variables after taking them. A socket not passed in is created:
  - the GUI socket at `--gui-socket`, default `$XDG_RUNTIME_DIR/pocketnix-gui.sock`
  - the Wayland socket at `$XDG_RUNTIME_DIR/<NAME>`, default `wayland-0`, with
    the usual `<NAME>.lock`
- It sets `WAYLAND_DISPLAY` for the processes it starts.
- It accepts GUI connections from its own uid and root (`SO_PEERCRED`), one
  at a time, the newest winning.
- It keeps running, with its clients, while no app is connected.
- `--log-hashes`, or `POCKETNIX_COMPOSITOR_LOG_HASHES=1`, logs one stderr line
  per BUFFER commit: `hash surface=<id> seq=<seq> <width>x<height> <hash as %016x>`.
- Logging follows `RUST_LOG`.

### Agent

- Stream kind `protocol.StreamGUI = "gui"`; its Hello has no other fields.
  - With GUI off, the agent rejects it with an Error, "GUI is not enabled".
  - Otherwise it dials the GUI socket and rejects with the dial error if that
    fails.
  - On success it sends its Hello and splices raw bytes both ways until either
    side closes. It never parses the GUI protocol.
- Agent config file (`agent.File`, agent-only): `"guiSocket"`, the GUI socket
  path, absolute or relative to the session user's runtime directory. Empty
  means `pocketnix-gui.sock`.
- Control stream `Config` push (`protocol.Config`): `"gui"`
  (`GUI *GUI json:"gui,omitempty"`):
  ```json
  {"enable": true,
   "defaults": {"size": "phone", "input": "touch"},
   "appDefaults": {"org.gnome.Calculator": {"size": "compact"}}}
  ```
  - `size` is `phone`, `compact` or `desktop`, and `input` is `touch`,
    `pointer` or `trackpad`. Every field is optional: an app id inherits what
    it leaves out from `defaults`.
  - GUI is on exactly when `gui.enable` is true. The app then keeps one `gui`
    stream open, reconnecting with backoff.
  - An older app ignores the field, and an older agent never sends it.
- Step 2: `POCKETNIX_TAB=<id>` in every PTY session's environment.

### NixOS module

- `pocketnix.guest.gui.enable` (default false) and `pocketnix.guest.gui.package`
  (default: this flake's `pocketnix-compositor`).
- `pocketnix.guest.gui.defaults` (`size`, `input`) and
  `pocketnix.guest.gui.appDefaults.<app-id>` (the same options, null to
  inherit) go into the agent config's `gui`. They may reach the app before
  anything uses them.
- With `gui.enable`, the module installs:
  - user socket `pocketnix-gui.socket`: `ListenStream=%t/pocketnix-gui.sock`,
    `FileDescriptorName=gui`, `SocketMode=0600`
  - user socket `pocketnix-wayland.socket`: `ListenStream=%t/wayland-0`,
    `FileDescriptorName=wayland`, `SocketMode=0600`
  - Both are wanted by `sockets.target`, with `Service=pocketnix-compositor.service`.
  - user service `pocketnix-compositor.service`: `ExecStart=pocketnix-compositor`,
    `Sockets=` both sockets
- It also sets:
  - `WAYLAND_DISPLAY=wayland-0` and `NIXOS_OZONE_WL=1`, both in
    `environment.sessionVariables` (the tabs' login shells) and in the user
    manager's environment (user services, D-Bus-activated apps)
  - `"gui": {"enable": true, …}` in the agent config
  - with `mkDefault`: `services.pipewire.enable` (with `pulse`),
    `xdg.portal.enable` (and through it the FileChooser integration), a GTK
    portal backend as `xdg.portal.config.common.default`, and
    `fonts.enableDefaultPackages`
- The session user's lingering, which the module already sets, is what
  starts the user manager and so the sockets.
