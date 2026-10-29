# GUI protocol

The wire between the guest compositor (`pocketnix-compositor`) and the app
(see [GUI apps](gui.md) for the user-facing side). The agent only carries it.
Features not built yet have reserved names (see Reserved), so adding them
only adds things. Contract is the agreement between the compositor, the
agent, the NixOS module and the app.

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
  binary fields it knows. That is how the protocol is extended. A type
  number or message name never changes meaning; an incompatible change bumps
  the version.
- Any other violation (bad length, malformed JSON, a commit that doesn't
  decode, an unknown surface in a commit) is fatal: the receiver closes the
  connection. The app then reconnects and gets a replay, so an error costs a
  redraw and never an app.

### Frame types

| type | name | direction |
|---|---|---|
| `0x00` | JSON | both |
| `0x01` | COMMIT | compositor → app |
| `0x02` | COMMIT_DATA | compositor → app |
| `0x03` | ICON | compositor → app |
| `0x04` | CURSOR_IMAGE (reserved) | compositor → app |
| `0x10` | PRESENTED | app → compositor |
| `0x20` | POINTER_MOTION | app → compositor |
| `0x21` | POINTER_BUTTON | app → compositor |
| `0x22` | POINTER_AXIS | app → compositor |
| `0x23` | POINTER_LEAVE | app → compositor |
| `0x24` | POINTER_RELATIVE (reserved) | app → compositor |
| `0x30` | KEY | app → compositor |
| `0x31` | MODIFIERS | app → compositor |
| `0x40` | TOUCH_DOWN | app → compositor |
| `0x41` | TOUCH_UP | app → compositor |
| `0x42` | TOUCH_MOTION | app → compositor |
| `0x43` | TOUCH_CANCEL | app → compositor |
| `0x44` | TOUCH_FRAME | app → compositor |

## Session

### Handshake

1. The app sends `hello`:
   `{"type":"hello","version":2,"minVersion":2,"app":"<build>","hashes":false}`.
   `hashes: true` asks for a content hash on every commit that carries pixels.
