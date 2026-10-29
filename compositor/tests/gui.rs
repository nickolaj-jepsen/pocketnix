//! Drives the compositor binary through its sockets: Wayland clients draw,
//! and a GUI client decodes what the app would get. The decoder here follows
//! docs/gui-protocol.md on its own, so it checks the compositor against the
//! spec rather than against itself.

use std::collections::HashMap;
use std::io::BufRead;
use std::io::BufReader;
use std::io::Read;
use std::io::Write;
use std::os::fd::AsFd;
use std::os::fd::FromRawFd;
use std::os::fd::OwnedFd;
use std::os::unix::fs::FileExt;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::path::PathBuf;
use std::process::Child;
use std::process::Command;
use std::process::Stdio;
use std::sync::Arc;
use std::sync::Mutex;
use std::time::Duration;
use std::time::Instant;

use serde_json::Value;
use serde_json::json;
use wayland_client::Connection;
use wayland_client::Dispatch;
use wayland_client::EventQueue;
use wayland_client::QueueHandle;
use wayland_client::WEnum;
use wayland_client::globals::GlobalListContents;
use wayland_client::globals::registry_queue_init;
use wayland_client::protocol::wl_buffer::WlBuffer;
use wayland_client::protocol::wl_callback;
use wayland_client::protocol::wl_callback::WlCallback;
use wayland_client::protocol::wl_compositor::WlCompositor;
use wayland_client::protocol::wl_keyboard;
use wayland_client::protocol::wl_keyboard::WlKeyboard;
use wayland_client::protocol::wl_pointer;
use wayland_client::protocol::wl_pointer::WlPointer;
use wayland_client::protocol::wl_registry::WlRegistry;
use wayland_client::protocol::wl_seat;
use wayland_client::protocol::wl_seat::WlSeat;
use wayland_client::protocol::wl_shm;
use wayland_client::protocol::wl_shm::WlShm;
use wayland_client::protocol::wl_shm_pool::WlShmPool;
use wayland_client::protocol::wl_surface::WlSurface;
use wayland_client::protocol::wl_touch;
use wayland_client::protocol::wl_touch::WlTouch;
use wayland_protocols::xdg::activation::v1::client::xdg_activation_v1::XdgActivationV1;
use wayland_protocols::xdg::dialog::v1::client::xdg_dialog_v1::XdgDialogV1;
use wayland_protocols::xdg::dialog::v1::client::xdg_wm_dialog_v1::XdgWmDialogV1;
use wayland_protocols::xdg::shell::client::xdg_popup;
use wayland_protocols::xdg::shell::client::xdg_popup::XdgPopup;
use wayland_protocols::xdg::shell::client::xdg_positioner;
use wayland_protocols::xdg::shell::client::xdg_positioner::XdgPositioner;
use wayland_protocols::xdg::shell::client::xdg_surface;
use wayland_protocols::xdg::shell::client::xdg_surface::XdgSurface;
use wayland_protocols::xdg::shell::client::xdg_toplevel;
use wayland_protocols::xdg::shell::client::xdg_toplevel::XdgToplevel;
use wayland_protocols::xdg::shell::client::xdg_wm_base;
use wayland_protocols::xdg::shell::client::xdg_wm_base::XdgWmBase;
use wayland_protocols::wp::text_input::zv3::client::zwp_text_input_manager_v3::ZwpTextInputManagerV3;
use wayland_protocols::wp::text_input::zv3::client::zwp_text_input_v3;
use wayland_protocols::wp::text_input::zv3::client::zwp_text_input_v3::ZwpTextInputV3;
use xkbcommon::xkb;

const TIMEOUT: Duration = Duration::from_secs(30);

const FLAG_BUFFER: u16 = 1;
const FLAG_FULL: u16 = 2;
const FLAG_SCROLL: u16 = 4;
const FLAG_HASH: u16 = 8;
const FLAG_GROUPED: u16 = 16;
const FLAG_HIDDEN: u16 = 32;

const BTN_LEFT: u32 = 0x110;
const KEY_A: u32 = 30;

fn wait_for(what: &str, mut f: impl FnMut() -> bool) {
    let deadline = Instant::now() + TIMEOUT;
    while !f() {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        std::thread::sleep(Duration::from_millis(10));
    }
}

struct Compositor {
    child: Child,
    dir: PathBuf,
    log: Arc<Mutex<Vec<String>>>,
}

/// A fresh directory for one test's files.
fn test_dir(name: &str) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("pnc-{}-{name}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o700)).unwrap();
    dir
}

impl Compositor {
    fn start(name: &str) -> Self {
        Self::start_with(name, |_| {})
    }

    /// Starts a compositor whose environment `env` adjusts.
    fn start_with(name: &str, env: impl FnOnce(&mut Command)) -> Self {
        let dir = test_dir(name);
        let mut cmd = Command::new(env!("CARGO_BIN_EXE_pocketnix-compositor"));
        cmd.arg("--gui-socket")
            .arg(dir.join("gui.sock"))
            .args(["--wayland-display", "wl-test", "--log-hashes"])
            .env("XDG_RUNTIME_DIR", &dir)
            .env("RUST_LOG", "info")
            .env_remove("POCKETNIX_TAB")
            .stderr(Stdio::piped());
        env(&mut cmd);
        let mut child = cmd.spawn().unwrap();
        let log = Arc::new(Mutex::new(Vec::new()));
        let stderr = child.stderr.take().unwrap();
        let lines = log.clone();
        std::thread::spawn(move || {
            for line in BufReader::new(stderr).lines().map_while(Result::ok) {
                eprintln!("compositor: {line}");
                lines.lock().unwrap().push(line);
            }
        });
        let c = Compositor { child, dir, log };
        wait_for("the compositor's sockets", || {
            c.dir.join("gui.sock").exists() && c.dir.join("wl-test").exists()
        });
        c
    }

    fn gui_socket(&self) -> PathBuf {
        self.dir.join("gui.sock")
    }

    fn wayland_socket(&self) -> PathBuf {
        self.dir.join("wl-test")
    }

    /// The `--log-hashes` line for a commit, waiting for it to arrive.
    fn logged_hash(&self, surface: u32, seq: u32) -> String {
        let prefix = format!("hash surface={surface} seq={seq} ");
        let mut found = None;
        wait_for("the compositor's hash line", || {
            found = self.log.lock().unwrap().iter().find_map(|l| l.strip_prefix(&prefix).map(str::to_owned));
            found.is_some()
        });
        found.unwrap()
    }
}

impl Drop for Compositor {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

/// The spec's content hash.
fn content_hash(bytes: &[u8]) -> u64 {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    let mut chunks = bytes.chunks_exact(8);
    for c in &mut chunks {
        h = (h ^ u64::from_le_bytes(c.try_into().unwrap()))
            .wrapping_mul(0x0100_0000_01b3)
            .rotate_left(29);
    }
    for b in chunks.remainder() {
        h = (h ^ *b as u64).wrapping_mul(0x0100_0000_01b3);
    }
    h
}

#[derive(Clone, Debug)]
struct Commit {
    flags: u16,
    surface: u32,
    window: u32,
    seq: u32,
    x: i32,
    y: i32,
    dst_width: u32,
    dst_height: u32,
    width: u32,
    height: u32,
    scroll: i32,
    rects: Vec<[u32; 4]>,
    hash: u64,
}

#[derive(Debug)]
enum Msg {
    Json(Value),
    Commit(Commit),
    /// An ICON frame, kept in `App::icons`.
    Icon,
}

/// A surface's reference copy, as the app keeps it.
struct Layer {
    width: u32,
    height: u32,
    pixels: Vec<u8>,
}

/// The app's side of a GUI connection.
struct App {
    stream: UnixStream,
    layers: HashMap<u32, Layer>,
    auto_ack: bool,
    hashed: usize,
    /// ICON frames: size, key and PNG.
    icons: Vec<(u16, String, Vec<u8>)>,
}

fn be32(b: &[u8], at: usize) -> u32 {
    u32::from_be_bytes(b[at..at + 4].try_into().unwrap())
}

impl App {
    fn connect(compositor: &Compositor) -> Self {
        let stream = UnixStream::connect(compositor.gui_socket()).unwrap();
        stream.set_read_timeout(Some(TIMEOUT)).unwrap();
        let mut app = App {
            stream,
            layers: HashMap::new(),
            auto_ack: false,
            hashed: 0,
            icons: Vec::new(),
        };
        app.send_json(json!({"type": "hello", "version": 2, "minVersion": 2, "app": "test", "hashes": true}));
        app
    }

    fn send_frame(&mut self, ty: u8, payload: &[u8]) {
        let mut f = vec![ty];
        f.extend_from_slice(&(payload.len() as u32).to_be_bytes());
        f.extend_from_slice(payload);
        self.stream.write_all(&f).unwrap();
    }

    fn send_json(&mut self, v: Value) {
        self.send_frame(0, &serde_json::to_vec(&v).unwrap());
    }

    fn send_binary(&mut self, ty: u8, fields: &[&[u8]]) {
        self.send_frame(ty, &fields.concat());
    }

    fn presented(&mut self, window: u32, seq: u32) {
        self.send_binary(0x10, &[&window.to_be_bytes(), &seq.to_be_bytes()]);
    }

    fn read_frame(&mut self) -> Option<(u8, Vec<u8>)> {
        let mut head = [0u8; 5];
        match self.stream.read_exact(&mut head) {
            Ok(()) => {}
            Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => return None,
            Err(e) => panic!("reading the GUI socket: {e}"),
        }
        let len = be32(&head, 1) as usize;
        assert!(len <= 1 << 20, "frame of {len} bytes");
        let mut payload = vec![0; len];
        self.stream.read_exact(&mut payload).unwrap();
        Some((head[0], payload))
    }

    fn next(&mut self) -> Msg {
        loop {
            let (ty, p) = self.read_frame().expect("the compositor closed the connection");
            match ty {
                0x00 => return Msg::Json(serde_json::from_slice(&p).unwrap()),
                0x01 => return Msg::Commit(self.commit(p)),
                0x02 => panic!("COMMIT_DATA without a COMMIT"),
                0x03 => {
                    let size = u16::from_be_bytes([p[0], p[1]]);
                    let key_len = p[2] as usize;
                    let key = String::from_utf8(p[3..3 + key_len].to_vec()).unwrap();
                    self.icons.push((size, key, p[3 + key_len..].to_vec()));
                    return Msg::Icon;
                }
                _ => {}
            }
        }
    }

