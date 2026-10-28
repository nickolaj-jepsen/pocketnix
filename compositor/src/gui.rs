//! The app's connection: accepting it, the handshake and replay, and the
//! messages the app sends (window state, acks and input).

use std::collections::HashMap;
use std::io::BufWriter;
use std::io::ErrorKind;
use std::io::Read;
use std::io::Write;
use std::os::fd::AsRawFd;
use std::os::unix::net::UnixListener;
use std::os::unix::net::UnixStream;
use std::sync::Arc;
use std::sync::atomic::AtomicBool;
use std::sync::atomic::AtomicUsize;
use std::sync::atomic::Ordering;
use std::sync::mpsc;

use anyhow::Context;
use anyhow::Result;
use anyhow::bail;
use serde::Deserialize;
use serde_json::Value;
use serde_json::json;
use smithay::backend::input::Axis;
use smithay::backend::input::AxisSource;
use smithay::backend::input::ButtonState;
use smithay::backend::input::KeyState;
use smithay::input::keyboard::FilterResult;
use smithay::input::keyboard::KeyboardTarget;
use smithay::input::pointer::AxisFrame;
use smithay::input::pointer::ButtonEvent;
use smithay::input::pointer::MotionEvent;
use smithay::reexports::calloop::Interest;
use smithay::reexports::calloop::Mode;
use smithay::reexports::calloop::PostAction;
use smithay::reexports::calloop::RegistrationToken;
use smithay::reexports::calloop::generic::Generic;
use smithay::reexports::calloop::ping::Ping;
use smithay::reexports::wayland_server::Resource;
use smithay::reexports::wayland_server::protocol::wl_surface::WlSurface;
use smithay::utils::Logical;
use smithay::utils::Point;
use smithay::utils::SERIAL_COUNTER;
use smithay::wayland::compositor;
use tracing::debug;
use tracing::info;
use tracing::warn;

use crate::protocol;
use crate::protocol::Fields;
use crate::protocol::FrameReader;
use crate::state::OutputCfg;
use crate::state::Settings;
use crate::state::State;
use crate::state::WindowCfg;
use crate::state::apply_window_cfg;

/// Above this many queued bytes the compositor stops sending content until
/// the writer catches up; below half of it, it resumes.
const BACKLOG: usize = 16 << 20;

pub struct Gui {
    pub conns: HashMap<u64, Conn>,
    pub active: Option<u64>,
    /// The active connection's generation; 0 while none is active.
    pub generation: u64,
    next_gen: u64,
    next_id: u64,
    pub instance: String,
    pub log_hashes: bool,
    wake: Ping,
    starved: Arc<AtomicBool>,
}

pub struct Conn {
    stream: UnixStream,
    tx: Option<mpsc::Sender<Vec<u8>>>,
    queued: Arc<AtomicUsize>,
    discard: Arc<AtomicBool>,
    token: Option<RegistrationToken>,
    reader: FrameReader,
    ready: bool,
    pub hashes: bool,
    seq: u32,
    /// The last `toplevel` message sent per toplevel id, with its window.
    pub toplevels: HashMap<u32, (u32, Value)>,
    /// The surfaces announced per window id, with their toplevel's id.
    pub surfaces: HashMap<u32, Vec<(u32, u32)>>,
    /// The last `apps` message sent.
    pub apps: Option<Value>,
}

impl Conn {
    pub fn send(&self, frame: Vec<u8>) {
        if let Some(tx) = &self.tx {
            self.queued.fetch_add(frame.len(), Ordering::AcqRel);
            if tx.send(frame).is_err() {
                self.discard.store(true, Ordering::Release);
            }
        }
    }

    pub fn next_seq(&mut self) -> u32 {
        self.seq = self.seq.wrapping_add(1);
        self.seq
    }
}

impl Gui {
    pub fn new(log_hashes: bool, wake: Ping) -> Self {
        Self {
            conns: HashMap::new(),
            active: None,
            generation: 0,
            next_gen: 1,
            next_id: 1,
            instance: random_id(),
            log_hashes,
            wake,
            starved: Arc::new(AtomicBool::new(false)),
        }
    }