2. The compositor answers with
   `{"type":"hello","version":<v>,"compositor":"<build>","instance":"<id>"}`,
   where `v` is the highest version both speak (the agent's `Negotiate` rule),
   or with `{"type":"error","message":"…"}` and closes.
   `instance` is a random string fixed for the compositor process's lifetime.
3. The compositor starts the replay at once. The app, after reading the hello,
   sends `output`, `settings` and `keyboard-layout`, and then a `window` for
   each window it still shows from the same `instance`.

Surface ids are `u32`, assigned by the compositor, unique within an `instance`
and never reused in it. Ids from another instance mean nothing. When the
instance differs from the one the app last saw, the compositor restarted and
its clients died with it: the app closes every window it had.

The protocol version is the GUI protocol's own, independent of the agent's.
This page is version 2. Version 1, never released and spoken by neither side
(both send `minVersion` 2), showed every toplevel as a window of its own;
version 2 draws a child toplevel inside its parent's window (see Dialogs),
which changes what `toplevel` and a commit's `window` mean. Everything else
version 2 added is new messages, fields and frame types. A side that lacks
one behaves as if it were absent (a `toplevel` without `touch` has none, so the
app uses Pointer mode).

### Replay

On every connection, after its `hello`, the compositor sends its whole state:

- for each window: its `toplevel`, then the `toplevel` of each dialog in it
  (parents before children), a `surface` for each surface in the window
  (dialogs' included), then one group of FULL commits for every surface of
  the window that has a buffer
- `cursor`
- `text-input`
- `apps`
- `ready`

After `ready` the app closes any window it still had that wasn't replayed,
and sends `launch` again for every launch it still waits on (see Resending a
launch). Commits that follow are relative to the replayed content.

### One connection at a time

The compositor serves one app connection. When a new one finishes its hello,
the compositor closes the old one and discards everything queued for it, so
no half-sent commit is replayed. The new connection gets a full replay from
the compositor's per-surface mirror. Wayland clients never see a reconnect,
other than frame callbacks pausing while no app is connected.

## Windows and surfaces

- A **window** is a mapped xdg toplevel without a parent, shown as one
  Android window. Its id is its root `wl_surface`'s id.
- A **dialog** is a mapped xdg toplevel with a parent: `xdg_toplevel.set_parent`,
  or an xdg-foreign-v2 import (a portal's file chooser names its app's window
  that way). It draws inside the window of its topmost ancestor, never as an
  Android window of its own (see Dialogs).
- Every surface the app draws (a toplevel's root surface, its subsurfaces,
  popups and their subsurfaces) belongs to exactly one window and is a layer
  in that window's stack. The compositor constrains popups, a dialog's
  included, inside their window, since Android clips child layers to the task.
- A window's coordinate origin is the top-left of the toplevel's window
  geometry (`xdg_surface.set_window_geometry`, or the whole root surface when
  it's unset). The Android window shows `(0, 0)` to `(width, height)` of that
  space. A root surface with client-side shadows has a negative position.
- The compositor converts logical coordinates to window pixels with the
  window's `scale`: an edge at logical `l` is at `round(l × scale)`, and a
  size is the difference of its rounded edges. The app does no scale
  arithmetic for placement.
- Cursor surfaces aren't sent (see `cursor`).

### Scale

- A window's `scale` (from `window`) is the display's density times the
  app's size factor for it, which the app lowers when the window's minimum
  size doesn't fit ([GUI apps](gui.md#size)). Every surface drawn in the window
  renders at it, whichever toplevel or client it belongs to: the compositor
  gives each the window's preferred fractional scale and `ceil` of it as
  `wl_surface.preferred_buffer_scale`.
- Everything a client places is in its window's logical space and becomes
  window pixels through that one scale: subsurface offsets, popup
  positions, dialog placement, input regions, the text input's cursor
  rectangle and, once CURSOR_IMAGE exists, the cursor hotspot. Popup positioners are
  solved against the window's logical size (`width / scale` ×
  `height / scale`). Input goes the other way: pointer and touch positions
  are divided by the window's scale before hit-testing.
- Until the app's first `window` for a new window, its scale is
  `output.scale` times its entry's size factor from `settings`, and its
  first configure is the output's size at that scale. The entry is the one
  the toplevel matches when that configure is sent (see Which entry a
  toplevel is; a launch is known then by its scope, and by its token only if
  the client activated first); none known yet means the default factor.
- When a window's scale changes, the compositor dismisses its open popups
  (`xdg_popup.popup_done`): they were placed for the old logical size.
- `wl_output.scale` is per client, not per surface. A client that reads
  only that (GTK3) and has windows at different scales renders them all at
  the integer scale of the one whose scale changed last; each commit's
  destination size still places them right.

### Compositor → app JSON

| message | fields | meaning |
|---|---|---|
| `toplevel` | `id`, `window`, `parent`, `title`, `appId`, `app`, `launch`, `tab`, `width`, `height`, `minWidth`, `minHeight`, `maxWidth`, `maxHeight`, `scale`, `decoration`, `touch` | A toplevel mapped, or its metadata changed. Sent whole every time. `window`: the window it draws in, its own `id` for a window. `parent`: the parent toplevel's id, 0 for a window. `app`: the app-list entry it belongs to (see Which entry a toplevel is). `launch`: the launch it was bound to when it first mapped (see Binding a window to a launch), kept for the toplevel's life. `tab`: where the window goes (see Where a window goes); `""` for a dialog. `width`/`height`: the window geometry's current size. Min/max: 0 for none. `scale`: the scale those pixel sizes were computed with, the window's (so the app recovers the client's logical minimum as `minWidth / scale`, to within a pixel). `decoration`: `"client"` or `"server"` (the xdg-decoration mode in effect). `touch`: whether the toplevel's client has a `wl_touch` (see Touch). Strings may be empty. |
| `toplevel-removed` | `id` | The toplevel was unmapped or destroyed. For a window, the app forgets the window and every surface and dialog in it. For a dialog, it forgets the dialog and the surfaces whose `toplevel` is `id`. If it maps again it's announced again, with the same id, followed by FULL commits. |
| `toplevel-request` | `id`, `request` | The client asked for `minimize`, `maximize`, `unmaximize`, `fullscreen`, `unfullscreen`, `move` or `resize`. Advisory; the app may ignore any of them. Never sent for a dialog. A full screen or maximize request (or its undoing) made before the app knew the toplevel, as a client starting full screen makes before its first commit, follows its first `toplevel` message on a connection; only the last of each pair is kept. The app remembers full screen for a window it has no activity for yet. |
| `surface` | `id`, `window`, `toplevel`, `role`, `parent` | A surface joined a window, before its first commit. `toplevel`: the toplevel whose tree it's in (its own id for a toplevel's root surface; a popup belongs to the toplevel it was opened from). `role`: `"toplevel"`, `"subsurface"` or `"popup"`. `parent`: the parent surface's id (0 for a toplevel's root surface, a dialog's included). |
| `surface-removed` | `id` | Drop the layer and its content. |
| `cursor` | `window`, `name` | The pointer's cursor over `window`. `name` is a cursor-shape-v1 name (`"default"`, `"text"`, `"pointer"`, `"grab"`, `"ew-resize"`, …) or `"hidden"`. A client-drawn cursor surface comes as `"default"` (CURSOR_IMAGE is reserved). |
| `apps` | `apps` | The app list (see The app list). |
| `launch-started` | `id` | A launch's process started (see Launching). |
| `launch-failed` | `id`, `reason`, `message` | A launch ended without a window (see Launching). |
| `activate` | `window`, `launch` | A client asked to bring `window` forward (see Activation). |
| `text-input` | `window`, `session`, `applied`, `text`, `cursor`, `anchor`, `cause`, `purpose`, `hints`, `cursorRect` | The focused text input's state (see Text input). |
| `ready` | | End of the replay. |
| `error` | `message` | Sent before the compositor closes the connection over a protocol error. |

### App → compositor JSON

| message | fields | meaning |
|---|---|---|
| `output` | `width`, `height`, `scale`, `refresh` | The display a new window opens on: the size in pixels a new window would get, the density (`densityDpi / 160`, a float) and the refresh rate in mHz. Sent after the hello and whenever it changes. The compositor's `wl_output` and the initial configure of new toplevels follow it (see Scale). |
| `settings` | `size`, `apps` | The size factor each app's new windows open at (see Scale): `size` for any entry, and `apps`, an object keyed by entry id, `{"<id>": {"size": <factor>}}`, for those that differ. Factors are positive floats, 1 for Phone; the app sends the factors that apply on the display `output` describes. Sent after `output` on every connection and whenever a factor changes. Absent, every factor is 1. The input mode is the app's alone and isn't sent. |
| `keyboard-layout` | `layout`, `variant` | The guest's keymap (see Keyboard layout). |
| `window` | `id`, `width`, `height`, `scale`, `focused`, `visible`, `maximized`, `fullscreen` | The Android window's state for a window. `width`/`height` in pixels (0: let the client choose): the area the window's layers show in, without pocketnix's own controls. `scale`: pixels per logical pixel for this window (density times its size factor, lowered to fit; see Scale); the compositor gives the client this as its preferred fractional scale (and `ceil` of it as the integer buffer scale) and configures it at `round(width / scale)` × `round(height / scale)`. `focused` maps to the `activated` state and keyboard focus, `visible: false` to `suspended`, and the other two to the xdg states of the same names. Debounce while a resize is in progress. Ignored for a dialog's id. |
| `close` | `id` | `xdg_toplevel.close`, e.g. from `onDestroy` of a window the user closed. Works for a dialog too. |
| `move` | `id`, `tab` | Move window `id` to `tab` (see Moving a window). |
| `icon` | `icon`, `size` | Render an icon (see Icons). |
| `launch` | `id`, `app`, `action`, `tab` | Start an app (see Launching). |
| `launch-cancel` | `id` | The app stopped waiting for a launch (see Launching). |
| `text` | `window`, `text` | Type `text` as key presses (see Typing text). |
| `ime` | `session`, `serial`, `deleteBefore`, `deleteAfter`, `commit`, `preedit`, `preeditCursorBegin`, `preeditCursorEnd` | An edit from Android's IME (see Text input). |

## Commits

A COMMIT carries one surface's committed state: placement, buffer metadata,
damage and pixels, all in one message, so a buffer and its commit can't come
apart.

### COMMIT (`0x01`)

| offset | type | field |
|---|---|---|
| 0 | u16 | `headerSize`: offset of the rects; 88 in versions 1 and 2 |
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

- `window`: the window the surface draws in. For a dialog's surfaces that is
  its topmost ancestor's window.
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
a CPU copy or rebuild it from its buffers.

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
  `wl_surface.frame` callbacks by what the app reports shown. It may fire a
  commit's callbacks before that commit is shown, keeping a few of a
  window's groups on their way to the screen so that no stage waits on the
  others, and fewer when they're large, since those only queue in the
  transport. The compositor's `callback_wait` holds the rule.
- Every Wayland commit that requested a frame callback produces a COMMIT,
  even when nothing visible changed, so the app has something to acknowledge.
- **PRESENTED (`0x10`)**, `[window u32][seq u32]`: the app's commits for
  `window` up to `seq` are on screen. The app sends it once the transaction
  carrying a group has been committed by SurfaceFlinger. A group with nothing
  to show is acknowledged at the next Choreographer frame. The compositor then
  fires the callbacks that wait on that window's commits up to that number,
  its dialogs' included.
- For a window the app isn't showing (`visible: false`, not yet open, or
  closed but still mapped), the app still applies commits but sends no
  PRESENTED. The compositor holds that window's callbacks and tells the client
  it's `suspended`, and its dialogs' clients too. With no app connected, all
  callbacks are held.
- If commits back up for a surface, the compositor may replace that surface's
  unsent commits with one FULL commit from its mirror.

## Input

The app reports input in window pixels, relative to the window's origin. The
compositor divides by the window's scale, hit-tests the window's surfaces
(input regions, popups, subsurfaces, dialogs) and sends the Wayland events.
The app does no hit-testing. Event times come from the compositor's clock on
receipt. Zoom is the app's alone: it maps a position on the view back through
its zoom transform before sending it.

| type | payload |
|---|---|
| POINTER_MOTION `0x20` | `[window u32][x f32][y f32]`. Implies pointer enter. |
| POINTER_BUTTON `0x21` | `[window u32][x f32][y f32][button u32][state u8]`: `button` a Linux `BTN_*` code (`BTN_LEFT` 0x110, `BTN_RIGHT` 0x111, `BTN_MIDDLE` 0x112, `BTN_SIDE` 0x113, `BTN_EXTRA` 0x114); `state` 1 pressed, 0 released. |
| POINTER_AXIS `0x22` | `[window u32][x f32][y f32][source u8][stop u8][dx f32][dy f32][v120x i32][v120y i32]`: `source` 0 wheel, 1 finger, 2 continuous, 3 wheel tilt; `stop` bit 0 horizontal, bit 1 vertical (finger lifted); `dx`/`dy` in window pixels; `v120x`/`v120y` in 1/120 notches, 0 when not a wheel. Positive is right and down, Wayland's direction, so Android's `AXIS_VSCROLL` is negated. The compositor moves the pointer to `x`,`y` in a `wl_pointer.frame` of its own before the axis events' frame: GTK keeps a frame's axis events back when the frame also has a motion, and never scrolls. |
| POINTER_LEAVE `0x23` | `[window u32]`. |
| KEY `0x30` | `[window u32][key u32][state u8]`: `key` a Linux evdev `KEY_*` code (XKB keycode − 8); `state` 1 pressed, 0 released. |
| MODIFIERS `0x31` | `[window u32][mods u8]`: bit 0 shift, 1 ctrl, 2 alt, 3 logo, 4 caps lock, 5 num lock. |
| TOUCH_DOWN `0x40` | `[window u32][id u32][x f32][y f32]`: a touch point went down. |
| TOUCH_UP `0x41` | `[window u32][id u32]`: it lifted. |
| TOUCH_MOTION `0x42` | `[window u32][id u32][x f32][y f32]`: it moved. |
| TOUCH_CANCEL `0x43` | `[window u32]`: every point in the window ended without effect. |
| TOUCH_FRAME `0x44` | `[window u32]`: the end of one set of changes. |

- KEY and MODIFIERS move keyboard focus to `window` first if it's elsewhere:
  to the window's topmost grabbing popup, or else its focused toplevel (see
  Dialogs). The app sends MODIFIERS when a window gains focus and whenever
  the meta state changes, including modifier-only changes: Android's
  `metaState` as is.