    fn commit(&mut self, p: Vec<u8>) -> Commit {
        let header = u16::from_be_bytes([p[0], p[1]]) as usize;
        let rect_count = be32(&p, 68) as usize;
        let data_len = be32(&p, 72) as usize;
        let c = Commit {
            flags: u16::from_be_bytes([p[2], p[3]]),
            surface: be32(&p, 4),
            window: be32(&p, 8),
            seq: be32(&p, 12),
            x: be32(&p, 24) as i32,
            y: be32(&p, 28) as i32,
            dst_width: be32(&p, 32),
            dst_height: be32(&p, 36),
            width: be32(&p, 40),
            height: be32(&p, 44),
            scroll: be32(&p, 64) as i32,
            rects: (0..rect_count)
                .map(|i| {
                    let at = header + 16 * i;
                    [be32(&p, at), be32(&p, at + 4), be32(&p, at + 8), be32(&p, at + 12)]
                })
                .collect(),
            hash: u64::from_be_bytes(p[80..88].try_into().unwrap()),
        };
        let encoding = p[19];
        let mut raw = p[header + 16 * rect_count..].to_vec();
        while raw.len() < data_len {
            let (ty, more) = self.read_frame().expect("EOF inside a commit");
            assert_eq!(ty, 0x02, "a commit's frames must be consecutive");
            assert_eq!(be32(&more, 0), c.seq);
            raw.extend_from_slice(&more[4..]);
        }
        assert_eq!(raw.len(), data_len);

        if c.flags & FLAG_HIDDEN != 0 {
            self.layers.remove(&c.surface);
        }
        if c.flags & FLAG_BUFFER != 0 {
            let data = if encoding == 1 { zstd::stream::decode_all(&raw[..]).unwrap() } else { raw };
            let expected: u32 = c.rects.iter().map(|r| r[2] * r[3] * 4).sum();
            assert_eq!(data.len(), expected as usize, "decoded size of seq {}", c.seq);
            if c.flags & FLAG_FULL != 0 {
                assert_eq!(c.rects, vec![[0, 0, c.width, c.height]]);
                self.layers.insert(
                    c.surface,
                    Layer {
                        width: c.width,
                        height: c.height,
                        pixels: data,
                    },
                );
            } else {
                let layer = self.layers.get_mut(&c.surface).expect("a patch for a surface without content");
                assert_eq!((layer.width, layer.height), (c.width, c.height));
                let row = layer.width as usize * 4;
                if c.flags & FLAG_SCROLL != 0 {
                    let old = layer.pixels.clone();
                    for y in 0..layer.height as i64 {
                        let sy = y + c.scroll as i64;
                        if (0..layer.height as i64).contains(&sy) {
                            layer.pixels[y as usize * row..(y as usize + 1) * row]
                                .copy_from_slice(&old[sy as usize * row..(sy as usize + 1) * row]);
                        }
                    }
                }
                let mut at = 0;
                for [x, y, w, h] in &c.rects {
                    for yy in *y..y + h {
                        let start = yy as usize * row + *x as usize * 4;
                        let n = *w as usize * 4;
                        layer.pixels[start..start + n].copy_from_slice(&data[at..at + n]);
                        at += n;
                    }
                }
            }
            if c.flags & FLAG_HASH != 0 {
                let layer = &self.layers[&c.surface];
                assert_eq!(content_hash(&layer.pixels), c.hash, "content hash of seq {}", c.seq);
                self.hashed += 1;
            }
        }
        if self.auto_ack && c.flags & FLAG_GROUPED == 0 {
            self.presented(c.window, c.seq);
        }
        c
    }

    fn wait<T>(&mut self, what: &str, mut f: impl FnMut(&Msg) -> Option<T>) -> T {
        let deadline = Instant::now() + TIMEOUT;
        loop {
            assert!(Instant::now() < deadline, "timed out waiting for {what}");
            let msg = self.next();
            if let Some(t) = f(&msg) {
                return t;
            }
        }
    }

    /// The next JSON message of type `ty` that `f` accepts.
    fn wait_json_where(&mut self, ty: &str, f: impl Fn(&Value) -> bool) -> Value {
        self.wait(ty, |m| match m {
            Msg::Json(v) if v["type"] == ty && f(v) => Some(v.clone()),
            _ => None,
        })
    }

    /// The PNG answering an `icon` request for `key` at `size`.
    fn icon(&mut self, key: &str, size: u16) -> Vec<u8> {
        self.send_json(json!({"type": "icon", "icon": key, "size": size}));
        let deadline = Instant::now() + TIMEOUT;
        loop {
            if let Some(i) = self.icons.iter().position(|(s, k, _)| *s == size && k == key) {
                return self.icons.remove(i).2;
            }
            assert!(Instant::now() < deadline, "timed out waiting for icon {key}");
            let _ = self.next();
        }
    }

    /// Returns once the compositor has handled every message sent before:
    /// it handles them in order, and answers an icon request.
    fn sync(&mut self) {
        self.icon("sync", 16);
    }

    fn wait_json(&mut self, ty: &str) -> Value {
        self.wait(ty, |m| match m {
            Msg::Json(v) if v["type"] == ty => Some(v.clone()),
            _ => None,
        })
    }

    fn wait_commit(&mut self, what: &str, f: impl Fn(&Commit) -> bool) -> Commit {
        self.wait(what, |m| match m {
            Msg::Commit(c) if f(c) => Some(c.clone()),
            _ => None,
        })
    }
}

#[derive(Default)]
struct Wl {
    configure: Option<u32>,
    bounds: Option<(i32, i32)>,
    dialog_configure: Option<u32>,
    dialog_bounds: Option<(i32, i32)>,
    /// The surface the pointer last entered and the one with keyboard focus.
    pointer_surface: Option<WlSurface>,
    keyboard_surface: Option<WlSurface>,
    popup_configure: Option<u32>,
    /// The popup's last configure: x, y, width, height.
    popup_geometry: Option<(i32, i32, i32, i32)>,
    size: (i32, i32),
    states: Vec<u32>,
    frames: u32,
    pointer: Vec<String>,
    keys: Vec<(u32, u32)>,
    mods_depressed: u32,
    mods_locked: u32,
    group: u32,
    keyboard_focus: bool,
    /// Every keymap the keyboard was sent, in order.
    keymaps: Vec<String>,
    /// Key presses, with what was in effect: key, keymap index, depressed
    /// and locked modifiers, group.
    presses: Vec<(u32, usize, u32, u32, u32)>,
    has_touch: bool,
    /// wl_touch events: "down <id> <x> <y>", "motion …", "up <id>", "frame", "cancel".
    touch: Vec<String>,
    /// The surface each `down` went to, in order.
    touch_surfaces: Vec<WlSurface>,
    popup_done: bool,
    /// The surface the text input last entered, and its events.
    ti_entered: Option<WlSurface>,
    ti: Vec<String>,
}

/// A Wayland client drawing known pixels with known damage.
struct Pattern {
    conn: Connection,
    queue: EventQueue<Wl>,
    state: Wl,
    qh: QueueHandle<Wl>,
    globals: Vec<String>,
    registry: wayland_client::globals::GlobalList,
    compositor: WlCompositor,
    shm: WlShm,
    wm_base: XdgWmBase,
    surface: WlSurface,
    xdg_surface: XdgSurface,
    toplevel: XdgToplevel,
    file: std::fs::File,
    buffers: Vec<WlBuffer>,
    next: usize,
    width: i32,
    height: i32,
    seat: WlSeat,
    /// The last popup `popup_from` opened.
    popup_surface: Option<WlSurface>,
}

impl Pattern {
    fn connect(compositor: &Compositor, width: i32, height: i32) -> Self {
        let conn = Connection::from_socket(UnixStream::connect(compositor.wayland_socket()).unwrap()).unwrap();
        let (globals, queue) = registry_queue_init::<Wl>(&conn).unwrap();
        let qh = queue.handle();
        let compositor: WlCompositor = globals.bind(&qh, 4..=6, ()).unwrap();
        let shm: WlShm = globals.bind(&qh, 1..=1, ()).unwrap();
        let wm_base: XdgWmBase = globals.bind(&qh, 1..=6, ()).unwrap();
        let seat: WlSeat = globals.bind(&qh, 1..=7, ()).unwrap();
        let names = globals.contents().with_list(|list| list.iter().map(|g| g.interface.clone()).collect());

        // SAFETY: memfd_create with a valid C string.
        let fd = unsafe { libc::memfd_create(c"pattern".as_ptr(), libc::MFD_CLOEXEC) };
        assert!(fd >= 0);
        // SAFETY: a fresh descriptor we own.
        let file = std::fs::File::from(unsafe { OwnedFd::from_raw_fd(fd) });
        let size = (width * height * 4) as u64;
        file.set_len(2 * size).unwrap();
        let pool: WlShmPool = shm.create_pool(file.as_fd(), 2 * size as i32, &qh, ());
        let buffers = (0..2)
            .map(|i| pool.create_buffer(i * size as i32, width, height, width * 4, wl_shm::Format::Argb8888, &qh, ()))
            .collect();

        let surface = compositor.create_surface(&qh, ());
        let xdg_surface = wm_base.get_xdg_surface(&surface, &qh, ());
        let toplevel = xdg_surface.get_toplevel(&qh, ());
        toplevel.set_title("pattern".into());
        let main = toplevel.clone();
        toplevel.set_app_id("test.pattern".into());
        surface.commit();
        let mut p = Pattern {
            conn,
            queue,
            state: Wl::default(),
            qh,
            globals: names,
            registry: globals,
            compositor,
            shm,
            wm_base,
            surface,
            xdg_surface,
            toplevel: main,
            file,
            buffers,
            next: 0,
            width,
            height,
            seat,
            popup_surface: None,
        };
        p.dispatch_until("the initial configure", |s| s.configure.is_some());
        p
    }

    fn dispatch_until(&mut self, what: &str, f: impl Fn(&Wl) -> bool) {
        let deadline = Instant::now() + TIMEOUT;
        while !f(&self.state) {
            assert!(Instant::now() < deadline, "timed out waiting for {what}");
            self.queue.roundtrip(&mut self.state).unwrap();
            std::thread::sleep(Duration::from_millis(5));
        }
    }

    fn ack(&mut self) {
        if let Some(serial) = self.state.configure.take() {
            self.xdg_surface.ack_configure(serial);
        }
    }

    /// Attaches `pixels` (ARGB8888 rows) with buffer damage `damage` and
    /// commits, optionally asking for a frame callback.
    fn draw(&mut self, pixels: &[u8], damage: [i32; 4], frame: bool) {
        self.ack();
        let size = (self.width * self.height * 4) as u64;
        self.file.write_all_at(pixels, self.next as u64 * size).unwrap();
        self.surface.attach(Some(&self.buffers[self.next]), 0, 0);
        self.next = 1 - self.next;
        self.surface.damage_buffer(damage[0], damage[1], damage[2], damage[3]);
        if frame {
            self.surface.frame(&self.qh, ());
        }
        self.surface.commit();
        self.conn.flush().unwrap();
    }

    /// Opens a popup on the toplevel that wants `size` below and right of
    /// `anchor`, allowing no constraint adjustment, and draws it at the size
    /// it's configured with. Returns that configure's x, y, width, height.
    fn popup(&mut self, anchor: [i32; 4], size: (i32, i32)) -> (i32, i32, i32, i32) {
        let parent = self.xdg_surface.clone();
        self.popup_from(&parent, anchor, size)
    }