    pub fn active_conn(&self) -> Option<&Conn> {
        self.active.and_then(|id| self.conns.get(&id))
    }

    pub fn active_conn_mut(&mut self) -> Option<&mut Conn> {
        self.active.and_then(|id| self.conns.get_mut(&id))
    }

    /// Whether the active connection's writer is too far behind to take
    /// more content; it wakes the loop once it has caught up.
    pub fn backlogged(&self) -> bool {
        let Some(conn) = self.active_conn() else { return false };
        if conn.queued.load(Ordering::Acquire) < BACKLOG {
            return false;
        }
        self.starved.store(true, Ordering::Release);
        true
    }
}

fn random_id() -> String {
    let mut bytes = [0u8; 8];
    if std::fs::File::open("/dev/urandom")
        .and_then(|mut f| f.read_exact(&mut bytes))
        .is_err()
    {
        let t = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default();
        bytes = (t.as_nanos() as u64 ^ std::process::id() as u64).to_le_bytes();
    }
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// Whether the peer is our own user or root (the agent).
fn peer_allowed(stream: &UnixStream) -> bool {
    let mut cred = libc::ucred { pid: 0, uid: 0, gid: 0 };
    let mut len = std::mem::size_of::<libc::ucred>() as libc::socklen_t;
    // SAFETY: SO_PEERCRED writes a ucred of at most `len` bytes.
    let r = unsafe {
        libc::getsockopt(
            stream.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_PEERCRED,
            &mut cred as *mut libc::ucred as *mut libc::c_void,
            &mut len,
        )
    };
    // SAFETY: getuid can't fail.
    r == 0 && (cred.uid == 0 || cred.uid == unsafe { libc::getuid() })
}

fn writer(
    stream: UnixStream,
    rx: mpsc::Receiver<Vec<u8>>,
    queued: Arc<AtomicUsize>,
    discard: Arc<AtomicBool>,
    starved: Arc<AtomicBool>,
    wake: Ping,
) {
    let mut out = BufWriter::with_capacity(1 << 16, stream);
    while let Ok(mut frame) = rx.recv() {
        loop {
            if discard.load(Ordering::Acquire) {
                return;
            }
            let len = frame.len();
            if out.write_all(&frame).is_err() {
                discard.store(true, Ordering::Release);
                return;
            }
            let left = queued.fetch_sub(len, Ordering::AcqRel) - len;
            if left < BACKLOG / 2 && starved.swap(false, Ordering::AcqRel) {
                wake.ping();
            }
            match rx.try_recv() {
                Ok(next) => frame = next,
                Err(_) => break,
            }
        }
        if out.flush().is_err() {
            discard.store(true, Ordering::Release);
            return;
        }
    }
}

#[derive(Deserialize, Default)]
#[serde(default, rename_all = "camelCase")]
struct HelloMsg {
    version: u32,
    min_version: u32,
    hashes: bool,
}

#[derive(Deserialize, Default)]
#[serde(default, rename_all = "camelCase")]
struct OutputMsg {
    width: i32,
    height: i32,
    scale: f64,
    refresh: i32,
}

#[derive(Deserialize)]
#[serde(default, rename_all = "camelCase")]
struct WindowMsg {
    id: u32,
    width: u32,
    height: u32,
    scale: f64,
    focused: bool,
    visible: bool,
    maximized: bool,
    fullscreen: bool,
}

impl Default for WindowMsg {
    fn default() -> Self {
        Self {
            id: 0,
            width: 0,
            height: 0,
            scale: 0.0,
            focused: false,
            visible: true,
            maximized: false,
            fullscreen: false,
        }
    }
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct CloseMsg {
    id: u32,
}

#[derive(Deserialize)]
#[serde(default)]
struct SettingsMsg {
    size: f64,
    apps: HashMap<String, AppSettings>,
}

impl Default for SettingsMsg {
    fn default() -> Self {
        Self { size: 1.0, apps: HashMap::new() }
    }
}

#[derive(Deserialize)]
#[serde(default)]
struct AppSettings {
    size: f64,
}

impl Default for AppSettings {
    fn default() -> Self {
        Self { size: 1.0 }
    }
}

impl State {
    pub fn accept_gui(&mut self, listener: &UnixListener) {
        loop {
            let stream = match listener.accept() {
                Ok((stream, _)) => stream,
                Err(e) if e.kind() == ErrorKind::WouldBlock => return,
                Err(e) if e.kind() == ErrorKind::Interrupted => continue,
                Err(e) => {
                    warn!("accepting a GUI connection: {e}");
                    return;
                }
            };
            if !peer_allowed(&stream) {
                warn!("refused a GUI connection from another user");
                continue;
            }
            if let Err(e) = self.add_conn(stream) {
                warn!("GUI connection: {e:#}");
            }
        }
    }

    fn add_conn(&mut self, stream: UnixStream) -> Result<()> {
        stream.set_nonblocking(false)?;
        let id = self.gui.next_id;
        self.gui.next_id += 1;
        let (tx, rx) = mpsc::channel();
        let queued = Arc::new(AtomicUsize::new(0));
        let discard = Arc::new(AtomicBool::new(false));
        {
            let stream = stream.try_clone()?;
            let (queued, discard) = (queued.clone(), discard.clone());
            let (starved, wake) = (self.gui.starved.clone(), self.gui.wake.clone());
            std::thread::Builder::new()
                .name("gui-writer".into())
                .spawn(move || writer(stream, rx, queued, discard, starved, wake))?;
        }
        let token = self
            .lh
            .insert_source(
                Generic::new(stream.try_clone()?, Interest::READ, Mode::Level),
                move |_, stream, state| Ok(state.gui_readable(id, stream)),
            )
            .map_err(|e| anyhow::anyhow!("registering a GUI connection: {e}"))?;
        self.gui.conns.insert(
            id,
            Conn {
                stream,
                tx: Some(tx),
                queued,
                discard,
                token: Some(token),
                reader: FrameReader::default(),
                ready: false,
                hashes: false,
                seq: 0,
                toplevels: HashMap::new(),
                surfaces: HashMap::new(),
                apps: None,
            },
        );
        debug!("GUI connection {id}");
        Ok(())
    }

    fn gui_readable(&mut self, id: u64, stream: &UnixStream) -> PostAction {
        let mut buf = vec![0u8; 1 << 16];
        let n = match (&*stream).read(&mut buf) {
            Ok(0) => {
                debug!("GUI connection {id} closed");
                self.close_conn(id, false, false);
                return PostAction::Remove;
            }
            Ok(n) => n,
            Err(e) if matches!(e.kind(), ErrorKind::WouldBlock | ErrorKind::Interrupted) => {
                return PostAction::Continue;
            }
            Err(e) => {
                debug!("GUI connection {id}: {e}");
                self.close_conn(id, false, false);
                return PostAction::Remove;
            }
        };
        let Some(conn) = self.gui.conns.get_mut(&id) else {
            return PostAction::Remove;
        };
        conn.reader.push(&buf[..n]);
        loop {
            let Some(conn) = self.gui.conns.get_mut(&id) else {
                return PostAction::Remove;
            };
            let frame = match conn.reader.next_frame() {
                Ok(Some(frame)) => frame,
                Ok(None) => return PostAction::Continue,
                Err(e) => {
                    self.fail(id, e);
                    return PostAction::Remove;
                }
            };
            if let Err(e) = self.on_frame(id, frame.0, &frame.1) {
                self.fail(id, e);
                return PostAction::Remove;
            }
        }
    }

    /// Ends a connection over a protocol error, telling the app why.
    fn fail(&mut self, id: u64, err: anyhow::Error) {
        warn!("GUI protocol error: {err:#}");
        if let Some(conn) = self.gui.conns.get(&id) {
            conn.send(protocol::json_frame(&json!({"type": "error", "message": format!("{err:#}")})));
        }
        self.close_conn(id, false, false);
    }

    /// `remove_source` when not called from the connection's own source;
    /// `discard` drops whatever is still queued for it.
    fn close_conn(&mut self, id: u64, remove_source: bool, discard: bool) {
        let Some(mut conn) = self.gui.conns.remove(&id) else { return };
        if remove_source && let Some(token) = conn.token.take() {
            self.lh.remove(token);
        }
        if discard {
            conn.discard.store(true, Ordering::Release);
            let _ = conn.stream.shutdown(std::net::Shutdown::Both);
        }
        // The writer sends what's queued (unless discarded) and then exits.
        conn.tx = None;
        if self.gui.active == Some(id) {
            self.gui.active = None;
            self.gui.generation = 0;
            info!("app disconnected");
        }
    }

    fn on_frame(&mut self, id: u64, ty: u8, payload: &[u8]) -> Result<()> {
        let ready = self.gui.conns.get(&id).is_some_and(|c| c.ready);
        if !ready {
            if ty != protocol::JSON {
                bail!("frame type {ty:#04x} before hello");
            }
            let msg: Value = serde_json::from_slice(payload).context("malformed JSON")?;
            if msg.get("type").and_then(Value::as_str) != Some("hello") {
                bail!("expected hello");
            }
            return self.hello(id, serde_json::from_value(msg).context("malformed hello")?);
        }
        let mut f = Fields::new(payload);
        match ty {
            protocol::JSON => {
                let msg: Value = serde_json::from_slice(payload).context("malformed JSON")?;
                self.on_json(id, msg)?;
            }
            protocol::PRESENTED => {
                let (window, seq) = (f.u32()?, f.u32()?);
                self.presented(window, seq);
            }
            protocol::POINTER_MOTION => {
                let (window, x, y) = (f.u32()?, f.f32()?, f.f32()?);
                self.pointer_motion(window, x, y);
                self.pointer_frame();
            }
            protocol::POINTER_BUTTON => {
                let (window, x, y, button, state) = (f.u32()?, f.f32()?, f.f32()?, f.u32()?, f.u8()?);
                self.pointer_button(window, x, y, button, state != 0);
            }
            protocol::POINTER_AXIS => {
                let (window, x, y) = (f.u32()?, f.f32()?, f.f32()?);
                let (source, stop, dx, dy) = (f.u8()?, f.u8()?, f.f32()?, f.f32()?);
                let (v120x, v120y) = (f.i32()?, f.i32()?);
                self.pointer_axis(window, x, y, source, stop, (dx, dy), (v120x, v120y));
            }
            protocol::POINTER_LEAVE => {
                let window = f.u32()?;
                self.pointer_leave(window);
            }
            protocol::KEY => {
                let (window, key, state) = (f.u32()?, f.u32()?, f.u8()?);
                self.key(window, key, state != 0);
            }
            protocol::MODIFIERS => {
                let (window, mods) = (f.u32()?, f.u8()?);
                self.modifiers(window, mods);
            }
            protocol::TOUCH_DOWN => {
                let (window, id, x, y) = (f.u32()?, f.u32()?, f.f32()?, f.f32()?);
                self.touch_down(window, id, x, y);
            }
            protocol::TOUCH_UP => {
                let (window, id) = (f.u32()?, f.u32()?);
                self.touch_up(window, id);
            }
            protocol::TOUCH_MOTION => {
                let (window, id, x, y) = (f.u32()?, f.u32()?, f.f32()?, f.f32()?);
                self.touch_motion(window, id, x, y);
            }
            protocol::TOUCH_CANCEL => {
                let window = f.u32()?;
                self.touch_cancel(window);
            }
            protocol::TOUCH_FRAME => {
                let _window = f.u32()?;
                self.touch_frame();
            }
            _ => {}
        }
        Ok(())
    }

    fn hello(&mut self, id: u64, hello: HelloMsg) -> Result<()> {
        let Some(version) = protocol::negotiate(hello.min_version, hello.version) else {
            bail!(
                "no common GUI protocol version: app speaks {}..{}, compositor {}..{}",
                hello.min_version,
                hello.version,
                protocol::MIN_VERSION,
                protocol::VERSION
            );
        };
        if let Some(old) = self.gui.active.filter(|old| *old != id) {
            info!("a new app connection replaces the old one");
            self.close_conn(old, true, true);
        }
        let generation = self.gui.next_gen;
        self.gui.next_gen += 1;
        let instance = self.gui.instance.clone();
        let Some(conn) = self.gui.conns.get_mut(&id) else { return Ok(()) };
        conn.ready = true;
        conn.hashes = hello.hashes;
        conn.send(protocol::json_frame(&json!({
            "type": "hello",
            "version": version,
            "compositor": concat!("pocketnix-compositor ", env!("CARGO_PKG_VERSION")),
            "instance": instance,
        })));
        self.gui.active = Some(id);
        self.gui.generation = generation;
        info!("app connected");

        // Callbacks sent on the old connection wait for the replay instead.
        for (window, held) in std::mem::take(&mut self.held) {
            let Some(toplevel) = self.toplevel_by_id(window) else {
                let time = self.time_ms();
                held.into_iter().for_each(|(_, cb)| cb.done(time));
                continue;
            };
            let root = toplevel.wl_surface().clone();
            compositor::with_states(&root, |data| {
                crate::surface::with_surf(data, |s| s.callbacks.extend(held.into_iter().map(|(_, cb)| cb)));
            });
            if !self.cb_surfaces.contains(&root) {
                self.cb_surfaces.push(root);
            }
        }

        // The replay names each toplevel's entry, so the list comes first.
        self.scan_apps();
        self.reset_text_input();
        self.flush();
        let (window, name) = self.cursor.clone();
        self.send_json(json!({"type": "cursor", "window": window, "name": name}));
        self.ti.hold = false;
        self.sync_text_input();
        self.send_apps();
        self.send_json(json!({"type": "ready"}));
        Ok(())
    }

    fn on_json(&mut self, conn: u64, msg: Value) -> Result<()> {
        let ty = msg.get("type").and_then(Value::as_str).unwrap_or_default().to_owned();
        match ty.as_str() {
            "output" => {
                let m: OutputMsg = serde_json::from_value(msg).context("malformed output")?;
                if m.width <= 0 || m.height <= 0 || !(m.scale > 0.0) {
                    bail!("output needs a size and a scale");
                }
                self.apply_output(OutputCfg {
                    width: m.width,
                    height: m.height,
                    scale: m.scale,
                    refresh: if m.refresh > 0 { m.refresh } else { 60_000 },
                });
            }
            "window" => {
                let m: WindowMsg = serde_json::from_value(msg).context("malformed window")?;
                self.window_msg(m);
            }
            "close" => {
                let m: CloseMsg = serde_json::from_value(msg).context("malformed close")?;
                if let Some(toplevel) = self.toplevel_by_id(m.id) {
                    toplevel.send_close();
                }
            }
            "settings" => {
                let m: SettingsMsg = serde_json::from_value(msg).context("malformed settings")?;
                let valid = |f: f64| f.is_finite() && f > 0.0;
                if !valid(m.size) || m.apps.values().any(|a| !valid(a.size)) {
                    bail!("settings: size factors must be positive");
                }
                self.settings = Settings {
                    size: m.size,
                    apps: m.apps.into_iter().map(|(id, a)| (id, a.size)).collect(),
                };
            }
            "keyboard-layout" => {
                self.set_keyboard_layout(serde_json::from_value(msg).context("malformed keyboard-layout")?);
            }
            "text" => self.type_text(serde_json::from_value(msg).context("malformed text")?),
            "ime" => self.on_ime(serde_json::from_value(msg).context("malformed ime")?),
            "move" => self.on_move(serde_json::from_value(msg).context("malformed move")?),
            "icon" => self.on_icon(conn, serde_json::from_value(msg).context("malformed icon")?),
            "launch" => self.on_launch(serde_json::from_value(msg).context("malformed launch")?)?,
            "launch-cancel" => self.on_launch_cancel(serde_json::from_value(msg).context("malformed launch-cancel")?),
            _ => {}
        }
        Ok(())
    }

    fn window_msg(&mut self, m: WindowMsg) {
        let Some(toplevel) = self.toplevel_by_id(m.id) else { return };
        if self.is_dialog(m.id) {
            return;
        }
        let old_scale = self.window_scale(m.id);
        let scale = if m.scale > 0.0 { m.scale } else { old_scale };
        if scale != old_scale {
            // Placed for the old logical size.
            self.dismiss_popups(m.id);
        }
        let cfg = WindowCfg {
            width: m.width,
            height: m.height,
            scale,
            focused: m.focused,
            visible: m.visible,
            maximized: m.maximized,
            fullscreen: m.fullscreen,
        };
        self.windows.insert(m.id, cfg);
        let has_focus = self.focus.get(&m.id).is_none_or(|f| *f == m.id);
        toplevel.with_pending_state(|s| apply_window_cfg(s, &cfg, has_focus));
        if toplevel.is_initial_configure_sent() {
            toplevel.send_pending_configure();
        }
        let keyboard_window = self.keyboard_window();
        if cfg.focused && keyboard_window != Some(m.id) {
            let target = self.focus_target(m.id);
            self.set_keyboard_focus(target);
        } else if !cfg.focused && keyboard_window == Some(m.id) {
            self.set_keyboard_focus(None);
        }
    }

    /// Closes every popup drawn in `window`, innermost first.
    fn dismiss_popups(&mut self, window: u32) {
        let popups: Vec<_> = self
            .xdg_shell_state
            .popup_surfaces()
            .iter()
            .filter(|p| p.alive() && self.window_of(p.wl_surface()) == Some(window))
            .cloned()
            .collect();
        if popups.is_empty() {
            return;
        }
        for popup in popups.iter().rev() {
            popup.send_popup_done();
        }
        self.grabs.retain(|g| !popups.contains(g));
        if self.keyboard_window() == Some(window) {
            let target = self.focus_target(window);
            self.set_keyboard_focus(target);
        }
    }

    fn presented(&mut self, window: u32, seq: u32) {
        let time = self.time_ms();
        if let Some(held) = self.held.get_mut(&window) {
            held.retain(|(s, cb)| {
                if *s <= seq {
                    cb.done(time);
                    false
                } else {
                    true
                }
            });
        }
    }

    /// The topmost surface of `window` that takes input at `p` (window
    /// logical coordinates), with its origin.
    fn hit_test(&self, window: u32, p: Point<f64, Logical>) -> Option<(WlSurface, Point<f64, Logical>)> {
        self.hit_layer(window, p).map(|(s, o, _)| (s, o))
    }

    /// `hit_test`'s surface and origin, and the toplevel it belongs to.
    pub fn hit_layer(&self, window: u32, p: Point<f64, Logical>) -> Option<(WlSurface, Point<f64, Logical>, u32)> {
        let layers = self.layouts.get(&window)?;
        layers.iter().rev().find_map(|l| {
            if !l.mapped || !l.surface.is_alive() {
                return None;
            }
            let local = p - l.loc.to_f64();
            if local.x < 0.0 || local.y < 0.0 || local.x >= l.size.w as f64 || local.y >= l.size.h as f64 {
                return None;
            }
            if let Some(region) = &l.input
                && !region.contains((local.x.floor() as i32, local.y.floor() as i32))
            {
                return None;
            }
            Some((l.surface.clone(), l.loc.to_f64(), l.toplevel))
        })
    }

    fn pointer_motion(&mut self, window: u32, x: f32, y: f32) {
        let scale = self.window_scale(window);
        let p: Point<f64, Logical> = (x as f64 / scale, y as f64 / scale).into();
        if let Some(drag) = &self.drag
            && drag.window == window
            && drag.touch.is_none()
        {
            let offset = drag.start_offset + (p - drag.start).to_i32_round();
            self.dialogs.entry(drag.dialog).or_default().offset = Some(offset);
        }
        let focus = self.hit_test(window, p);
        self.pointer_window = Some(window);
        let Some(pointer) = self.seat.get_pointer() else { return };
        let event = MotionEvent {
            location: p,
            serial: SERIAL_COUNTER.next_serial(),
            time: self.time_ms(),
        };
        pointer.motion(self, focus, &event);
    }

    fn pointer_frame(&mut self) {
        if let Some(pointer) = self.seat.get_pointer() {
            pointer.frame(self);
        }
    }

    /// Whether `surface` draws in a popup that holds a grab.
    pub fn in_grab(&self, surface: &WlSurface) -> bool {
        let mut s = surface.clone();
        while let Some(parent) = compositor::get_parent(&s) {
            s = parent;
        }
        self.grabs.iter().any(|p| p.wl_surface() == &s)
    }

    fn pointer_button(&mut self, window: u32, x: f32, y: f32, button: u32, pressed: bool) {
        self.pointer_motion(window, x, y);
        let Some(pointer) = self.seat.get_pointer() else { return };
        let scale = self.window_scale(window);
        let hit = self.hit_layer(window, (x as f64 / scale, y as f64 / scale).into()).map(|(_, _, t)| t);
        if pressed && hit.is_some_and(|t| self.blocked_by_modal(t)) {
            // A modal dialog's parent takes no presses.
            self.swallowed_buttons.insert(button);
            pointer.frame(self);
            return;
        }
        if !pressed && self.drag.as_ref().is_some_and(|d| d.touch.is_none()) {
            self.drag = None;
        }
        if pressed {
            self.grabs.retain(|p| p.alive());
            let target = pointer.current_focus();
            if !self.grabs.is_empty() && !target.as_ref().is_some_and(|t| self.in_grab(t)) {
                // A press outside a grabbing popup dismisses it, and goes no further.
                self.dismiss_grabs(window);
                self.swallowed_buttons.insert(button);
                pointer.frame(self);
                return;
            }
            self.pressed_buttons.insert(button);
            if let Some(toplevel) = hit {
                self.pressed_on(window, toplevel);
            }
        } else {
            if self.swallowed_buttons.remove(&button) {
                pointer.frame(self);
                return;
            }
            self.pressed_buttons.remove(&button);
        }
        let serial = SERIAL_COUNTER.next_serial();
        let event = ButtonEvent {
            serial,
            time: self.time_ms(),
            button,
            state: if pressed { ButtonState::Pressed } else { ButtonState::Released },
        };
        pointer.button(self, &event);
        pointer.frame(self);
        // A press is an input event a client may mint a token with, if it has the keyboard too.
        let keyboard = self.seat.get_keyboard().and_then(|k| k.current_focus());
        let client = pointer.current_focus().and_then(|s| s.client());
        if pressed
            && let Some(client) = client
            && keyboard.and_then(|k| k.client()).is_some_and(|k| k.id() == client.id())
        {
            self.input_serials.push(serial, client.id());
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn pointer_axis(&mut self, window: u32, x: f32, y: f32, source: u8, stop: u8, d: (f32, f32), v120: (i32, i32)) {
        self.pointer_motion(window, x, y);
        let Some(pointer) = self.seat.get_pointer() else { return };
        let scale = self.window_scale(window);
        let hit = self.hit_layer(window, (x as f64 / scale, y as f64 / scale).into()).map(|(_, _, t)| t);
        if hit.is_some_and(|t| self.blocked_by_modal(t)) {
            pointer.frame(self);
            return;
        }
        let source = match source {
            1 => AxisSource::Finger,
            2 => AxisSource::Continuous,
            3 => AxisSource::WheelTilt,
            _ => AxisSource::Wheel,
        };
        let mut frame = AxisFrame::new(self.time_ms()).source(source);
        for (axis, delta, steps, stop_bit) in [
            (Axis::Horizontal, d.0, v120.0, 1),
            (Axis::Vertical, d.1, v120.1, 2),
        ] {
            if delta != 0.0 {
                frame = frame.value(axis, delta as f64 / scale);
            }
            if steps != 0 {
                frame = frame.v120(axis, steps);
            }
            if stop & stop_bit != 0 {
                frame = frame.stop(axis);
            }
        }
        pointer.axis(self, frame);
        pointer.frame(self);
    }

    /// Dismisses the grabbing popups, handing the keyboard back to
    /// `window`'s focused toplevel if it had it.
    pub fn dismiss_grabs(&mut self, window: u32) {
        for popup in std::mem::take(&mut self.grabs).into_iter().rev() {
            popup.send_popup_done();
        }
        if self.keyboard_window() == Some(window) {
            let target = self.focus_target(window);
            self.set_keyboard_focus(target);
        }
    }

    fn pointer_leave(&mut self, window: u32) {
        let Some(pointer) = self.seat.get_pointer() else { return };
        if !self.dnd_active {
            let time = self.time_ms();
            for button in std::mem::take(&mut self.pressed_buttons) {
                let event = ButtonEvent {
                    serial: SERIAL_COUNTER.next_serial(),
                    time,
                    button,
                    state: ButtonState::Released,
                };
                pointer.button(self, &event);
            }
        }
        let event = MotionEvent {
            location: pointer.current_location(),
            serial: SERIAL_COUNTER.next_serial(),
            time: self.time_ms(),
        };
        pointer.motion(self, None, &event);
        pointer.frame(self);
        if self.pointer_window == Some(window) {
            self.pointer_window = None;
        }
    }

    pub fn focus_window(&mut self, window: u32) {
        if self.keyboard_window() != Some(window) {
            let target = self.focus_target(window);
            if target.is_some() {
                self.set_keyboard_focus(target);
            }
        }
    }

    fn key(&mut self, window: u32, key: u32, pressed: bool) {
        self.focus_window(window);
        let Some(keyboard) = self.seat.get_keyboard() else { return };
        if pressed {
            self.pressed_keys.insert(key);
        } else if !self.pressed_keys.remove(&key) {
            return;
        }
        let state = if pressed { KeyState::Pressed } else { KeyState::Released };
        let time = self.time_ms();
        let serial = SERIAL_COUNTER.next_serial();
        keyboard.input::<(), _>(self, (key + 8).into(), state, serial, time, |_, _, _| FilterResult::Forward);
        if let Some(client) = keyboard.current_focus().and_then(|f| f.client()) {
            self.input_serials.push(serial, client.id());
        }
    }

    /// Android's metaState as is: locks are toggled with their keys, held
    /// modifiers set on the XKB state directly, since the app may send them
    /// without any key event.
    fn modifiers(&mut self, window: u32, mods: u8) {
        const KEY_CAPSLOCK: u32 = 58;
        const KEY_NUMLOCK: u32 = 69;
        self.focus_window(window);
        let Some(keyboard) = self.seat.get_keyboard() else { return };
        let current = keyboard.modifier_state();
        for (want, have, key) in [
            (mods & 1 << 4 != 0, current.caps_lock, KEY_CAPSLOCK),
            (mods & 1 << 5 != 0, current.num_lock, KEY_NUMLOCK),
        ] {
            if want != have {
                for state in [KeyState::Pressed, KeyState::Released] {
                    let time = self.time_ms();
                    keyboard.input::<(), _>(self, (key + 8).into(), state, SERIAL_COUNTER.next_serial(), time, |_, _, _| {
                        FilterResult::Forward
                    });
                }
            }
        }
        let mut m = keyboard.modifier_state();
        let want = (mods & 1 != 0, mods & 1 << 1 != 0, mods & 1 << 2 != 0, mods & 1 << 3 != 0);
        if (m.shift, m.ctrl, m.alt, m.logo) != want {
            (m.shift, m.ctrl, m.alt, m.logo) = want;
            if keyboard.set_modifier_state(m) != 0
                && let Some(focus) = keyboard.current_focus()
            {
                let seat = self.seat.clone();
                focus.modifiers(&seat, self, keyboard.modifier_state(), SERIAL_COUNTER.next_serial());
            }
        }
    }
}