- Clients repeat keys themselves, so the app drops events with
  `getRepeatCount() > 0`.
- When keyboard focus leaves a window, the compositor releases the keys it
  holds pressed. When the pointer leaves, it releases held buttons unless a
  drag is in progress. When the app's connection ends, it releases every
  key and button the app held.
- Keymap: the compositor's own XKB keymap interprets the codes (see
  Keyboard layout). The app sends `KeyEvent.getScanCode()` for hardware
  keyboards, which is the evdev code, and maps `keyCode` to evdev for events
  without one. A KEY code is a position in the guest's layout, so a virtual
  keyboard's printable keys, whose `keyCode` names a character rather than a
  position, go as `text` or `ime` instead.

### Touch

The app sends TOUCH_* in Touch mode ([GUI apps](gui.md#input)), for a window
whose `toplevel` has `touch: true`. A window whose client has no `wl_touch`
gets Pointer mode instead.

- `id` is Android's pointer id: unique among the window's points that are
  down, and reused once its point is up. A DOWN for an id that's already
  down ends that point first; an UP or MOTION for an id that isn't down is
  ignored.
- A DOWN is hit-tested and handled like a pointer press: a DOWN outside a
  grabbing popup dismisses the popups and goes no further, one over a modal
  dialog's parent goes nowhere, and one on a toplevel's surface makes it
  its window's focused toplevel and raises a dialog. A DOWN that hits no
  surface goes nowhere either. Such a point's later events are dropped.
- The point stays with the surface it went down on until UP or CANCEL:
  MOTION positions are relative to that surface, even outside it. A point
  whose surface's client has no `wl_touch` is dropped.
- A DOWN's serial counts as the client's input serial like a press's: for
  xdg-activation tokens (when the client has keyboard focus), `xdg_popup.grab`
  and `xdg_toplevel.move`, which for a dialog follows that point until it
  ends.
- The app sends TOUCH_FRAME after each Android `MotionEvent`'s changes, and
  the compositor then sends `wl_touch.frame` to each client it sent touch
  events since the last one.
- TOUCH_CANCEL is Android's `ACTION_CANCEL` (a system gesture took the
  touch): `wl_touch.cancel` to every client with points in the window, and
  all its points end. They end the same way when the window unmaps, and
  every window's when the app's connection ends.
- `toplevel.touch` is true once the toplevel's client has a `wl_touch`; the
  compositor's seat always has the touch capability, so toolkits create one
  at startup. The compositor sends the `toplevel` again when that changes.

### Pointer and Trackpad modes

Both are made on the app side from Android gestures into the pointer frames
above, and need nothing else on the wire.

- Pointer: a tap is a `BTN_LEFT` press and release at the finger, a long
  press a `BTN_RIGHT` click, and a two-finger drag POINTER_AXIS with source
  finger, with the stop bits when the fingers lift. The finger marks the
  spot, so the app draws no cursor.
- Trackpad: the app keeps the cursor's position, moves it by the finger's
  relative motion (clamped to the window), sends it as POINTER_MOTION and
  clicks there. `wl_pointer` positions are absolute and the app needs the
  position to draw the cursor, so there's no relative motion on the wire;
  POINTER_RELATIVE stays reserved for relative-pointer (Android pointer
  capture).
- Android draws no pointer for a touchscreen, so in Trackpad mode the app
  draws its own cursor at that position, picking the image by `cursor`'s
  `name` and drawing none for `"hidden"`.

### Keyboard layout

`keyboard-layout` (app → compositor),
`{"type":"keyboard-layout","layout":"dk","variant":""}`: XKB layout and
variant names for the guest's keymap, from Android's physical keyboard
layout (that of the keyboard last typed on). `layout` `""` means the
compositor's default: its environment's `XKB_DEFAULT_LAYOUT` and friends,
`us` otherwise. The app sends it after `output` on every connection and
whenever it changes.

- The compositor compiles the keymap with the `evdev` rules and sends it to
  every client. A layout or variant it can't compile is logged, and the
  current keymap stays.
- Processes it launches get `XKB_DEFAULT_LAYOUT` and `XKB_DEFAULT_VARIANT`
  set to it, for nested desktops, which read their own and ignore the
  keymap they're sent.

### Typing text

`text` (app → compositor), `{"type":"text","window":<id>,"text":"…"}`: type
`text` into `window` as key presses. The app uses it for the keyboard button
on a window with no enabled text input, and for each character typed while
the extra keys row holds Ctrl or Alt, so it reaches the client as a key
combination.

- Keyboard focus moves to `window` first, as for KEY.
- Per character: `\n` is Return and `\t` Tab; other control characters are
  dropped. Any other character's keysym (`xkb_utf32_to_keysym`) is looked up
  in the current keymap, at the lowest keycode and then the lowest level
  that produce it. The compositor presses and releases that key with the
  level's Shift and level-3 modifiers set and Ctrl, Alt and Logo as
  MODIFIERS left them, then restores the modifier state.
- A keysym the keymap lacks is typed as `wtype` does: the compositor sends
  every client a keymap that is the current one plus the missing keysyms on
  unused keycodes (Smithay has one keymap per seat), types them, and sends
  the layout's keymap again at the end of the message. Each keymap change
  keeps the modifiers and locks as they were.

## Text input

The compositor implements `zwp_text_input_manager_v3` itself (Smithay's
serves only an input-method-v2 client) and bridges the focused text input to
Android's IME. Offsets and lengths are UTF-8 bytes, as in text-input-v3 (see
Units).