    /// `popup`, opened from `parent`.
    fn popup_from(&mut self, parent: &XdgSurface, anchor: [i32; 4], size: (i32, i32)) -> (i32, i32, i32, i32) {
        let surface = self.compositor.create_surface(&self.qh, ());
        let xdg_surface = self.wm_base.get_xdg_surface(&surface, &self.qh, 1u32);
        let positioner = self.wm_base.create_positioner(&self.qh, ());
        positioner.set_size(size.0, size.1);
        positioner.set_anchor_rect(anchor[0], anchor[1], anchor[2], anchor[3]);
        positioner.set_anchor(xdg_positioner::Anchor::BottomRight);
        positioner.set_gravity(xdg_positioner::Gravity::BottomRight);
        xdg_surface.get_popup(Some(parent), &positioner, &self.qh, ());
        surface.commit();
        self.dispatch_until("the popup's configure", |s| s.popup_configure.is_some());
        xdg_surface.ack_configure(self.state.popup_configure.take().unwrap());
        let geometry = self.state.popup_geometry.unwrap();
        let (w, h) = (geometry.2, geometry.3);

        // SAFETY: memfd_create with a valid C string.
        let fd = unsafe { libc::memfd_create(c"popup".as_ptr(), libc::MFD_CLOEXEC) };
        assert!(fd >= 0);
        // SAFETY: a fresh descriptor we own.
        let file = std::fs::File::from(unsafe { OwnedFd::from_raw_fd(fd) });
        file.write_all_at(&pattern(w, h, 7), 0).unwrap();
        let pool = self.shm.create_pool(file.as_fd(), w * h * 4, &self.qh, ());
        let buffer = pool.create_buffer(0, w, h, w * 4, wl_shm::Format::Argb8888, &self.qh, ());
        surface.attach(Some(&buffer), 0, 0);
        surface.damage_buffer(0, 0, w, h);
        surface.commit();
        self.conn.flush().unwrap();
        self.popup_surface = Some(surface);
        geometry
    }

    fn text_input(&mut self) -> ZwpTextInputV3 {
        let manager: ZwpTextInputManagerV3 = self.registry.bind(&self.qh, 1..=1, ()).unwrap();
        manager.get_text_input(&self.seat, &self.qh, ())
    }

    fn roundtrip(&mut self) {
        self.queue.roundtrip(&mut self.state).unwrap();
    }

    /// A toplevel with this one as its parent, drawn `w` × `h` with
    /// pattern `seed` once configured.
    fn dialog(&mut self, parent: &XdgToplevel, w: i32, h: i32, seed: u32) -> Dialog {
        let surface = self.compositor.create_surface(&self.qh, ());
        let xdg_surface = self.wm_base.get_xdg_surface(&surface, &self.qh, Tag);
        let toplevel = xdg_surface.get_toplevel(&self.qh, Tag);
        toplevel.set_parent(Some(parent));
        toplevel.set_app_id("test.pattern".into());
        surface.commit();
        self.dispatch_until("the dialog's configure", |s| s.dialog_configure.is_some());
        xdg_surface.ack_configure(self.state.dialog_configure.take().unwrap());
        // SAFETY: memfd_create with a valid C string.
        let fd = unsafe { libc::memfd_create(c"dialog".as_ptr(), libc::MFD_CLOEXEC) };
        assert!(fd >= 0);
        // SAFETY: a fresh descriptor we own.
        let file = std::fs::File::from(unsafe { OwnedFd::from_raw_fd(fd) });
        file.write_all_at(&pattern(w, h, seed), 0).unwrap();
        let pool = self.shm.create_pool(file.as_fd(), w * h * 4, &self.qh, ());
        let buffer = pool.create_buffer(0, w, h, w * 4, wl_shm::Format::Argb8888, &self.qh, ());
        surface.attach(Some(&buffer), 0, 0);
        surface.damage_buffer(0, 0, w, h);
        surface.commit();
        self.conn.flush().unwrap();
        Dialog { surface, xdg_surface, toplevel }
    }
}

/// User data marking a dialog's objects.
struct Tag;

struct Dialog {
    surface: WlSurface,
    xdg_surface: XdgSurface,
    toplevel: XdgToplevel,
}

/// The wire's RGBA for ARGB8888 rows.
fn to_rgba(argb: &[u8]) -> Vec<u8> {
    argb.chunks(4).flat_map(|p| [p[2], p[1], p[0], p[3]]).collect()
}

fn pattern(width: i32, height: i32, seed: u32) -> Vec<u8> {
    let mut out = Vec::with_capacity((width * height * 4) as usize);
    for y in 0..height as u32 {
        for x in 0..width as u32 {
            let v = (y + seed).wrapping_mul(2654435761) ^ x.wrapping_mul(40503);
            out.extend_from_slice(&[v as u8, (v >> 8) as u8, (v >> 16) as u8, 0xff]);
        }
    }
    out
}

#[test]
fn a_client_window_reaches_the_app_with_matching_pixels() {
    let compositor = Compositor::start("pattern");
    let mut app = App::connect(&compositor);
    let hello = app.wait_json("hello");
    assert_eq!(hello["version"], 2);
    let instance = hello["instance"].as_str().unwrap().to_owned();
    assert!(!instance.is_empty());
    app.wait_json("ready");
    app.send_json(json!({"type": "output", "width": 800, "height": 600, "scale": 2.0, "refresh": 60000}));

    // The output message must land before the toplevel's first configure.
    app.sync();
    let (w, h) = (200, 100);
    let mut client = Pattern::connect(&compositor, w, h);
    assert_eq!(client.state.size, (400, 300), "the first configure fills the output");

    let mut content = pattern(w, h, 0);
    client.draw(&content, [0, 0, w, h], false);
    let toplevel = app.wait_json("toplevel");
    assert_eq!(toplevel["appId"], "test.pattern");
    assert_eq!(toplevel["title"], "pattern");
    assert_eq!((toplevel["width"].as_i64(), toplevel["height"].as_i64()), (Some(400), Some(200)));
    let window = toplevel["id"].as_u64().unwrap() as u32;
    let surface = app.wait_json("surface");
    assert_eq!(surface["role"], "toplevel");
    assert_eq!(surface["window"], window);
    let full = app.wait_commit("the first commit", |c| c.surface == window);
    assert_eq!(full.flags & (FLAG_BUFFER | FLAG_FULL | FLAG_HASH), FLAG_BUFFER | FLAG_FULL | FLAG_HASH);
    assert_eq!((full.x, full.y, full.dst_width, full.dst_height), (0, 0, 400, 200));
    assert_eq!(app.layers[&window].pixels, to_rgba(&content));
    assert_eq!(compositor.logged_hash(window, full.seq), format!("{w}x{h} {:016x}", full.hash));

    // A small change sends only its rect.
    for y in 20..25 {
        for x in 30..40 {
            content[((y * w + x) * 4) as usize] ^= 0x5a;
        }
    }
    client.draw(&content, [30, 20, 10, 5], false);
    let patch = app.wait_commit("a patch", |c| c.surface == window);
    assert_eq!(patch.flags & (FLAG_BUFFER | FLAG_FULL), FLAG_BUFFER);
    assert_eq!(patch.rects, vec![[30, 20, 10, 5]]);
    assert_eq!(app.layers[&window].pixels, to_rgba(&content));

    // Content moving up by 7 rows comes as a scroll and the new rows.
    let row = (w * 4) as usize;
    let fresh = pattern(w, 7, 1000);
    let mut scrolled = content[7 * row..].to_vec();
    scrolled.extend_from_slice(&fresh);
    client.draw(&scrolled, [0, 0, w, h], false);
    let scroll = app.wait_commit("a scroll", |c| c.surface == window);
    assert_eq!(scroll.flags & (FLAG_BUFFER | FLAG_FULL | FLAG_SCROLL), FLAG_BUFFER | FLAG_SCROLL);
    assert_eq!(scroll.scroll, 7);
    assert_eq!(scroll.rects, vec![[0, (h - 7) as u32, w as u32, 7]]);
    assert_eq!(app.layers[&window].pixels, to_rgba(&scrolled));

    // Frame callbacks wait for the app to present.
    client.draw(&scrolled, [0, 0, 0, 0], true);
    let paced = app.wait_commit("a callback's commit", |c| c.surface == window);
    client.roundtrip();
    std::thread::sleep(Duration::from_millis(100));
    client.roundtrip();
    assert_eq!(client.state.frames, 0, "a frame callback fired before PRESENTED");
    app.presented(window, paced.seq);
    client.dispatch_until("the frame callback", |s| s.frames == 1);

    // Window state: size and focus, then input in window pixels.
    app.send_json(json!({"type": "window", "id": window, "width": 600, "height": 400, "scale": 2.0,
        "focused": true, "visible": true, "maximized": false, "fullscreen": false}));
    client.dispatch_until("the window's configure", |s| s.size == (300, 200) && s.keyboard_focus);
    assert!(client.state.states.contains(&(xdg_toplevel::State::Activated as u32)));
    app.send_binary(0x20, &[&window.to_be_bytes(), &100f32.to_be_bytes(), &50f32.to_be_bytes()]);
    app.send_binary(
        0x21,
        &[&window.to_be_bytes(), &100f32.to_be_bytes(), &50f32.to_be_bytes(), &BTN_LEFT.to_be_bytes(), &[1]],
    );
    client.dispatch_until("pointer events", |s| s.pointer.iter().any(|e| e.starts_with("button")));
    assert_eq!(client.state.pointer[0], "enter 50 25");
    assert!(client.state.pointer.contains(&format!("button {BTN_LEFT} 1")));
    // A scroll's axis events come in a frame without a motion, or GTK never scrolls.
    app.send_binary(
        0x22,
        &[&window.to_be_bytes(), &f(100.0), &f(50.0), &[0, 0], &f(0.0), &f(30.0), &0i32.to_be_bytes(), &120i32.to_be_bytes()],
    );
    client.dispatch_until("a scroll", |s| s.pointer.iter().skip_while(|e| !e.starts_with("axis")).any(|e| e == "frame"));
    let p = &client.state.pointer;
    let axis = p.iter().position(|e| e.starts_with("axis")).unwrap();
    let frame_start = p[..axis].iter().rposition(|e| e == "frame").map_or(0, |i| i + 1);
    assert!(!p[frame_start..axis].iter().any(|e| e.starts_with("motion")), "{p:?}");
    assert_eq!(p[axis], "axis 0 15");
    app.send_binary(0x31, &[&window.to_be_bytes(), &[0b10]]);
    client.dispatch_until("ctrl", |s| s.mods_depressed != 0);
    app.send_binary(0x30, &[&window.to_be_bytes(), &KEY_A.to_be_bytes(), &[1]]);
    app.send_binary(0x30, &[&window.to_be_bytes(), &KEY_A.to_be_bytes(), &[0]]);
    client.dispatch_until("a key", |s| s.keys == vec![(KEY_A, 1), (KEY_A, 0)]);

    // A new connection replaces this one and replays the window whole.
    let last_hash = content_hash(&app.layers[&window].pixels);
    let mut second = App::connect(&compositor);
    assert_eq!(second.wait_json("hello")["instance"], instance.as_str());
    assert_eq!(second.wait_json("toplevel")["id"], window);
    let replay = second.wait_commit("the replay", |c| c.surface == window);
    assert_eq!(replay.flags & (FLAG_BUFFER | FLAG_FULL), FLAG_BUFFER | FLAG_FULL);
    assert_eq!(replay.hash, last_hash);
    second.wait_json("ready");
    let deadline = Instant::now() + TIMEOUT;
    while app.read_frame().is_some() {
        assert!(Instant::now() < deadline, "the old connection stayed open");
    }
}

#[test]
fn popups_stay_inside_their_window_and_data_control_is_offered() {
    let compositor = Compositor::start("popup");
    let mut app = App::connect(&compositor);
    app.auto_ack = true;
    app.wait_json("ready");
    app.send_json(json!({"type": "output", "width": 800, "height": 400, "scale": 2.0, "refresh": 60000}));
    app.sync();

    let (w, h) = (200, 100);
    let mut client = Pattern::connect(&compositor, w, h);
    for global in ["zwlr_data_control_manager_v1", "ext_data_control_manager_v1"] {
        assert!(client.globals.iter().any(|g| g == global), "{global} missing from {:?}", client.globals);
    }
    client.draw(&pattern(w, h, 0), [0, 0, w, h], false);
    let window = app.wait_json("toplevel")["id"].as_u64().unwrap() as u32;
    app.wait_commit("the toplevel's first commit", |c| c.surface == window);

    // Unconstrained it would hang 80 px past the right edge and 50 past the bottom.
    let geometry = client.popup([150, 60, 10, 10], (120, 80));
    assert_eq!(geometry, (80, 20, 120, 80));
    let surface = app.wait("the popup's surface", |m| match m {
        Msg::Json(v) if v["type"] == "surface" && v["role"] == "popup" => Some(v.clone()),
        _ => None,
    });
    assert_eq!((surface["window"].as_u64(), surface["parent"].as_u64()), (Some(window as u64), Some(window as u64)));
    let id = surface["id"].as_u64().unwrap() as u32;
    let commit = app.wait_commit("the popup's commit", |c| c.surface == id && c.flags & FLAG_BUFFER != 0);
    assert_eq!(commit.window, window);
    // Window pixels at scale 2, inside the 400x200 window.
    assert_eq!((commit.x, commit.y, commit.dst_width, commit.dst_height), (160, 40, 240, 160));
    assert_eq!(app.layers[&id].pixels, to_rgba(&pattern(120, 80, 7)));

    // A window drawn larger than it was configured shows only the configured part.
    app.send_json(json!({"type": "window", "id": window, "width": 240, "height": 120, "scale": 2.0,
        "focused": true, "visible": true, "maximized": true, "fullscreen": false}));
    client.dispatch_until("the smaller configure", |s| s.size == (120, 60));
    client.draw(&pattern(w, h, 1), [0, 0, w, h], false);
    client.roundtrip();
    assert_eq!(client.popup([150, 60, 10, 10], (120, 80)), (0, 0, 120, 60));
}

/// foot, a real client: set FOOT to its binary (with fonts reachable) to run.
#[test]
fn foot_renders_and_its_hashes_match() {
    let Ok(foot) = std::env::var("FOOT") else {
        eprintln!("FOOT is not set; skipping");
        return;
    };
    let compositor = Compositor::start("foot");
    let mut app = App::connect(&compositor);
    app.auto_ack = true;
    app.wait_json("ready");
    app.send_json(json!({"type": "output", "width": 1080, "height": 2400, "scale": 2.625, "refresh": 60000}));
    app.sync();

    let mut child = Command::new(foot)
        .args(["--override", "pad=0x0", "sh", "-c", "echo pocketnix; exec cat"])
        .env("XDG_RUNTIME_DIR", &compositor.dir)
        .env("WAYLAND_DISPLAY", "wl-test")
        .env_remove("DISPLAY")
        .stdout(Stdio::null())
        .spawn()
        .unwrap();
    let toplevel = app.wait_json("toplevel");
    assert_eq!(toplevel["appId"], "foot");
    let window = toplevel["id"].as_u64().unwrap() as u32;
    let first = app.wait_commit("foot's first frame", |c| c.window == window && c.flags & FLAG_BUFFER != 0);
    assert_eq!(compositor.logged_hash(first.surface, first.seq), format!("{}x{} {:016x}", first.width, first.height, first.hash));
    // Fractional scale: foot renders at 2.625 itself, so its buffer matches the window's pixels.
    assert_eq!((first.width, first.height), (first.dst_width, first.dst_height));

    app.send_json(json!({"type": "window", "id": window, "width": 1080, "height": 2400, "scale": 2.625,
        "focused": true, "visible": true, "maximized": true, "fullscreen": false}));
    let before = app.hashed;
    for key in [35u32, 18, 38, 38, 24] {
        app.send_binary(0x30, &[&window.to_be_bytes(), &key.to_be_bytes(), &[1]]);
        app.send_binary(0x30, &[&window.to_be_bytes(), &key.to_be_bytes(), &[0]]);
    }
    let patch = app.wait_commit("a patch from typing", |c| {
        c.window == window && c.flags & FLAG_BUFFER != 0 && c.flags & FLAG_FULL == 0
    });
    assert!(app.hashed > before);
    assert_eq!(compositor.logged_hash(patch.surface, patch.seq), format!("{}x{} {:016x}", patch.width, patch.height, patch.hash));
    let _ = child.kill();
    let _ = child.wait();
}

fn write(path: &Path, content: impl AsRef<[u8]>) {
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(path, content).unwrap();
}

fn png(width: u32, height: u32, rgba: [u8; 4]) -> Vec<u8> {
    let mut out = Vec::new();
    let mut encoder = png::Encoder::new(&mut out, width, height);
    encoder.set_color(png::ColorType::Rgba);
    encoder.set_depth(png::BitDepth::Eight);
    let data: Vec<u8> = (0..width * height).flat_map(|_| rgba).collect();
    encoder.write_header().unwrap().write_image_data(&data).unwrap();
    out
}

/// A PNG's size and RGBA pixels.
fn decode_png(bytes: &[u8]) -> (u32, u32, Vec<u8>) {
    let decoder = png::Decoder::new(std::io::Cursor::new(bytes));
    let mut reader = decoder.read_info().unwrap();
    let mut buf = vec![0; reader.output_buffer_size().unwrap()];
    let info = reader.next_frame(&mut buf).unwrap();
    assert_eq!(info.color_type, png::ColorType::Rgba);
    buf.truncate(info.buffer_size());
    (info.width, info.height, buf)
}

fn pixel(img: &(u32, u32, Vec<u8>), x: u32, y: u32) -> [u8; 4] {
    let at = ((y * img.0 + x) * 4) as usize;
    img.2[at..at + 4].try_into().unwrap()
}

fn square_svg(color: &str) -> String {
    format!(r#"<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16"><rect width="16" height="16" fill="{color}"/></svg>"#)
}

fn entry(name: &str, extra: &str) -> String {
    format!("[Desktop Entry]\nType=Application\nName={name}\nExec=true\n{extra}\n")
}

fn ids(apps: &Value) -> Vec<String> {
    apps["apps"].as_array().unwrap().iter().map(|a| a["id"].as_str().unwrap().to_owned()).collect()
}

fn app_entry<'a>(apps: &'a Value, id: &str) -> &'a Value {
    apps["apps"].as_array().unwrap().iter().find(|a| a["id"] == id).unwrap()
}