### `text-input`

Compositor → app, sent whole every time:

| field | meaning |
|---|---|
| `window` | The window whose keyboard focus has an enabled text input, or 0 for none. |
| `session` | Which enable this is, or 0 for none (below). |
| `applied` | The `serial` of the last `ime` the compositor had sent the client when the client committed this state; 0 for none. |
| `text` | The surrounding text (`set_surrounding_text`), or `null` when the client sent none. |
| `cursor`, `anchor` | The cursor and the selection's other end, in bytes of `text`. Equal without a selection. |
| `cause` | `"input-method"` when the client says the change came from an `ime`, `"other"` otherwise (`set_text_change_cause`). |
| `purpose` | The content purpose's name: `"normal"`, `"alpha"`, `"digits"`, `"number"`, `"phone"`, `"url"`, `"email"`, `"name"`, `"password"`, `"pin"`, `"date"`, `"time"`, `"datetime"` or `"terminal"`. |
| `hints` | The content hints' names, a list from `"completion"`, `"spellcheck"`, `"auto-capitalization"`, `"lowercase"`, `"uppercase"`, `"titlecase"`, `"hidden-text"`, `"sensitive-data"`, `"latin"` and `"multiline"`. |
| `cursorRect` | `{"x","y","width","height"}`, the client's cursor rectangle in window pixels, or `null` when it set none. |

- There is one text input at a time: the enabled `zwp_text_input_v3` of the
  surface with keyboard focus, whose `enter` the compositor sends as that
  focus moves. With several on one client, the last enabled one counts.
- The compositor sends it in the replay, after each client commit that
  changes any field, and with `window` 0 and `session` 0 when the text
  input goes away (disabled, focus elsewhere, destroyed).
- **Sessions:** a new `session` begins with each `enable` the client
  commits, and when focus moves to another enabled text input. Sessions
  count up from 1 per connection. On a new session the app restarts its
  IME connection with the new `purpose` and `hints`; later messages of the
  same session update it.
- **`applied`:** text-input-v3 never tells the compositor which of its
  events a client commit reflects, so `applied` is the compositor's best
  knowledge. While it is below the last `ime` the app sent, `text`, `cursor`
  and `anchor` may not show the app's latest edits yet, and the app keeps
  its own copy of the text until a state catches up.
- The app shows the phone keyboard for a session and hides it when the
  session ends, unless the user opened it with the keyboard button.

### `ime`

App → compositor:
`{"type":"ime","session":<n>,"serial":<n>,"deleteBefore":0,"deleteAfter":0,"commit":"","preedit":"","preeditCursorBegin":0,"preeditCursorEnd":0}`.
Every field but `session` and `serial` is optional, with the defaults shown,
except that the preedit cursor defaults to the end of `preedit`.

- One `ime` is one text-input-v3 `done`. The compositor sends
  `delete_surrounding_text` when either length isn't 0, `commit_string`
  when `commit` isn't empty, `preedit_string` when `preedit` isn't empty,
  and `done` with the text input's commit count, which it keeps. The client
  then removes its current preedit, deletes `deleteBefore` bytes before the
  cursor and `deleteAfter` after it (a selection excluded), inserts `commit`
  at the cursor, and shows `preedit` there with its cursor at
  `preeditCursorBegin`..`preeditCursorEnd` (bytes of `preedit`, both -1 to
  hide it).
- So `preedit` replaces the preedit: an `ime` that keeps one repeats it.
  Android's `setComposingText` is a `preedit`; `commitText` a `commit` with
  no `preedit`; `finishComposingText` a `commit` of the composing text.
- `serial`: the app's own count of `ime` messages on the connection, from 1.
- `session`: the session the IME's edit was made in. The compositor drops
  an `ime` whose session isn't the current one: the user moved to another
  field or window, and the edit was meant for text that isn't there. Within
  a session nothing is dropped: an edit is relative to the client's cursor,
  which text-input-v3 is built for.
- text-input-v3 can't move the cursor or select. The app turns the IME's
  `setSelection` and its key events (Enter, Backspace, arrows) into KEY
  presses. A `"\n"` the IME commits, and its editor action, are a Return
  press too.
- GTK and Qt queue key events but apply a `done` as it arrives, so an
  `ime` sent right after KEY presses it depends on would act first. The
  app holds such an `ime` until a `text-input` state shows the keys
  applied, and sends it anyway if no state comes within a commit's round
  trip. Typing over a selection needs no key: a `commit` replaces the
  selection in GTK, Qt and Chromium.
- With `text` `null` the app can't see what the client did with an edit,
  and a client that commits on its own (foot commits a cursor rectangle
  after every redraw) ignores a `done` whose serial is behind its own count.
  So for such a text input the app sends committed text as `text` and only
  the preedit as `ime`.

### Units

Android counts UTF-16 code units; text-input-v3 counts UTF-8 bytes. The app
converts both ways, against its own copy of the text, which is the text the
IME acted on and can be ahead of the client's:

- A character outside the BMP is 2 units and 4 bytes, and no offset or
  length splits a character. The compositor passes the client's offsets
  through, and the app clamps one that isn't on a character boundary.
- A delete reaching past the text the app knows is clamped to it. With
  `text` `null`, the app sends deletes as BackSpace and Delete KEY presses,
  one per character.
- Offsets are within `text`, which may be only part of a longer document
  (text-input-v3 caps it at 4000 bytes); the app gives them to Android as
  positions in `text`.

## Dialogs

A dialog (a toplevel with a parent) is a set of layers in its window, the
way popups are. The app treats its surfaces like any others in the window;
everything below is the compositor's.

- **Placement:** centred on its parent's window geometry (the parent may be
  a dialog), then slid to lie inside the area popups are constrained to. When
  it doesn't fit, its top-left corner shows. It's centred again whenever its
  size changes and slid back inside whenever the window's size changes. Once
  the user has moved it, it keeps its offset from its parent instead,
  constrained the same way.
- **Size:** its first configure has no size (the client picks) and
  `bounds` set to that area's logical size. If it maps larger than the area,
  the compositor configures it to the area's size; a client whose minimum
  size is larger stays clipped.
- **Requests:** the compositor handles `xdg_toplevel.move` itself: the dialog
  follows the pointer inside the window until the button that started it is
  released. It ignores a dialog's minimize, maximize, fullscreen and resize
  requests, and sends no `toplevel-request` for a dialog.
- **Stacking:** each toplevel's surfaces stack together, its popups at their
  top. A dialog stacks above its parent, and sibling dialogs in the order they
  were last raised. Mapping raises a dialog, and so does a pointer press on
  one of its surfaces. A restack is one group, like any other.
- **Focus:** each window has a focused toplevel, initially the window's own.
  A dialog that maps becomes its window's focused toplevel, as does a toplevel
  a pointer button is pressed on. When a dialog unmaps, focus goes back to its
  parent. Keyboard input for the window goes to its topmost grabbing popup,
  or else to its focused toplevel. Only the focused toplevel has the
  `activated` state, and only while the window is `focused`.