#[test]
fn the_app_list_follows_the_data_dirs_and_renders_icons() {
    let dir = test_dir("apps-data");
    let (home, data1, data2) = (dir.join("home"), dir.join("data1"), dir.join("data2"));
    let apps1 = data1.join("applications");
    write(&home.join("applications/shadow.desktop"), entry("Mine", ""));
    write(&apps1.join("shadow.desktop"), entry("Theirs", ""));
    write(&apps1.join("hidden.desktop"), entry("Hidden", "Hidden=true"));
    write(&data2.join("applications/hidden.desktop"), entry("Shown later", ""));
    write(&apps1.join("sub/nested.desktop"), entry("Nested", ""));
    write(&apps1.join("term.desktop"), entry("Term", "Terminal=true"));
    write(&apps1.join("tryexec.desktop"), entry("Missing", "TryExec=pnc-no-such-program"));
    write(&apps1.join("tryexec-ok.desktop"), entry("Present", "TryExec=sh"));
    write(&apps1.join("noname.desktop"), "[Desktop Entry]\nType=Application\nExec=true\n");
    write(&apps1.join("noexec.desktop"), "[Desktop Entry]\nType=Application\nName=No exec\n");
    write(&apps1.join("dbus.desktop"), "[Desktop Entry]\nType=Application\nName=Bus\nDBusActivatable=true\n");
    write(&apps1.join("nodisplay.desktop"), entry("Quiet", "NoDisplay=true"));
    write(&apps1.join("onlyshow.desktop"), entry("Elsewhere", "OnlyShowIn=GNOME;"));
    write(&apps1.join("broken.desktop"), "[Desktop Entry]\nName=Broken\ngarbage\n");
    write(&apps1.join("link.desktop"), "[Desktop Entry]\nType=Link\nName=Link\nURL=https://example.org\n");
    write(&apps1.join("xpm.desktop"), entry("Old icon", "Icon=pnc-xpm"));
    write(&data1.join("pixmaps/pnc-xpm.xpm"), "/* XPM */");
    write(
        &apps1.join("calc.desktop"),
        "[Desktop Entry]\nType=Application\nName=Calculator\nName[da]=Lommeregner\nGenericName=Calc\n\
         Comment=Adds;\nKeywords=math;sum;\nKeywords[da]=matematik;\nCategories=Utility;Math;\nIcon=pnc-calc\n\
         Exec=calc\nActions=new;gone;\nStartupWMClass=CalcWM\n\n\
         [Desktop Action new]\nName=New window\nName[da_DK]=Nyt vindue\nIcon=pnc-pix\nExec=calc --new\n",
    );
    write(&apps1.join("same-icon.desktop"), entry("Same icon", "Icon=pnc-calc"));
    write(&data1.join("icons/hicolor/48x48/apps/pnc-calc.png"), png(48, 48, [0, 0, 255, 255]));
    write(&data1.join("icons/hicolor/scalable/apps/pnc-calc.svg"), square_svg("#ff0000"));
    write(&data1.join("pixmaps/pnc-pix.png"), png(32, 16, [0, 255, 0, 255]));
    // A profile: a symlink to one generation's tree.
    write(&dir.join("gen1/applications/gen.desktop"), entry("One", ""));
    std::os::unix::fs::symlink(dir.join("gen1"), dir.join("profile")).unwrap();

    let compositor = Compositor::start_with("apps", |cmd| {
        cmd.env("XDG_DATA_HOME", &home)
            .env("XDG_DATA_DIRS", format!("{}:{}:{}", data1.display(), data2.display(), dir.join("profile").display()))
            .env("HOME", &dir)
            .env("LANG", "da_DK.UTF-8")
            .env_remove("LC_ALL")
            .env_remove("LC_MESSAGES");
    });
    let mut app = App::connect(&compositor);
    let apps = app.wait_json("apps");
    app.wait_json("ready");
    // A hidden entry takes later directories' copies with it.
    assert_eq!(
        ids(&apps),
        ["calc", "dbus", "gen", "nodisplay", "onlyshow", "same-icon", "shadow", "sub-nested", "tryexec-ok", "xpm"],
        "{apps}"
    );
    let calc = app_entry(&apps, "calc");
    assert_eq!(calc["name"], "Lommeregner");
    assert_eq!(calc["genericName"], "Calc");
    assert_eq!(calc["comment"], "Adds;");
    assert_eq!(calc["keywords"], json!(["matematik"]));
    assert_eq!(calc["categories"], json!(["Utility", "Math"]));
    assert_eq!(calc["noDisplay"], false);
    let calc_key = calc["icon"].as_str().unwrap().to_owned();
    assert!(!calc_key.is_empty() && calc_key.bytes().all(|b| b.is_ascii_digit() || b.is_ascii_lowercase()));
    assert_eq!(app_entry(&apps, "same-icon")["icon"], calc_key.as_str());
    let actions = calc["actions"].as_array().unwrap();
    assert_eq!(actions.len(), 1, "{actions:?}");
    assert_eq!((&actions[0]["id"], &actions[0]["name"]), (&json!("new"), &json!("Nyt vindue")));
    let pix_key = actions[0]["icon"].as_str().unwrap().to_owned();
    assert!(!pix_key.is_empty() && pix_key != calc_key);
    assert_eq!(app_entry(&apps, "shadow")["name"], "Mine");
    assert_eq!(app_entry(&apps, "nodisplay")["noDisplay"], true);
    assert_eq!(app_entry(&apps, "onlyshow")["noDisplay"], true);
    assert_eq!(app_entry(&apps, "xpm")["icon"], "");

    // The SVG wins over the PNG and fills the square.
    let calc_icon = decode_png(&app.icon(&calc_key, 64));
    assert_eq!((calc_icon.0, calc_icon.1), (64, 64));
    assert_eq!(pixel(&calc_icon, 32, 32), [255, 0, 0, 255]);
    // A 2:1 pixmap is centred on transparency.
    let pix = decode_png(&app.icon(&pix_key, 32));
    assert_eq!((pix.0, pix.1), (32, 32));
    assert_eq!(pixel(&pix, 16, 2)[3], 0);
    assert_eq!(pixel(&pix, 16, 16), [0, 255, 0, 255]);
    assert!(app.icon("0123456789abcdef", 48).is_empty(), "an unknown key renders as no icon");

    // A new file in an applications directory shows up by itself.
    write(&home.join("applications/fresh.desktop"), entry("Fresh", ""));
    let apps = app.wait_json_where("apps", |v| ids(v).contains(&"fresh".to_owned()));
    assert_eq!(app_entry(&apps, "calc")["icon"], calc_key.as_str());

    // A switch replaces the profile's symlink, as nix-env and switch-to-configuration do.
    write(&dir.join("gen2/applications/gen.desktop"), entry("Two", ""));
    std::os::unix::fs::symlink(dir.join("gen2"), dir.join("profile.new")).unwrap();
    std::fs::rename(dir.join("profile.new"), dir.join("profile")).unwrap();
    app.wait_json_where("apps", |v| v["apps"].as_array().unwrap().iter().any(|a| a["id"] == "gen" && a["name"] == "Two"));

    // Icon directories aren't watched: SIGHUP rescans, and new content gets a new key.
    write(&data1.join("icons/hicolor/scalable/apps/pnc-calc.svg"), square_svg("#0000ff"));
    // SAFETY: signalling the child we started.
    unsafe { libc::kill(compositor.child.id() as i32, libc::SIGHUP) };
    let apps = app.wait_json_where("apps", |v| app_entry(v, "calc")["icon"] != calc_key.as_str());
    let new_key = app_entry(&apps, "calc")["icon"].as_str().unwrap().to_owned();
    assert_eq!(pixel(&decode_png(&app.icon(&new_key, 64)), 32, 32), [0, 0, 255, 255]);
    let _ = std::fs::remove_dir_all(&dir);
}