- **Modal dialogs:** the compositor offers xdg-dialog-v1. While a dialog
  marked modal (`set_modal`) is mapped, its parent never becomes the focused
  toplevel, and pointer buttons and scrolls over the parent's surfaces are
  dropped. Clients without xdg-dialog-v1 (GTK3) make their dialogs modal
  themselves.
- **State:** a dialog's surfaces use its window's `scale`, it's `suspended`
  while its window is, and it's never maximized or fullscreen.
- **Changing windows:** xdg-shell gives a dialog its grandparent when its
  parent unmaps, and none when there is no grandparent; `set_parent` can also
  change it. When the window a dialog draws in changes, the compositor sends
  `toplevel-removed` for it and announces it again where it now belongs:
  `toplevel`, its `surface`s and one group of FULL commits. A dialog that
  becomes a window takes its old window's `tab`.

## The app list

### `apps`

`{"type":"apps","apps":[<entry>, …]}`, compositor → app: the whole list every
time, sorted by `id`. Sent in every replay and again whenever the list
changes. The app keeps the last list in its storage, so the launcher and pins
work with the VM off.

| field | meaning |
|---|---|
| `id` | The desktop file id without its `.desktop` suffix: `org.gnome.Calculator` for `org.gnome.Calculator.desktop`. Apps use it as their Wayland app id, and `gui.appDefaults` is keyed by it. |
| `name` | `Name`. |
| `genericName` | `GenericName`, or `""`. |
| `comment` | `Comment`, or `""`. |
| `keywords` | `Keywords`, a list of strings. |
| `categories` | `Categories`, a list of strings. |
| `icon` | The icon's key (see Icons), or `""` for none. |
| `noDisplay` | `true` for an entry the launcher doesn't show (below). |
| `actions` | The entry's actions in `Actions=` order: `{"id", "name", "icon"}` each, `id` being the `[Desktop Action <id>]` group's name and `icon` a key or `""`. |

- **Where entries come from:** `$XDG_DATA_HOME/applications` (default
  `~/.local/share/applications`), then the `applications` directory of each
  `$XDG_DATA_DIRS` entry, in that order and recursively. The desktop file id
  is the path below `applications/` with `/` turned into `-`. The first
  directory that has an id wins, so a user's own file shadows the profile's.
- **Which ones:**
  - Only `Type=Application` entries.
  - `Hidden=true` removes the id, the copies in later directories included.
  - Dropped: a `TryExec` not found on `PATH`; `Terminal=true` (a terminal
    program belongs in a tab); no `Name`; no `Exec` without
    `DBusActivatable=true`; a file that doesn't parse (logged).
  - `noDisplay: true` for `NoDisplay=true` and for any `OnlyShowIn`: the
    compositor names no desktop (`XDG_CURRENT_DESKTOP` stays unset), and
    `NotShowIn` is ignored. These entries are listed anyway so their windows
    can be named (`toplevel.app`) and a launch of one works.
- **Strings:** localized keys (`Name[da_DK]`, `Name[da]`, …) are picked for
  the compositor's own locale (`LC_ALL`, `LC_MESSAGES`, `LANG`) by the
  Desktop Entry spec's matching rules. That's the guest's locale, the
  language the apps themselves will show.
- **Changes:** the compositor rescans when an app connection says hello, on
  inotify events in any `applications` directory it reads, and on `SIGHUP`
  (the module's activation sends one on every switch). It debounces by 1 s
  and sends `apps` only when the list differs from the last one it sent on
  that connection.
- **Size:** the message fits in `MaxFramePayload`. Past that the compositor
  drops `keywords`, then `comment` and `genericName`, and logs it.

### Which entry a toplevel is

A `toplevel`'s `app` is the `id` of: its launch's entry; or else the entry
whose `id` equals the toplevel's `appId`; or else the one whose
`StartupWMClass` does; or else `""`. A dialog is matched by its own `appId`.
When a new list changes a toplevel's `app`, the compositor sends its
`toplevel` again.

### Icons

- **Keys:** an `icon` key is an opaque string of 1–64 characters from
  `[0-9a-z]`. The compositor derives it from the content of every file the
  icon could be rendered from and its own rendering version, so it changes
  when those do and only then. Entries with the same icon share a key. The
  app caches rendered icons by key and size, across VM-off, and drops those
  no entry names any more.
- **`icon`** (app → compositor), `{"type":"icon","icon":"<key>","size":<px>}`:
  render that key at `size` × `size` pixels, `size` from 16 to 512 (the
  compositor clamps others). After each `apps`, the app asks for every key it
  lacks at every size it draws (the launcher grid, a shortcut, the splash),
  in pixels for the display. Requests a connection didn't answer are lost
  with it; the app asks again on the next.
- **ICON (`0x03`)**, compositor → app: `[size u16][keyLength u8][key][png]`.
  The PNG is RGBA, exactly `size` × `size`, the icon scaled to fit with its
  aspect ratio kept, centred on a transparent background. An empty `png`
  means no icon (the key isn't known any more, or its file can't be
  rendered); the app shows a generic one. The compositor answers each
  request once, in any order (it renders off its event loop), and never
  between a group's commits or a COMMIT's frames.
- **Lookup:** an absolute `Icon=` path is used as is. A name (a trailing
  `.png` or `.svg` dropped) is looked up in the `hicolor` theme, then
  `Adwaita`, under `$XDG_DATA_HOME/icons`, each data dir's `icons` and
  `~/.icons`, and then in each data dir's `pixmaps`. A theme's sizes come
  from its `index.theme`, or from the directory names (`48x48`, `scalable`)
  when it has none. An SVG is rendered at the size asked for; otherwise the
  smallest PNG at least that size is scaled down, or the largest one up.
  Other formats (XPM) render as no icon.

## Launching

A tap opens the window's Activity at once, on a splash. The app sends
`launch`; the compositor starts the app and binds the window it maps to the
launch, and the Activity then shows that window.

### Messages

- **`launch`** (app → compositor), `{"type":"launch","id":"…","app":"…","action":"…","tab":"…"}`:
  - `id`: the app's name for this launch, 1–64 characters from
    `[A-Za-z0-9_-]` and unique (a random UUID). A malformed `id` is a
    protocol error.
  - `app`: an entry's `id`. `action`: one of its actions' ids, or `""`.
  - `tab`: where its windows go, as in `toplevel.tab`: `""` for their own
    task.
- **`launch-cancel`** (app → compositor), `{"type":"launch-cancel","id":"…"}`:
  the app stopped waiting (the user closed the splash). If no window has been
  bound to the launch yet, the compositor stops its scope; a window that maps
  later anyway isn't bound to it.
- **`launch-started`** (compositor → app), `{"type":"launch-started","id":"…"}`:
  the process started, or its D-Bus activation was sent. Informative: after a
  reconnect a launch can end without the app having seen it.
- **`launch-failed`** (compositor → app),
  `{"type":"launch-failed","id":"…","reason":"…","message":"…"}`. `message`
  is for display. `reason`:
  - `unknown`: the current list has no entry `app`, or it has no action
    `action` (a switch removed it). The app disables the entry's pins.
  - `exec`: it couldn't be started, or its process exited with a non-zero
    status before a window was bound ("exited with status 127").
  - `timeout`: no window within 90 s of the compositor receiving the launch.
    The process keeps running, and a window it maps later is an ordinary
    new window.
  - `closed`: only in answer to a resent launch: its window opened and
    closed while the app wasn't connected.

A launch ends exactly once: with a new window whose `toplevel` names it in
`launch`, with an `activate` that names it, or with `launch-failed`. A process
that exits with status 0 doesn't end it: a single-instance app's second
process hands the launch to the running one and exits, and that one maps or
activates a window with the launch's token later.

### Starting the process

- The compositor mints an xdg-activation token for the launch.
- It runs `systemd-cat --identifier=<id> systemd-run --user --scope
  --slice=app.slice --unit=app-pocketnix-<escaped id>-<random>
  --description=<name> -- <argv>`, with `<escaped id>` the entry id through
  `systemd-escape`. The app gets a scope of its own, named the way systemd
  names a desktop's apps, and its output goes to the user journal under the
  entry's id. The compositor watches the process for its exit status.
- `argv` is the entry's `Exec` (or the action's), unquoted by the Desktop
  Entry spec's rules. Field codes: `%f`, `%F`, `%u` and `%U` are dropped
  (launches carry no files yet), `%i` becomes `--icon <Icon>` when there is
  an icon, `%c` the name, `%k` the desktop file's path, `%%` a `%`; others
  are dropped. The program resolves through the compositor's `PATH`.
- The environment is the compositor's, plus `XDG_ACTIVATION_TOKEN` and
  `DESKTOP_STARTUP_ID` set to the token, and `POCKETNIX_TAB=<tab>` when the
  launch names a tab, so the process's later windows go there too. The
  working directory is the entry's `Path`, or the user's home.
- An entry with `DBusActivatable=true` and no `Exec` is started over the
  session bus instead: `org.freedesktop.Application.Activate` (or
  `ActivateAction` for an action) on its well-known name, with
  `activation-token` and `desktop-startup-id` in the platform data. An entry
  with both uses `Exec`; the app forwards to its running instance itself.

### Binding a window to a launch

When a toplevel without a parent first maps, the compositor binds it to the
oldest pending launch that matches, checking in this order:

1. **Token:** the client activated the surface with the launch's token before
   mapping it. Every app tried does so between its first role commit and its
   first buffer, and a single-instance app does it from the running process.
2. **Scope:** the client's pid (`SO_PEERCRED` on its Wayland connection) is in
   the launch's scope (`/proc/<pid>/cgroup`).
3. **App id:** its `appId` is the launch's entry `id` or that entry's
   `StartupWMClass`.

A toplevel nothing matches has `launch` `""`. A dialog never binds a launch.

### Activation

- **`activate`** (compositor → app), `{"type":"activate","window":<id>,"launch":"…"}`:
  a client activated a mapped toplevel in `window` with a valid token. For a
  dialog, the compositor also raises it in its window.
  - `launch`: the launch whose token it was, which this ends (an existing
    window: a single-instance app's, or one that mapped before activating);
    `""` otherwise.
  - Valid tokens are a pending launch's, and those a client minted with the
    serial of a recent input event on a surface that had keyboard focus.
    Activations with any other token are dropped, so a background app can't
    steal focus.
- The app brings the window forward when Android lets it (pocketnix in the
  foreground) and otherwise ignores it. A launch's `activate` arrives while
  its splash is showing, which is in the foreground.

### Resending a launch

The compositor keeps every launch's outcome for its lifetime and never starts
an `id` twice. A `launch` with an `id` it already has gets that launch's
state again:

- still pending: `launch-started`
- ended with a window that's still mapped: `activate` naming the window and
  the launch
- ended with a window that's gone: `launch-failed` with `closed`
- failed: the same `launch-failed`
- cancelled: nothing

The app queues launches while it has no connection (a tap that boots the VM),
and after each `ready` sends `launch` for every launch it still waits on that
no replayed `toplevel` named. A compositor with a new `instance` knows no
earlier launch, so they start afresh.

## Where a window goes

### `tab`

A window's `tab` is `""` for its own Android task, or a tab id: then it shows
as a tab in the terminal screen, after that tab, or at the end of the tab
bar when the app has no such tab (it closed, or the id is from before an
agent restart). The compositor decides it when the window first maps:

1. A window bound to a launch takes the launch's `tab`, whatever its
   environment says. A launcher tap gives its own task even to a
   single-instance app first started from a tab.
2. Otherwise `POCKETNIX_TAB` from the client's own `/proc/<pid>/environ` (pid
   from `SO_PEERCRED`), read once per client.