/// Stand-ins for systemd-cat and systemd-run that log their arguments and
/// run the command; the build sandbox has no user manager.
fn fake_systemd(dir: &Path) -> (PathBuf, PathBuf) {
    let bin = dir.join("bin");
    let log = dir.join("run.log");
    write(&bin.join("systemd-cat"), "#!/bin/sh\nwhile case \"$1\" in -*) true;; *) false;; esac; do shift; done\nexec \"$@\"\n");
    write(
        &bin.join("systemd-run"),
        format!("#!/bin/sh\necho \"$@\" >> '{}'\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n", log.display()),
    );
    for f in ["systemd-cat", "systemd-run"] {
        std::fs::set_permissions(bin.join(f), std::fs::Permissions::from_mode(0o755)).unwrap();
    }
    (bin, log)
}

/// The Exec line that runs `wayland_child` with this app id.
fn child_exec(app_id: &str, extra_env: &str) -> String {
    let exe = std::env::current_exe().unwrap();
    format!("env PNC_CHILD={app_id} {extra_env} {} wayland_child --exact", exe.display())
}

fn start_launcher(name: &str, apps: &[(&str, String)]) -> (Compositor, PathBuf, PathBuf) {
    let dir = test_dir(&format!("{name}-data"));
    for (id, content) in apps {
        write(&dir.join(format!("data/applications/{id}.desktop")), content);
    }
    let (bin, log) = fake_systemd(&dir);
    let data = dir.join("data");
    let path = format!("{}:{}", bin.display(), std::env::var("PATH").unwrap_or_default());
    let compositor = Compositor::start_with(name, |cmd| {
        cmd.env("XDG_DATA_HOME", dir.join("home"))
            .env("XDG_DATA_DIRS", &data)
            .env("HOME", &dir)
            .env("PATH", path);
    });
    (compositor, dir, log)
}

fn connect_ready(compositor: &Compositor) -> App {
    let mut app = App::connect(compositor);
    app.auto_ack = true;
    app.wait_json("ready");
    app.send_json(json!({"type": "output", "width": 800, "height": 600, "scale": 1.0, "refresh": 60000}));
    app
}

#[test]
fn launches_bind_their_window_and_end_once() {
    let (compositor, dir, log) = start_launcher(
        "launch",
        &[
            // Its app id isn't the entry's: only the token can bind it.
            ("test.child", format!("[Desktop Entry]\nType=Application\nName=Child\nExec={}\n", child_exec("other.id", ""))),
            (
                "test.notoken",
                format!("[Desktop Entry]\nType=Application\nName=No token\nExec={}\n", child_exec("test.notoken", "PNC_CHILD_NO_TOKEN=1")),
            ),
            ("test.false", "[Desktop Entry]\nType=Application\nName=Fails\nExec=false\n".into()),
        ],
    );
    let mut app = connect_ready(&compositor);

    let launch = |app: &mut App, id: &str, entry: &str, action: &str, tab: &str| {
        app.send_json(json!({"type": "launch", "id": id, "app": entry, "action": action, "tab": tab}));
    };
    launch(&mut app, "l1", "test.child", "", "0123456789abcdef");
    app.wait_json_where("launch-started", |v| v["id"] == "l1");
    let t1 = app.wait_json_where("toplevel", |v| v["launch"] == "l1");
    assert_eq!(t1["appId"], "other.id");
    assert_eq!(t1["app"], "test.child");
    assert_eq!(t1["tab"], "0123456789abcdef");
    assert_eq!(t1["window"], t1["id"]);
    assert_eq!(t1["title"], "tab=0123456789abcdef token=true");
    let run = std::fs::read_to_string(&log).unwrap();
    assert!(run.contains("--user --scope --slice=app.slice"), "{run}");
    assert!(run.contains("--unit=app-pocketnix-test.child-"), "{run}");

    // A launch sent again gets its state again.
    launch(&mut app, "l1", "test.child", "", "");
    let again = app.wait_json("activate");
    assert_eq!((&again["window"], &again["launch"]), (&t1["id"], &json!("l1")));

    launch(&mut app, "l2", "test.notoken", "", "");
    let t2 = app.wait_json_where("toplevel", |v| v["appId"] == "test.notoken");
    assert_eq!((&t2["launch"], &t2["app"], &t2["tab"]), (&json!("l2"), &json!("test.notoken"), &json!("")));
    assert_eq!(t2["title"], "tab= token=true");

    launch(&mut app, "l3", "test.false", "", "");
    let failed = app.wait_json_where("launch-failed", |v| v["id"] == "l3");
    assert_eq!(failed["reason"], "exec");
    let message = failed["message"].as_str().unwrap();
    assert!(message.split(|c: char| !c.is_ascii_digit()).any(|n| n == "1"), "{message:?} doesn't name status 1");
    launch(&mut app, "l4", "nope", "", "");
    assert_eq!(app.wait_json_where("launch-failed", |v| v["id"] == "l4")["reason"], "unknown");
    launch(&mut app, "l4", "nope", "", "");
    assert_eq!(app.wait_json_where("launch-failed", |v| v["id"] == "l4")["reason"], "unknown");
    launch(&mut app, "l5", "test.child", "nope", "");
    assert_eq!(app.wait_json_where("launch-failed", |v| v["id"] == "l5")["reason"], "unknown");

    // Its window closed: the launch says so when sent again.
    app.send_json(json!({"type": "close", "id": t1["id"]}));
    app.wait_json_where("toplevel-removed", |v| v["id"] == t1["id"]);
    launch(&mut app, "l1", "test.child", "", "");
    assert_eq!(app.wait_json_where("launch-failed", |v| v["id"] == "l1")["reason"], "closed");

    launch(&mut app, "not an id", "test.child", "", "");
    app.wait_json("error");
    let deadline = Instant::now() + TIMEOUT;
    while app.read_frame().is_some() {
        assert!(Instant::now() < deadline, "the connection stayed open");
    }
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn a_client_from_a_tab_lands_in_it_and_moves() {
    let (compositor, dir, _) = start_launcher("tab", &[]);
    let mut app = connect_ready(&compositor);
    let spawn = |app_id: &str, tab: &str| {
        Command::new(std::env::current_exe().unwrap())
            .args(["wayland_child", "--exact"])
            .env("PNC_CHILD", app_id)
            .env("POCKETNIX_TAB", tab)
            .env("WAYLAND_DISPLAY", "wl-test")
            .env("XDG_RUNTIME_DIR", &compositor.dir)
            .env_remove("XDG_ACTIVATION_TOKEN")
            .stdout(Stdio::null())
            .spawn()
            .unwrap()
    };
    let mut good = spawn("tab.good", "fedcba9876543210");
    let t = app.wait_json_where("toplevel", |v| v["appId"] == "tab.good");
    assert_eq!((&t["tab"], &t["launch"]), (&json!("fedcba9876543210"), &json!("")));
    let mut bad = spawn("tab.bad", "not a tab!");
    assert_eq!(app.wait_json_where("toplevel", |v| v["appId"] == "tab.bad")["tab"], "");

    app.send_json(json!({"type": "move", "id": t["id"], "tab": "0011223344556677"}));
    let moved = app.wait_json_where("toplevel", |v| v["id"] == t["id"]);
    assert_eq!(moved["tab"], "0011223344556677");
    app.send_json(json!({"type": "move", "id": t["id"], "tab": ""}));
    assert_eq!(app.wait_json_where("toplevel", |v| v["id"] == t["id"])["tab"], "");
    for child in [&mut good, &mut bad] {
        let _ = child.kill();
        let _ = child.wait();
    }
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn a_dialog_draws_inside_its_parents_window() {
    let compositor = Compositor::start("dialog");
    let mut app = connect_ready(&compositor);
    app.sync();
    let (w, h) = (200, 100);
    let mut client = Pattern::connect(&compositor, w, h);
    client.draw(&pattern(w, h, 0), [0, 0, w, h], false);
    let window = app.wait_json("toplevel")["id"].as_u64().unwrap() as u32;
    app.send_json(json!({"type": "window", "id": window, "width": 400, "height": 300, "scale": 1.0,
        "focused": true, "visible": true, "maximized": false, "fullscreen": false}));
    client.dispatch_until("keyboard focus", |s| s.keyboard_focus);

    let parent = client.toplevel.clone();
    let dialog = client.dialog(&parent, 60, 40, 3);
    // Bounded by what the window shows: drawn 200x100 of its configured 400x300.
    assert_eq!(client.state.dialog_bounds, Some((200, 100)));
    let d = app.wait_json_where("toplevel", |v| v["parent"] == window);
    let id = d["id"].as_u64().unwrap() as u32;
    assert_eq!((&d["window"], &d["tab"], &d["appId"]), (&json!(window), &json!(""), &json!("test.pattern")));
    let surface = app.wait_json_where("surface", |v| v["toplevel"] == id);
    assert_eq!((&surface["window"], &surface["role"], &surface["parent"]), (&json!(window), &json!("toplevel"), &json!(0)));
    let commit = app.wait_commit("the dialog's commit", |c| c.surface == id && c.flags & FLAG_BUFFER != 0);
    assert_eq!(commit.window, window);
    assert_eq!((commit.x, commit.y, commit.dst_width, commit.dst_height), (70, 30, 60, 40), "centred on its parent");
    let dialog_surface = dialog.surface.clone();
    client.dispatch_until("the dialog's keyboard focus", |s| s.keyboard_surface.as_ref() == Some(&dialog_surface));

    // Its popups may use the whole window: anchored at its corner, this one slides into the window's.
    assert_eq!(client.popup_from(&dialog.xdg_surface, [50, 30, 10, 10], (120, 80)), (10, -10, 120, 80));

    let press = |app: &mut App, x: f32, y: f32| {
        for state in [1u8, 0] {
            app.send_binary(
                0x21,
                &[&window.to_be_bytes(), &x.to_be_bytes(), &y.to_be_bytes(), &BTN_LEFT.to_be_bytes(), &[state]],
            );
        }
    };
    let main = client.surface.clone();
    press(&mut app, 10.0, 10.0);
    client.dispatch_until("the parent's keyboard focus", |s| s.keyboard_surface.as_ref() == Some(&main));
    // Left of the popup, which covers the dialog's right part.
    press(&mut app, 75.0, 60.0);
    client.dispatch_until("the dialog's keyboard focus again", |s| {
        s.keyboard_surface.as_ref() == Some(&dialog_surface) && s.pointer_surface.as_ref() == Some(&dialog_surface)
    });

    // Modal: the parent takes no presses and no focus.
    let wm_dialog: XdgWmDialogV1 = client.registry.bind(&client.qh, 1..=1, ()).unwrap();
    wm_dialog.get_xdg_dialog(&dialog.toplevel, &client.qh, ()).set_modal();
    client.roundtrip();
    let buttons = |s: &Wl| s.pointer.iter().filter(|e| e.starts_with("button")).count();
    let before = buttons(&client.state);
    press(&mut app, 10.0, 10.0);
    app.send_binary(0x20, &[&window.to_be_bytes(), &11f32.to_be_bytes(), &11f32.to_be_bytes()]);
    client.dispatch_until("the pointer over the parent", |s| s.pointer_surface.as_ref() == Some(&main));
    client.roundtrip();
    assert_eq!(buttons(&client.state), before, "a press reached a modal dialog's parent");
    assert_eq!(client.state.keyboard_surface.as_ref(), Some(&dialog_surface));

    // Gone: the app forgets it, and focus returns to the parent.
    dialog.toplevel.destroy();
    dialog.xdg_surface.destroy();
    dialog.surface.destroy();
    client.roundtrip();
    app.wait_json_where("toplevel-removed", |v| v["id"] == id);
    client.dispatch_until("the parent's keyboard focus back", |s| s.keyboard_surface.as_ref() == Some(&main));
}

fn f(v: f32) -> [u8; 4] {
    v.to_be_bytes()
}

#[test]
fn full_screen_asked_before_mapping_follows_the_toplevel() {
    let compositor = Compositor::start("early-fullscreen");
    let mut app = App::connect(&compositor);
    app.wait_json("ready");
    app.send_json(json!({"type": "output", "width": 800, "height": 600, "scale": 1.0, "refresh": 60000}));
    app.sync();

    let (w, h) = (200, 100);
    let mut client = Pattern::connect(&compositor, w, h);
    client.toplevel.set_fullscreen(None);
    client.roundtrip();
    client.draw(&pattern(w, h, 0), [0, 0, w, h], false);
    let toplevel = app.wait_json("toplevel");
    let request = app.wait_json("toplevel-request");
    assert_eq!((&request["id"], &request["request"]), (&toplevel["id"], &json!("fullscreen")));
}

#[test]
fn touch_lands_on_the_surface_under_each_point() {
    let compositor = Compositor::start("touch");
    let mut app = App::connect(&compositor);
    app.auto_ack = true;
    app.wait_json("ready");
    app.send_json(json!({"type": "output", "width": 800, "height": 600, "scale": 1.75, "refresh": 60000}));
    app.sync();

    let (w, h) = (200, 100);
    let mut client = Pattern::connect(&compositor, w, h);
    client.dispatch_until("a wl_touch", |s| s.has_touch);
    client.draw(&pattern(w, h, 0), [0, 0, w, h], false);
    let t = app.wait_json("toplevel");
    assert_eq!((&t["touch"], &t["scale"]), (&json!(true), &json!(1.75)));
    let window = t["id"].as_u64().unwrap() as u32;
    app.send_json(json!({"type": "window", "id": window, "width": 700, "height": 350, "scale": 1.75,
        "focused": true, "visible": true, "maximized": false, "fullscreen": false}));
    client.dispatch_until("the window's configure", |s| s.size == (400, 200));

    // At 1.75 a popup's edges round on their own: logical (160, 70) to (200, 100).
    assert_eq!(client.popup([150, 60, 10, 10], (40, 30)), (160, 70, 40, 30));
    let popup = app.wait_json_where("surface", |v| v["role"] == "popup")["id"].as_u64().unwrap() as u32;
    let commit = app.wait_commit("the popup's commit", |c| c.surface == popup && c.flags & FLAG_BUFFER != 0);
    assert_eq!((commit.x, commit.y, commit.dst_width, commit.dst_height), (280, 123, 70, 52));

    let w32 = window.to_be_bytes();
    // One finger on the toplevel, then one on the popup, in window pixels.
    app.send_binary(0x40, &[&w32, &7u32.to_be_bytes(), &f(35.0), &f(52.5)]);
    app.send_binary(0x44, &[&w32]);
    app.send_binary(0x40, &[&w32, &9u32.to_be_bytes(), &f(297.5), &f(140.0)]);
    app.send_binary(0x44, &[&w32]);
    // The first moves past its surface's edge and stays its surface's.
    app.send_binary(0x42, &[&w32, &7u32.to_be_bytes(), &f(420.0), &f(280.0)]);
    app.send_binary(0x44, &[&w32]);
    app.send_binary(0x41, &[&w32, &7u32.to_be_bytes()]);
    app.send_binary(0x41, &[&w32, &9u32.to_be_bytes()]);
    app.send_binary(0x44, &[&w32]);
    client.dispatch_until("both points up", |s| s.touch.iter().filter(|e| *e == "frame").count() == 4);
    assert_eq!(
        client.state.touch,
        ["down 0 20 30", "frame", "down 1 10 10", "frame", "motion 0 240 160", "frame", "up 0", "up 1", "frame"]
    );
    let popup_surface = client.popup_surface.clone().unwrap();
    assert_eq!(client.state.touch_surfaces, [client.surface.clone(), popup_surface]);

    // A cancel ends a point without effect.
    client.state.touch.clear();
    app.send_binary(0x40, &[&w32, &7u32.to_be_bytes(), &f(35.0), &f(52.5)]);
    app.send_binary(0x44, &[&w32]);
    app.send_binary(0x43, &[&w32]);
    app.send_binary(0x41, &[&w32, &7u32.to_be_bytes()]);
    app.send_binary(0x44, &[&w32]);
    client.dispatch_until("the cancel", |s| s.touch.iter().any(|e| e == "cancel"));
    client.roundtrip();
    assert_eq!(client.state.touch[..2], ["down 0 20 30", "frame"]);
    assert_eq!(client.state.touch.last().map(String::as_str), Some("cancel"));
    assert!(!client.state.touch.iter().any(|e| e.starts_with("up")), "{:?}", client.state.touch);

    // A new scale dismisses the popups, placed for the old logical size.
    app.send_json(json!({"type": "window", "id": window, "width": 700, "height": 350, "scale": 2.0,
        "focused": true, "visible": true, "maximized": false, "fullscreen": false}));
    client.dispatch_until("the popup's dismissal", |s| s.popup_done);

    // A point the app never lifts ends with its connection.
    client.state.touch.clear();
    app.send_binary(0x40, &[&w32, &3u32.to_be_bytes(), &f(40.0), &f(60.0)]);
    app.send_binary(0x44, &[&w32]);
    client.dispatch_until("the point", |s| s.touch.iter().any(|e| e == "frame"));
    drop(app);
    client.dispatch_until("the cancel", |s| s.touch.iter().any(|e| e == "cancel"));
}

#[test]
fn a_new_window_opens_at_its_apps_size_factor() {
    let entry = "[Desktop Entry]\nType=Application\nName=Pattern\nExec=true\n";
    let (compositor, dir, _) = start_launcher("settings", &[("test.pattern", entry.into())]);
    let mut app = connect_ready(&compositor);
    app.send_json(json!({"type": "output", "width": 800, "height": 600, "scale": 2.0, "refresh": 60000}));
    app.send_json(json!({"type": "settings", "size": 0.75, "apps": {"test.pattern": {"size": 0.5}}}));
    app.sync();
    let (w, h) = (200, 100);
    let mut client = Pattern::connect(&compositor, w, h);
    assert_eq!(client.state.size, (800, 600), "the output's size at 2 × 0.5");
    client.draw(&pattern(w, h, 0), [0, 0, w, h], false);
    let t = app.wait_json("toplevel");
    assert_eq!((&t["scale"], &t["width"]), (&json!(1.0), &json!(200)));
    let _ = std::fs::remove_dir_all(&dir);
}

#[test]
fn text_input_bridges_the_ime() {
    let compositor = Compositor::start("text-input");
    let mut app = connect_ready(&compositor);
    app.send_json(json!({"type": "output", "width": 800, "height": 600, "scale": 2.0, "refresh": 60000}));
    app.sync();
    let (w, h) = (200, 100);
    let mut client = Pattern::connect(&compositor, w, h);
    let ti = client.text_input();
    client.draw(&pattern(w, h, 0), [0, 0, w, h], false);
    let window = app.wait_json("toplevel")["id"].as_u64().unwrap() as u32;
    app.send_json(json!({"type": "window", "id": window, "width": 400, "height": 200, "scale": 2.0,
        "focused": true, "visible": true, "maximized": false, "fullscreen": false}));
    let main = client.surface.clone();
    client.dispatch_until("the text input's enter", |s| s.ti_entered.as_ref() == Some(&main));

    // Offsets are UTF-8 bytes: the cursor is after "wö", byte 10 of 13.
    ti.enable();
    ti.set_surrounding_text("héllo wörld".into(), 10, 10);
    ti.set_content_type(
        zwp_text_input_v3::ContentHint::Spellcheck | zwp_text_input_v3::ContentHint::Multiline,
        zwp_text_input_v3::ContentPurpose::Email,
    );
    ti.set_cursor_rectangle(10, 5, 2, 12);
    ti.commit();
    client.roundtrip();
    let state = app.wait_json_where("text-input", |v| v["session"] != 0);
    assert_eq!(
        state,
        json!({"type": "text-input", "window": window, "session": 1, "applied": 0, "text": "héllo wörld",
            "cursor": 10, "anchor": 10, "cause": "other", "purpose": "email", "hints": ["spellcheck", "multiline"],
            "cursorRect": {"x": 20, "y": 10, "width": 4, "height": 24}})
    );

    // Replace "ö" (2 bytes) with "ø", and show a preedit after it.
    app.send_json(json!({"type": "ime", "session": 1, "serial": 1, "deleteBefore": 2, "commit": "ø", "preedit": "ab"}));
    client.dispatch_until("the IME's done", |s| s.ti.iter().any(|e| e.starts_with("done")));
    assert_eq!(client.state.ti, ["delete 2 0", "commit ø", "preedit ab 2 2", "done 1"]);
    ti.set_surrounding_text("héllo wørld".into(), 10, 10);
    ti.set_text_change_cause(zwp_text_input_v3::ChangeCause::InputMethod);
    ti.commit();
    client.roundtrip();
    let applied = app.wait_json_where("text-input", |v| v["applied"] == 1);
    assert_eq!(
        (&applied["session"], &applied["text"], &applied["cause"]),
        (&json!(1), &json!("héllo wørld"), &json!("input-method"))
    );

    // A new connection replays it, its sessions counting from 1 again.
    let mut app = App::connect(&compositor);
    app.auto_ack = true;
    let replayed = app.wait_json("text-input");
    assert_eq!(
        (&replayed["session"], &replayed["applied"], &replayed["text"]),
        (&json!(1), &json!(0), &json!("héllo wørld"))
    );
    app.wait_json("ready");

    // Enabling again is a new session, and an edit for the old one is dropped.
    ti.enable();
    ti.commit();
    client.roundtrip();
    let fresh = app.wait_json_where("text-input", |v| v["session"] == 2);
    assert_eq!(fresh["text"], Value::Null);
    client.state.ti.clear();
    app.send_json(json!({"type": "ime", "session": 1, "serial": 1, "commit": "stale"}));
    app.send_json(json!({"type": "ime", "session": 2, "serial": 2, "commit": "fresh"}));
    client.dispatch_until("the IME's done", |s| s.ti.iter().any(|e| e.starts_with("done")));
    assert_eq!(client.state.ti, ["commit fresh", "done 3"]);

    ti.disable();
    ti.commit();
    client.roundtrip();
    let ended = app.wait_json_where("text-input", |v| v["session"] == 0);
    assert_eq!(ended["window"], 0);
}

/// The keysym each key press typed, from the keymap and modifiers in effect.
fn typed(s: &Wl) -> Vec<u32> {
    let ctx = xkb::Context::new(xkb::CONTEXT_NO_FLAGS);
    s.presses
        .iter()
        .map(|(key, keymap, depressed, locked, group)| {
            let keymap = xkb::Keymap::new_from_string(&ctx, s.keymaps[*keymap].clone(), xkb::KEYMAP_FORMAT_TEXT_V1, 0).unwrap();
            let mut state = xkb::State::new(&keymap);
            state.update_mask(*depressed, 0, *locked, 0, 0, *group);
            state.key_get_one_sym(xkb::Keycode::new(key + 8)).raw()
        })
        .collect()
}

/// Whether any key of `keymap` types `sym`.
fn keymap_has(keymap: &str, sym: u32) -> bool {
    let ctx = xkb::Context::new(xkb::CONTEXT_NO_FLAGS);
    let keymap = xkb::Keymap::new_from_string(&ctx, keymap.to_owned(), xkb::KEYMAP_FORMAT_TEXT_V1, 0).unwrap();
    (keymap.min_keycode().raw()..=keymap.max_keycode().raw()).any(|k| {
        let k = xkb::Keycode::new(k);
        (0..keymap.num_layouts_for_key(k)).any(|l| {
            (0..keymap.num_levels_for_key(k, l))
                .any(|v| keymap.key_get_syms_by_level(k, l, v).iter().any(|s| s.raw() == sym))
        })
    })
}

#[test]
fn text_becomes_key_presses_in_the_guests_layout() {
    let compositor = Compositor::start_with("typing", |cmd| {
        cmd.env_remove("XKB_DEFAULT_LAYOUT").env_remove("XKB_DEFAULT_VARIANT");
    });
    let mut app = connect_ready(&compositor);
    app.sync();
    let (w, h) = (200, 100);
    let mut client = Pattern::connect(&compositor, w, h);
    client.draw(&pattern(w, h, 0), [0, 0, w, h], false);
    let window = app.wait_json("toplevel")["id"].as_u64().unwrap() as u32;
    app.send_json(json!({"type": "window", "id": window, "width": 400, "height": 300, "scale": 1.0,
        "focused": true, "visible": true, "maximized": false, "fullscreen": false}));
    client.dispatch_until("keyboard focus", |s| s.keyboard_focus && !s.keymaps.is_empty());

    // "語" isn't in the layout: it's typed through a keymap with it, and the layout's comes back.
    // BEL, a control character, is dropped.
    let cjk = 0x0100_8a9e;
    app.send_json(json!({"type": "text", "window": window, "text": "hI!\n語a\u{7}"}));
    client.dispatch_until("six key presses", |s| s.presses.len() == 6);
    client.roundtrip();
    assert_eq!(typed(&client.state), [0x68, 0x49, 0x21, 0xff0d, cjk, 0x61]);
    assert!(keymap_has(&client.state.keymaps[client.state.presses[4].1], cjk));
    let last = client.state.keymaps.last().unwrap();
    assert!(!keymap_has(last, cjk), "the layout's keymap wasn't restored");
    assert_eq!(client.state.mods_depressed, 0, "a typed character's modifiers stayed");

    // Caps Lock and a held Ctrl from MODIFIERS outlast the keymap swaps: Ctrl types with the character.
    let w32 = window.to_be_bytes();
    app.send_binary(0x31, &[&w32, &[1 << 1 | 1 << 4]]);
    client.dispatch_until("caps lock and ctrl", |s| s.mods_locked != 0 && s.mods_depressed != 0);
    client.roundtrip();
    let (held, locked) = (client.state.mods_depressed, client.state.mods_locked);
    let n = client.state.presses.len();
    app.send_json(json!({"type": "text", "window": window, "text": "語"}));
    client.dispatch_until("a key press", |s| s.presses.len() == n + 1);
    client.roundtrip();
    assert_eq!(client.state.presses[n].2 & held, held, "Ctrl was let go for the press");
    assert_eq!(client.state.presses[n].3, locked, "Caps Lock was let go for the press");
    assert_eq!((client.state.mods_depressed, client.state.mods_locked), (held, locked));
    app.send_binary(0x31, &[&w32, &[0]]);
    client.dispatch_until("no modifiers", |s| s.mods_locked == 0 && s.mods_depressed == 0);
    client.roundtrip();

    // The guest's layout: "z" is on the Y key in German.
    const KEY_Y: u32 = 21;
    app.send_json(json!({"type": "keyboard-layout", "layout": "de", "variant": ""}));
    let n = client.state.presses.len();
    app.send_json(json!({"type": "text", "window": window, "text": "z"}));
    client.dispatch_until("a key press", |s| s.presses.len() == n + 1);
    assert_eq!(client.state.presses[n].0, KEY_Y);
    // One that doesn't compile keeps it.
    app.send_json(json!({"type": "keyboard-layout", "layout": "no-such-layout", "variant": ""}));
    app.send_json(json!({"type": "text", "window": window, "text": "z"}));
    client.dispatch_until("a key press", |s| s.presses.len() == n + 2);
    assert_eq!(client.state.presses[n + 1].0, KEY_Y);
}

/// Not a test on its own: the Wayland client the launch and tab tests
/// start, when `PNC_CHILD` names its app id. It activates with
/// `XDG_ACTIVATION_TOKEN` between its first commit and its first buffer, as
/// the apps tried do, and exits when closed.
#[test]
fn wayland_child() {
    let Ok(app_id) = std::env::var("PNC_CHILD") else { return };
    let token = std::env::var("XDG_ACTIVATION_TOKEN").unwrap_or_default();
    let same = !token.is_empty() && std::env::var("DESKTOP_STARTUP_ID").is_ok_and(|d| d == token);
    let tab = std::env::var("POCKETNIX_TAB").unwrap_or_default();
    let conn = Connection::connect_to_env().unwrap();
    let (globals, mut queue) = registry_queue_init::<ChildWl>(&conn).unwrap();
    let qh = queue.handle();
    let compositor: WlCompositor = globals.bind(&qh, 4..=6, ()).unwrap();
    let shm: WlShm = globals.bind(&qh, 1..=1, ()).unwrap();
    let wm_base: XdgWmBase = globals.bind(&qh, 1..=6, ()).unwrap();
    let activation: XdgActivationV1 = globals.bind(&qh, 1..=1, ()).unwrap();
    let surface = compositor.create_surface(&qh, ());
    let xdg_surface = wm_base.get_xdg_surface(&surface, &qh, ());
    let toplevel = xdg_surface.get_toplevel(&qh, ());
    toplevel.set_app_id(app_id);
    toplevel.set_title(format!("tab={tab} token={same}"));
    surface.commit();
    let mut state = ChildWl::default();
    while state.configure.is_none() {
        queue.blocking_dispatch(&mut state).unwrap();
    }
    xdg_surface.ack_configure(state.configure.take().unwrap());
    if !token.is_empty() && std::env::var_os("PNC_CHILD_NO_TOKEN").is_none() {
        activation.activate(token, &surface);
    }
    let (w, h) = (100, 50);
    // SAFETY: memfd_create with a valid C string.
    let fd = unsafe { libc::memfd_create(c"child".as_ptr(), libc::MFD_CLOEXEC) };
    // SAFETY: a fresh descriptor we own.
    let file = std::fs::File::from(unsafe { OwnedFd::from_raw_fd(fd) });
    file.write_all_at(&pattern(w, h, 5), 0).unwrap();
    let pool = shm.create_pool(file.as_fd(), w * h * 4, &qh, ());
    let buffer = pool.create_buffer(0, w, h, w * 4, wl_shm::Format::Argb8888, &qh, ());
    surface.attach(Some(&buffer), 0, 0);
    surface.damage_buffer(0, 0, w, h);
    surface.commit();
    while !state.closed {
        if queue.blocking_dispatch(&mut state).is_err() {
            break;
        }
    }
}

#[derive(Default)]
struct ChildWl {
    configure: Option<u32>,
    closed: bool,
}

impl Dispatch<WlRegistry, GlobalListContents> for ChildWl {
    fn event(_: &mut Self, _: &WlRegistry, _: <WlRegistry as wayland_client::Proxy>::Event, _: &GlobalListContents, _: &Connection, _: &QueueHandle<Self>) {}
}

wayland_client::delegate_noop!(ChildWl: ignore WlCompositor);
wayland_client::delegate_noop!(ChildWl: ignore WlShm);
wayland_client::delegate_noop!(ChildWl: ignore WlShmPool);
wayland_client::delegate_noop!(ChildWl: ignore WlBuffer);
wayland_client::delegate_noop!(ChildWl: ignore WlSurface);
wayland_client::delegate_noop!(ChildWl: ignore XdgActivationV1);

impl Dispatch<XdgWmBase, ()> for ChildWl {
    fn event(_: &mut Self, base: &XdgWmBase, event: xdg_wm_base::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_wm_base::Event::Ping { serial } = event {
            base.pong(serial);
        }
    }
}

impl Dispatch<XdgSurface, ()> for ChildWl {
    fn event(state: &mut Self, _: &XdgSurface, event: xdg_surface::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_surface::Event::Configure { serial } = event {
            state.configure = Some(serial);
        }
    }
}

impl Dispatch<XdgToplevel, ()> for ChildWl {
    fn event(state: &mut Self, _: &XdgToplevel, event: xdg_toplevel::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_toplevel::Event::Close = event {
            state.closed = true;
        }
    }
}

impl Dispatch<WlRegistry, GlobalListContents> for Wl {
    fn event(_: &mut Self, _: &WlRegistry, _: <WlRegistry as wayland_client::Proxy>::Event, _: &GlobalListContents, _: &Connection, _: &QueueHandle<Self>) {}
}

wayland_client::delegate_noop!(Wl: ignore WlCompositor);
wayland_client::delegate_noop!(Wl: ignore WlShm);
wayland_client::delegate_noop!(Wl: ignore WlShmPool);
wayland_client::delegate_noop!(Wl: ignore WlBuffer);
wayland_client::delegate_noop!(Wl: ignore WlSurface);

impl Dispatch<XdgWmBase, ()> for Wl {
    fn event(_: &mut Self, base: &XdgWmBase, event: xdg_wm_base::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_wm_base::Event::Ping { serial } = event {
            base.pong(serial);
        }
    }
}

impl Dispatch<XdgSurface, ()> for Wl {
    fn event(state: &mut Self, _: &XdgSurface, event: xdg_surface::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_surface::Event::Configure { serial } = event {
            state.configure = Some(serial);
        }
    }
}

impl Dispatch<XdgSurface, u32> for Wl {
    fn event(state: &mut Self, _: &XdgSurface, event: xdg_surface::Event, _: &u32, _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_surface::Event::Configure { serial } = event {
            state.popup_configure = Some(serial);
        }
    }
}

impl Dispatch<XdgPopup, ()> for Wl {
    fn event(state: &mut Self, _: &XdgPopup, event: xdg_popup::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        match event {
            xdg_popup::Event::Configure { x, y, width, height } => state.popup_geometry = Some((x, y, width, height)),
            xdg_popup::Event::PopupDone => state.popup_done = true,
            _ => {}
        }
    }
}

wayland_client::delegate_noop!(Wl: ignore XdgPositioner);

impl Dispatch<XdgToplevel, ()> for Wl {
    fn event(state: &mut Self, _: &XdgToplevel, event: xdg_toplevel::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        match event {
            xdg_toplevel::Event::Configure { width, height, states } => {
                state.size = (width, height);
                state.states = states.chunks(4).map(|c| u32::from_ne_bytes(c.try_into().unwrap())).collect();
            }
            xdg_toplevel::Event::ConfigureBounds { width, height } => state.bounds = Some((width, height)),
            _ => {}
        }
    }
}

impl Dispatch<XdgSurface, Tag> for Wl {
    fn event(state: &mut Self, _: &XdgSurface, event: xdg_surface::Event, _: &Tag, _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_surface::Event::Configure { serial } = event {
            state.dialog_configure = Some(serial);
        }
    }
}

impl Dispatch<XdgToplevel, Tag> for Wl {
    fn event(state: &mut Self, _: &XdgToplevel, event: xdg_toplevel::Event, _: &Tag, _: &Connection, _: &QueueHandle<Self>) {
        if let xdg_toplevel::Event::ConfigureBounds { width, height } = event {
            state.dialog_bounds = Some((width, height));
        }
    }
}

wayland_client::delegate_noop!(Wl: ignore XdgWmDialogV1);
wayland_client::delegate_noop!(Wl: ignore XdgDialogV1);

impl Dispatch<WlCallback, ()> for Wl {
    fn event(state: &mut Self, _: &WlCallback, event: wl_callback::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        if let wl_callback::Event::Done { .. } = event {
            state.frames += 1;
        }
    }
}

impl Dispatch<WlSeat, ()> for Wl {
    fn event(state: &mut Self, seat: &WlSeat, event: wl_seat::Event, _: &(), _: &Connection, qh: &QueueHandle<Self>) {
        if let wl_seat::Event::Capabilities { capabilities: WEnum::Value(caps) } = event {
            if caps.contains(wl_seat::Capability::Pointer) {
                seat.get_pointer(qh, ());
            }
            if caps.contains(wl_seat::Capability::Keyboard) {
                seat.get_keyboard(qh, ());
            }
            if caps.contains(wl_seat::Capability::Touch) && !state.has_touch {
                seat.get_touch(qh, ());
                state.has_touch = true;
            }
        }
    }
}

impl Dispatch<WlTouch, ()> for Wl {
    fn event(state: &mut Self, _: &WlTouch, event: wl_touch::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        match event {
            wl_touch::Event::Down { surface, id, x, y, .. } => {
                state.touch.push(format!("down {id} {x} {y}"));
                state.touch_surfaces.push(surface);
            }
            wl_touch::Event::Motion { id, x, y, .. } => state.touch.push(format!("motion {id} {x} {y}")),
            wl_touch::Event::Up { id, .. } => state.touch.push(format!("up {id}")),
            wl_touch::Event::Frame => state.touch.push("frame".into()),
            wl_touch::Event::Cancel => state.touch.push("cancel".into()),
            _ => {}
        }
    }
}

wayland_client::delegate_noop!(Wl: ignore ZwpTextInputManagerV3);

impl Dispatch<ZwpTextInputV3, ()> for Wl {
    fn event(state: &mut Self, _: &ZwpTextInputV3, event: zwp_text_input_v3::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        match event {
            zwp_text_input_v3::Event::Enter { surface } => state.ti_entered = Some(surface),
            zwp_text_input_v3::Event::Leave { .. } => state.ti_entered = None,
            zwp_text_input_v3::Event::PreeditString { text, cursor_begin, cursor_end } => {
                state.ti.push(format!("preedit {} {cursor_begin} {cursor_end}", text.unwrap_or_default()))
            }
            zwp_text_input_v3::Event::CommitString { text } => state.ti.push(format!("commit {}", text.unwrap_or_default())),
            zwp_text_input_v3::Event::DeleteSurroundingText { before_length, after_length } => {
                state.ti.push(format!("delete {before_length} {after_length}"))
            }
            zwp_text_input_v3::Event::Done { serial } => state.ti.push(format!("done {serial}")),
            _ => {}
        }
    }
}

impl Dispatch<WlPointer, ()> for Wl {
    fn event(state: &mut Self, _: &WlPointer, event: wl_pointer::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        match event {
            wl_pointer::Event::Enter { surface, surface_x, surface_y, .. } => {
                state.pointer.push(format!("enter {surface_x} {surface_y}"));
                state.pointer_surface = Some(surface);
            }
            wl_pointer::Event::Button { button, state: WEnum::Value(s), .. } => {
                state.pointer.push(format!("button {button} {}", s as u32))
            }
            wl_pointer::Event::Motion { .. } => state.pointer.push("motion".into()),
            wl_pointer::Event::Axis { axis: WEnum::Value(a), value, .. } => {
                state.pointer.push(format!("axis {} {value}", a as u32))
            }
            wl_pointer::Event::Frame => state.pointer.push("frame".into()),
            _ => {}
        }
    }
}

impl Dispatch<WlKeyboard, ()> for Wl {
    fn event(state: &mut Self, _: &WlKeyboard, event: wl_keyboard::Event, _: &(), _: &Connection, _: &QueueHandle<Self>) {
        match event {
            wl_keyboard::Event::Enter { surface, .. } => {
                state.keyboard_focus = true;
                state.keyboard_surface = Some(surface);
            }
            wl_keyboard::Event::Leave { .. } => {
                state.keyboard_focus = false;
                state.keyboard_surface = None;
            }
            wl_keyboard::Event::Key { key, state: WEnum::Value(s), .. } => {
                state.keys.push((key, s as u32));
                if s == wl_keyboard::KeyState::Pressed {
                    let keymap = state.keymaps.len().saturating_sub(1);
                    state.presses.push((key, keymap, state.mods_depressed, state.mods_locked, state.group));
                }
            }
            wl_keyboard::Event::Modifiers { mods_depressed, mods_locked, group, .. } => {
                state.mods_depressed = mods_depressed;
                state.mods_locked = mods_locked;
                state.group = group;
            }
            wl_keyboard::Event::Keymap { fd, size, .. } => {
                let mut buf = vec![0; size as usize];
                std::fs::File::from(fd).read_exact_at(&mut buf, 0).unwrap();
                let text = String::from_utf8(buf).unwrap();
                state.keymaps.push(text.trim_end_matches('\0').to_owned());
            }
            _ => {}
        }
    }
}