3. Otherwise `""`.

Only the client's own environment counts, not its ancestors': children
inherit the variable, so a client without it was started outside a tab
(systemd, D-Bus activation, ssh) or cleared its environment on purpose. The
compositor treats the value as opaque but takes only 1–64 characters from
`[A-Za-z0-9_-]`, and anything else as unset.

### Moving a window

`move` (app → compositor), `{"type":"move","id":<window>,"tab":"…"}`: the
user moved the window between its own task (`""`) and a tab (the id of the
tab it now follows). The compositor stores the new `tab` for the toplevel's
life, across reconnects, and sends its `toplevel` again. The app moves the
window when that `toplevel` arrives, so a move and a replay take one path.
It doesn't change where the client's later windows go. A `move` for a dialog
or an unknown id is ignored.

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

All arithmetic wraps at 64 bits. Rows are packed so stride padding doesn't
count.

## Reserved

Names only, specified when they're built. None of them changes an existing
message.

- Docked polish: CURSOR_IMAGE `0x04`, the client's cursor
  surface at its window's scale with the hotspot in window pixels; toplevel
  icons (xdg-toplevel-icon) as an `icon` key in `toplevel`, rendered
  through `icon` and ICON; a `decoration` field in `window`.
- POINTER_RELATIVE `0x24`, for relative-pointer and pointer
  constraints with Android's pointer capture.

No clipboard: the compositor offers data-control, for a guest-side bridge to the clipboard integration.

## Contract

Normative. Paths are under the session user's runtime
directory, `$XDG_RUNTIME_DIR` (`/run/user/<uid>`).

### Compositor

- Crate `compositor/`, binary `pocketnix-compositor`. wprs's Apache-2.0
  `LICENSE` and `NOTICE` stay in the crate.
- `pocketnix-compositor [--gui-socket PATH] [--wayland-display NAME] [--stats-socket PATH] [--frames-in-flight N] [--log-hashes]`
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
- It offers xdg-activation-v1, xdg-dialog-v1 and xdg-foreign-v2.
- Its seat has the touch capability, and it offers `zwp_text_input_manager_v3`
  with its own dispatch and no input-method-v2.
- Its keymap comes from its environment's `XKB_DEFAULT_*` (`us` without
  them) until the app sends a `keyboard-layout`; the processes it launches
  get that layout as `XKB_DEFAULT_LAYOUT` and `XKB_DEFAULT_VARIANT`.
- It reads the app list and icons through its environment's `XDG_DATA_HOME`,
  `XDG_DATA_DIRS` and `HOME`, names them in its locale (`LC_ALL`,
  `LC_MESSAGES`, `LANG`), and resolves `TryExec` and `Exec` through its
  `PATH`. `SIGHUP` makes it rescan the list.
- It launches with `systemd-cat` and `systemd-run` from its `PATH`, into
  `app-pocketnix-*` scopes under `app.slice`, outside its own cgroup.
- It reads `POCKETNIX_TAB` from `/proc/<pid>/environ` of its own uid's
  clients.

### Agent

- Stream kind `protocol.StreamGUI = "gui"`; its Hello has no other fields.
  - With GUI off, the agent rejects it with an Error, "GUI is not enabled".
  - Otherwise it dials the GUI socket and rejects with the dial error if that
    fails.
  - On success it sends its Hello and splices raw bytes both ways until either
    side closes. It never parses the GUI protocol.
- Agent config file: `guiSocket` (`agent.File.GUISocket`) is the GUI
  socket it dials.
- Control stream `Config` push: `gui` (`protocol.GUI`), the module's
  `gui.enable`, `gui.defaults` and `gui.appDefaults`.
  - `appDefaults` is keyed by app-list entry `id`, and an entry inherits
    what it leaves out from `defaults`.
  - The app applies them, under the user's own changes per entry; the
    compositor gets only the resulting size factors, through `settings`.
  - GUI is on exactly when `gui.enable` is true. The app then keeps one `gui`
    stream open, reconnecting with backoff.
  - An older app ignores the field, and an older agent never sends it.
- Every PTY session's environment has `POCKETNIX_TAB=<tab>`, and the agent's
  PTY Hello carries the same id as `tab` (`protocol.Hello.Tab`): that's how
  the app knows each terminal tab's id. The id is 16 random lowercase hex
  digits per session, so one from before an agent restart never names a new
  tab.

### NixOS module

The options and what `gui.enable` turns on besides are in `nix/module.nix`.

- The `gui` options go into the agent config's `gui`. They may reach the app
  before anything uses them.
- With `gui.enable`, the user service `pocketnix-compositor.service` is
  socket-activated by two user-only sockets wanted by `sockets.target`: the
  GUI socket at `%t/pocketnix-gui.sock`, named `gui`, and the Wayland socket
  at `%t/wayland-0`, named `wayland`. Reloading it sends `SIGHUP`, and the
  user activation script that every boot and switch runs reloads it when
  it's active, so a switch refreshes the app list.
- `WAYLAND_DISPLAY=wayland-0` and `NIXOS_OZONE_WL=1` are set both in
  `environment.sessionVariables` (the tabs' login shells) and in the user
  manager's environment (user services, D-Bus-activated apps).
- The compositor's `XDG_DATA_DIRS` and `PATH` are the user manager's, which
  pam_env fills from NixOS's profile-relative session variables: both cover
  the user's and the system's profiles. The module adds nothing to them; it
  only sets `enableDefaultPath = false` on the service, since NixOS would
  otherwise replace that `PATH` with a minimal one of its own.
- The session user's lingering, which the module already sets, is what
  starts the user manager and so the sockets.
