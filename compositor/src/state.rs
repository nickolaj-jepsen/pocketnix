//! The compositor's state and its smithay protocol handlers.

use std::collections::HashMap;
use std::collections::HashSet;
use std::process::ExitStatus;
use std::sync::Mutex;
use std::sync::mpsc;
use std::time::Instant;

use smithay::input::Seat;
use smithay::input::SeatHandler;
use smithay::input::SeatState;
use smithay::input::pointer::CursorImageStatus;
use smithay::output::Mode;
use smithay::output::Output;
use smithay::output::PhysicalProperties;
use smithay::output::Scale;
use smithay::output::Subpixel;
use smithay::reexports::calloop::LoopHandle;
use smithay::reexports::calloop::RegistrationToken;
use smithay::reexports::calloop::channel::Sender;
use smithay::reexports::wayland_protocols::xdg::decoration::zv1::server::zxdg_toplevel_decoration_v1::Mode as DecorationMode;
use smithay::reexports::wayland_protocols::xdg::shell::server::xdg_toplevel;
use smithay::reexports::wayland_server::Client;
use smithay::reexports::wayland_server::DisplayHandle;
use smithay::reexports::wayland_server::Resource;
use smithay::reexports::wayland_server::backend::ClientData;
use smithay::reexports::wayland_server::backend::ClientId;
use smithay::reexports::wayland_server::backend::DisconnectReason;
use smithay::reexports::wayland_server::protocol::wl_buffer::WlBuffer;
use smithay::reexports::wayland_server::protocol::wl_output::WlOutput;
use smithay::reexports::wayland_server::protocol::wl_seat::WlSeat;
use smithay::reexports::wayland_server::protocol::wl_surface;
use smithay::reexports::wayland_server::protocol::wl_surface::WlSurface;
use smithay::utils::Logical;
use smithay::utils::Rectangle;
use smithay::utils::Serial;
use smithay::utils::Size;
use smithay::wayland::buffer::BufferHandler;
use smithay::wayland::compositor;
use smithay::wayland::compositor::BufferAssignment;
use smithay::wayland::compositor::CompositorClientState;
use smithay::wayland::compositor::CompositorHandler;
use smithay::wayland::compositor::CompositorState;
use smithay::wayland::compositor::Damage;
use smithay::wayland::compositor::SurfaceAttributes;
use smithay::wayland::cursor_shape::CursorShapeManagerState;
use smithay::wayland::fractional_scale::FractionalScaleHandler;
use smithay::wayland::fractional_scale::FractionalScaleManagerState;
use smithay::wayland::output::OutputHandler;
use smithay::wayland::output::OutputManagerState;
use smithay::wayland::selection::SelectionHandler;
use smithay::wayland::selection::data_device::ClientDndGrabHandler;
use smithay::wayland::selection::data_device::DataDeviceHandler;
use smithay::wayland::selection::data_device::DataDeviceState;
use smithay::wayland::selection::data_device::ServerDndGrabHandler;
use smithay::wayland::selection::data_device::set_data_device_focus;
use smithay::wayland::selection::primary_selection::PrimarySelectionHandler;
use smithay::wayland::selection::primary_selection::PrimarySelectionState;
use smithay::wayland::selection::primary_selection::set_primary_focus;
use smithay::wayland::selection::{ext_data_control, wlr_data_control};
use smithay::wayland::shell::xdg::PopupSurface;
use smithay::wayland::shell::xdg::PositionerState;
use smithay::wayland::shell::xdg::SurfaceCachedState;
use smithay::wayland::shell::xdg::ToplevelState;
use smithay::wayland::shell::xdg::ToplevelSurface;
use smithay::wayland::shell::xdg::XdgPopupSurfaceData;
use smithay::wayland::shell::xdg::XdgShellHandler;
use smithay::wayland::shell::xdg::XdgShellState;
use smithay::wayland::shell::xdg::XdgToplevelSurfaceData;
use smithay::wayland::shell::xdg::decoration::XdgDecorationHandler;
use smithay::wayland::shell::xdg::decoration::XdgDecorationState;
use smithay::wayland::shell::xdg::dialog::XdgDialogHandler;
use smithay::wayland::shell::xdg::dialog::XdgDialogState;
use smithay::wayland::shm;
use smithay::wayland::shm::ShmHandler;
use smithay::wayland::shm::ShmState;
use smithay::wayland::tablet_manager::TabletSeatHandler;
use smithay::wayland::viewporter::ViewportCachedState;
use smithay::wayland::viewporter::ViewporterState;
use smithay::wayland::xdg_activation::XdgActivationState;
use smithay::wayland::xdg_foreign::XdgForeignHandler;
use smithay::wayland::xdg_foreign::XdgForeignState;
use tracing::warn;

use crate::apps::AppList;
use crate::apps::Env;
use crate::damage::Viewport;
use crate::damage::surface_damage_to_buffer;
use crate::flush::Layer;
use crate::gui::Gui;
use crate::icons::HashCache;
use crate::launch::Launch;
use crate::launcher::IconJob;
use crate::launcher::InputSerials;
use crate::popup::solve_in_toplevel;
use crate::protocol;
use crate::protocol::Rect;
use crate::surface::Surf;
use crate::surface::SurfCell;
use crate::surface::wire_format;
use crate::surface::with_surf;
use crate::text_input::TextInputs;
use crate::touch::PerPointGrab;
use crate::touch::Touch;
use crate::watch::Inotify;
use crate::windows::Dialog;
use crate::windows::Drag;
use crate::windows::Meta;
use crate::windows::Tree;

/// The display a new window opens on, from the app's `output` message.
#[derive(Clone, Copy, Debug)]
pub struct OutputCfg {
    pub width: i32,
    pub height: i32,
    pub scale: f64,
    pub refresh: i32,
}

impl OutputCfg {
    /// The output's size in logical pixels of a window at `scale`.
    pub fn logical_size_at(&self, scale: f64) -> Size<i32, Logical> {
        Size::from((
            (self.width as f64 / scale).round() as i32,
            (self.height as f64 / scale).round() as i32,
        ))
    }
}

/// The size factors new windows open at, from the app's `settings`.
#[derive(Clone, Debug)]
pub struct Settings {
    pub size: f64,
    pub apps: HashMap<String, f64>,
}

impl Default for Settings {
    fn default() -> Self {
        Self { size: 1.0, apps: HashMap::new() }
    }
}

/// The app's last `window` message for a toplevel.
#[derive(Clone, Copy, Debug)]
pub struct WindowCfg {
    pub width: u32,
    pub height: u32,
    pub scale: f64,
    pub focused: bool,
    pub visible: bool,
    pub maximized: bool,
    pub fullscreen: bool,
}

pub struct State {
    pub dh: DisplayHandle,
    pub lh: LoopHandle<'static, State>,
    pub start: Instant,

    pub compositor_state: CompositorState,
    pub xdg_shell_state: XdgShellState,
    pub shm_state: ShmState,
    pub seat_state: SeatState<State>,
    pub data_device_state: DataDeviceState,
    pub primary_selection_state: PrimarySelectionState,
    pub ext_data_control_state: ext_data_control::DataControlState,
    pub wlr_data_control_state: wlr_data_control::DataControlState,
    pub xdg_activation_state: XdgActivationState,
    pub xdg_foreign_state: XdgForeignState,
    /// Globals no handler asks for; they live as long as the compositor.
    _globals: (
        XdgDecorationState,
        ViewporterState,
        FractionalScaleManagerState,
        OutputManagerState,
        CursorShapeManagerState,
        XdgDialogState,
    ),

    pub seat: Seat<State>,
    pub output: Output,
    pub output_cfg: OutputCfg,
    pub settings: Settings,
    /// The XKB layout and variant from the app's `keyboard-layout`; empty
    /// for the compositor's own default.
    pub keyboard_layout: (String, String),
    next_surface_id: u32,

    pub gui: Gui,
    /// Per window id, from the app.
    pub windows: HashMap<u32, WindowCfg>,
    /// Per toplevel id, the scale it was first configured at, until the
    /// app's first `window` for it.
    pub initial_scales: HashMap<u32, f64>,
    /// Per window id, the surfaces as the last flush laid them out.
    pub layouts: HashMap<u32, Vec<Layer>>,
    /// Frame callbacks sent to the app with a commit, per window id, with
    /// the seq the app must present before they fire.
    pub held: HashMap<u32, Vec<(u32, smithay::reexports::wayland_server::protocol::wl_callback::WlCallback)>>,
    /// Surfaces that committed frame callbacks not yet sent or fired.
    pub cb_surfaces: Vec<WlSurface>,
    /// The integer wl_output scale each client was last sent.
    pub client_scales: HashMap<ClientId, i32>,

    pub grabs: Vec<PopupSurface>,
    pub pressed_keys: HashSet<u32>,
    pub pressed_buttons: HashSet<u32>,
    /// Buttons whose press dismissed a popup grab: their release goes nowhere.
    pub swallowed_buttons: HashSet<u32>,
    pub dnd_active: bool,
    pub pointer_window: Option<u32>,
    /// The window and cursor name last reported to the app.
    pub cursor: (u32, String),

    pub tree: Tree,
    pub meta: HashMap<u32, Meta>,
    pub dialogs: HashMap<u32, Dialog>,
    /// Per window id, its focused toplevel.
    pub focus: HashMap<u32, u32>,
    pub raise_seq: u64,
    pub drag: Option<Drag>,
    /// `POCKETNIX_TAB` per client, read once.
    pub client_tabs: HashMap<ClientId, Option<String>>,
    pub input_serials: InputSerials,
    pub touch: Touch,
    pub ti: TextInputs,

    pub env: Env,
    pub apps: AppList,
    pub hashes: HashCache,
    pub inotify: Option<Inotify>,
    pub rescan_timer: Option<RegistrationToken>,
    pub icon_jobs: mpsc::Sender<IconJob>,
    pub launches: HashMap<String, Launch>,
    /// Toplevels activated with a pending launch's token before mapping.
    pub activations: HashMap<u32, String>,
    pub launch_exits: Sender<(String, ExitStatus)>,
}

/// Where the state hands work off its event loop: icons to render, and
/// launched processes to wait for.
pub struct Channels {
    pub icon_jobs: mpsc::Sender<IconJob>,
    pub launch_exits: Sender<(String, ExitStatus)>,
}

impl State {
    pub fn new(dh: &DisplayHandle, lh: LoopHandle<'static, State>, gui: Gui, channels: Channels) -> anyhow::Result<Self> {
        let mut seat_state = SeatState::new();
        let mut seat = seat_state.new_wl_seat(dh, "seat0");
        // An empty XkbConfig lets libxkbcommon read XKB_DEFAULT_LAYOUT and friends.
        seat.add_keyboard(Default::default(), 400, 25)
            .map_err(|e| anyhow::anyhow!("keyboard: {e:?}"))?;
        seat.add_pointer();
        seat.add_touch_with_default_grab(|| Box::new(PerPointGrab::new()));
        crate::text_input::create_global(dh);

        let primary_selection_state = PrimarySelectionState::new::<Self>(dh);
        // Clipboard tools (wl-copy, the guest's clipboard bridge) need no surface with these.
        let ext_data_control_state =
            ext_data_control::DataControlState::new::<Self, _>(dh, Some(&primary_selection_state), |_| true);
        let wlr_data_control_state =
            wlr_data_control::DataControlState::new::<Self, _>(dh, Some(&primary_selection_state), |_| true);

        let output = Output::new(
            "pocketnix".into(),
            PhysicalProperties {
                size: (0, 0).into(),
                subpixel: Subpixel::Unknown,
                make: "pocketnix".into(),
                model: "Android".into(),
            },
        );
        output.create_global::<Self>(dh);
        // Before the app's first `output` message.
        let output_cfg = OutputCfg {
            width: 1280,
            height: 720,
            scale: 1.0,
            refresh: 60_000,
        };

        let mut state = Self {
            dh: dh.clone(),
            lh,
            start: Instant::now(),
            compositor_state: CompositorState::new::<Self>(dh),
            xdg_shell_state: XdgShellState::new::<Self>(dh),
            shm_state: ShmState::new::<Self>(dh, Vec::new()),
            seat_state,
            data_device_state: DataDeviceState::new::<Self>(dh),
            primary_selection_state,
            ext_data_control_state,
            wlr_data_control_state,
            xdg_activation_state: XdgActivationState::new::<Self>(dh),
            xdg_foreign_state: XdgForeignState::new::<Self>(dh),
            _globals: (
                XdgDecorationState::new::<Self>(dh),
                ViewporterState::new::<Self>(dh),
                FractionalScaleManagerState::new::<Self>(dh),
                OutputManagerState::new_with_xdg_output::<Self>(dh),
                CursorShapeManagerState::new::<Self>(dh),
                XdgDialogState::new::<Self>(dh),
            ),
            seat,
            output,
            output_cfg,
            settings: Settings::default(),
            keyboard_layout: Default::default(),
            next_surface_id: 1,
            gui,
            windows: HashMap::new(),
            initial_scales: HashMap::new(),
            layouts: HashMap::new(),
            held: HashMap::new(),
            cb_surfaces: Vec::new(),
            client_scales: HashMap::new(),
            grabs: Vec::new(),
            pressed_keys: HashSet::new(),
            pressed_buttons: HashSet::new(),
            swallowed_buttons: HashSet::new(),
            dnd_active: false,
            pointer_window: None,
            cursor: (0, "default".into()),
            tree: Tree::default(),
            meta: HashMap::new(),
            dialogs: HashMap::new(),
            focus: HashMap::new(),
            raise_seq: 0,
            drag: None,
            client_tabs: HashMap::new(),
            input_serials: InputSerials::default(),
            touch: Touch::default(),
            ti: TextInputs::default(),
            env: Env::from_process(),
            apps: AppList::default(),
            hashes: HashCache::default(),
            inotify: None,
            rescan_timer: None,
            icon_jobs: channels.icon_jobs,
            launches: HashMap::new(),
            activations: HashMap::new(),
            launch_exits: channels.launch_exits,
        };
        state.apply_output(output_cfg);
        Ok(state)
    }

    pub fn time_ms(&self) -> u32 {
        self.start.elapsed().as_millis() as u32
    }

    pub fn apply_output(&mut self, cfg: OutputCfg) {
        self.output_cfg = cfg;
        let mode = Mode {
            size: (cfg.width, cfg.height).into(),
            refresh: cfg.refresh,
        };
        if let Some(old) = self.output.current_mode().filter(|m| *m != mode) {
            self.output.delete_mode(old);
        }
        self.output.change_current_state(
            Some(mode),
            None,
            Some(Scale::Custom {
                advertised_integer: cfg.scale.ceil().max(1.0) as i32,
                fractional: cfg.scale,
            }),
            Some((0, 0).into()),
        );
        self.output.set_preferred(mode);
        // The change re-sent every client the output's scale; the next
        // flush sends each its own again.
        self.client_scales.clear();
    }

    /// The scale a window's surfaces render at: the app's, or until the app
    /// has said, the one it was first configured at, or the output's.
    pub fn window_scale(&self, window: u32) -> f64 {
        self.windows
            .get(&window)
            .map(|w| w.scale)
            .or_else(|| self.initial_scales.get(&window).copied())
            .filter(|s| *s > 0.0)
            .unwrap_or(self.output_cfg.scale)
    }

    pub fn toplevel(&self, surface: &WlSurface) -> Option<ToplevelSurface> {
        self.xdg_shell_state
            .toplevel_surfaces()
            .iter()
            .find(|t| t.wl_surface() == surface)
            .cloned()
    }

    pub fn toplevel_by_id(&self, id: u32) -> Option<ToplevelSurface> {
        self.xdg_shell_state
            .toplevel_surfaces()
            .iter()
            .find(|t| surface_id(t.wl_surface()) == Some(id))
            .cloned()
    }

    pub fn popup(&self, surface: &WlSurface) -> Option<PopupSurface> {
        self.xdg_shell_state
            .popup_surfaces()
            .iter()
            .find(|p| p.wl_surface() == surface)
            .cloned()
    }

    fn initial_configure(&mut self, toplevel: &ToplevelSurface) {
        let id = surface_id(toplevel.wl_surface()).unwrap_or(0);
        let cfg = self.windows.get(&id).copied();
        let parent = toplevel.parent();
        if cfg.is_none() {
            // A window opens at its entry's size factor, a dialog at its window's scale.
            let scale = match &parent {
                Some(p) => self.window_of(p).map_or(self.output_cfg.scale, |w| self.window_scale(w)),
                None => {
                    let entry = self.entry_for_new(id, toplevel);
                    let factor = entry.and_then(|e| self.settings.apps.get(&e).copied()).unwrap_or(self.settings.size);
                    self.output_cfg.scale * factor
                }
            };
            self.initial_scales.insert(id, scale);
            self.apply_scale(toplevel.wl_surface(), scale, true);
        }
        let output = self.output_cfg.logical_size_at(self.window_scale(id));
        let has_parent = parent.is_some();
        let bounds = parent.and_then(|p| self.dialog_bounds(&p)).unwrap_or(output);
        toplevel.with_pending_state(|s| {
            s.bounds = Some(bounds);
            match cfg {
                Some(cfg) => apply_window_cfg(s, &cfg, true),
                // Dialogs pick their own size.
                None if !has_parent => s.size = Some(output),
                None => {}
            }
        });
        toplevel.send_configure();
    }

    /// Sends the app a JSON message, if one is connected.
    pub fn send_json(&mut self, value: serde_json::Value) {
        if let Some(conn) = self.gui.active_conn() {
            conn.send(crate::protocol::json_frame(&value));
        }
    }

    fn toplevel_request(&mut self, toplevel: &ToplevelSurface, request: &'static str) {
        let Some(id) = surface_id(toplevel.wl_surface()) else { return };
        match self.gui.active_conn().and_then(|c| c.toplevels.get(&id)) {
            Some((w, _)) => {
                if *w == id {
                    self.send_json(serde_json::json!({"type": "toplevel-request", "id": id, "request": request}));
                }
            }
            // Asked before the app knew the window, as a client starting full screen does before its first commit.
            None => {
                let opposite = match request {
                    "fullscreen" => "unfullscreen",
                    "unfullscreen" => "fullscreen",
                    "maximize" => "unmaximize",
                    "unmaximize" => "maximize",
                    _ => return,
                };
                let early = &mut self.meta.entry(id).or_default().early;
                early.retain(|r| *r != request && *r != opposite);
                early.push(request);
            }
        }
    }

    /// Moves keyboard focus, first releasing the keys held in the window it
    /// leaves.
    pub fn set_keyboard_focus(&mut self, target: Option<WlSurface>) {
        let Some(keyboard) = self.seat.get_keyboard() else { return };
        let current = keyboard.current_focus();
        if current == target {
            return;
        }
        let old_window = current.as_ref().and_then(|s| self.window_of(s));
        let new_window = target.as_ref().and_then(|s| self.window_of(s));
        if old_window != new_window {
            let time = self.time_ms();
            for key in std::mem::take(&mut self.pressed_keys) {
                keyboard.input::<(), _>(
                    self,
                    (key + 8).into(),
                    smithay::backend::input::KeyState::Released,
                    smithay::utils::SERIAL_COUNTER.next_serial(),
                    time,
                    |_, _, _| smithay::input::keyboard::FilterResult::Forward,
                );
            }
        }
        keyboard.set_focus(self, target, smithay::utils::SERIAL_COUNTER.next_serial());
    }
}

/// A window's state from the app's `window` message; `activated` only while
/// the window's own toplevel has its focus.
pub fn apply_window_cfg(s: &mut ToplevelState, cfg: &WindowCfg, has_focus: bool) {
    let scale = if cfg.scale > 0.0 { cfg.scale } else { 1.0 };
    s.size = (cfg.width > 0 && cfg.height > 0).then(|| {
        Size::from((
            (cfg.width as f64 / scale).round().max(1.0) as i32,
            (cfg.height as f64 / scale).round().max(1.0) as i32,
        ))
    });
    for (state, on) in [
        (xdg_toplevel::State::Activated, cfg.focused && has_focus),
        (xdg_toplevel::State::Suspended, !cfg.visible),
        (xdg_toplevel::State::Maximized, cfg.maximized),
        (xdg_toplevel::State::Fullscreen, cfg.fullscreen),
    ] {
        if on {
            s.states.set(state);
        } else {
            s.states.unset(state);
        }
    }
}

pub fn surface_id(surface: &WlSurface) -> Option<u32> {
    if !surface.is_alive() {
        return None;
    }
    compositor::with_states(surface, |data| with_surf(data, |s| s.id))
}

/// The root surface of the toplevel `surface` belongs to: its own, its
/// subsurfaces' and the popups it opened.
pub fn toplevel_root(surface: &WlSurface) -> Option<WlSurface> {
    let mut s = surface.clone();
    for _ in 0..64 {
        if !s.is_alive() {
            return None;
        }
        while let Some(parent) = compositor::get_parent(&s) {
            s = parent;
        }
        enum Up {
            Root,
            Parent(WlSurface),
            None,
        }
        let up = compositor::with_states(&s, |data| match data.role {
            Some("xdg_toplevel") => Up::Root,
            Some("xdg_popup") => data
                .data_map
                .get::<XdgPopupSurfaceData>()
                .and_then(|p| p.lock().unwrap().parent.clone())
                .map_or(Up::None, Up::Parent),
            _ => Up::None,
        });
        match up {
            Up::Root => return Some(s),
            Up::Parent(p) => s = p,
            Up::None => return None,
        }
    }
    None
}

/// The part of a window Android shows, logical: the client may have drawn
/// smaller than its configured size (foot's cell grid) or larger (GTK's
/// minimum size).
pub fn window_area(root: &WlSurface) -> Size<i32, Logical> {
    let drawn = window_geometry(root).size;
    let configured = compositor::with_states(root, |states| {
        let data = states.data_map.get::<XdgToplevelSurfaceData>()?;
        data.lock().unwrap().current.size
    });
    match configured {
        Some(c) if c.w > 0 && c.h > 0 => (drawn.w.min(c.w), drawn.h.min(c.h)).into(),
        _ => drawn,
    }
}

/// The window geometry of an xdg surface, relative to the surface: the
/// client's, or the whole surface when it set none.
pub fn window_geometry(surface: &WlSurface) -> Rectangle<i32, Logical> {
    compositor::with_states(surface, |data| {
        let geometry = data.cached_state.get::<SurfaceCachedState>().current().geometry;
        geometry.unwrap_or_else(|| {
            let size = with_surf(data, |s| s.logical_size()).unwrap_or_default();
            Rectangle::from_size(size)
        })
    })
}

impl CompositorHandler for State {
    fn compositor_state(&mut self) -> &mut CompositorState {
        &mut self.compositor_state
    }

    fn client_compositor_state<'a>(&self, client: &'a Client) -> &'a CompositorClientState {
        &client.get_data::<ClientState>().unwrap().compositor_state
    }

    fn new_surface(&mut self, surface: &WlSurface) {
        let id = self.next_surface_id;
        self.next_surface_id = self.next_surface_id.checked_add(1).expect("surface ids exhausted");
        compositor::with_states(surface, |data| {
            data.data_map.insert_if_missing_threadsafe(|| SurfCell(Mutex::new(Surf::new(id))));
        });
    }

    fn commit(&mut self, surface: &WlSurface) {
        if let Some(toplevel) = self.toplevel(surface) {
            if !toplevel.is_initial_configure_sent() {
                self.initial_configure(&toplevel);
            }
        } else if let Some(popup) = self.popup(surface)
            && !popup.is_initial_configure_sent()
            && let Err(err) = popup.send_configure()
        {
            warn!("popup configure: {err}");
        }

        let generation = self.gui.generation;
        let (removed, has_callbacks, entered, refused) = compositor::with_states(surface, |data| {
            let mut guard = data.cached_state.get::<SurfaceAttributes>();
            let attrs = guard.current();
            let buffer = attrs.buffer.take();
            let damage = std::mem::take(&mut attrs.damage);
            let callbacks = std::mem::take(&mut attrs.frame_callbacks);
            let scale = attrs.buffer_scale;
            let transform = attrs.buffer_transform.into();
            drop(guard);
            let viewport = {
                let mut guard = data.cached_state.get::<ViewportCachedState>();
                let current = guard.current();
                Viewport {
                    src: current.src,
                    dst: current.dst,
                }
            };
            with_surf(data, |s| {
                s.buffer_scale = scale;
                s.transform = transform;
                s.viewport = viewport;
                let mut removed = false;
                let mut refused = None;
                match buffer {
                    Some(BufferAssignment::NewBuffer(buffer)) => {
                        refused = take_buffer(s, &buffer, &damage, generation).err();
                        buffer.release();
                    }
                    Some(BufferAssignment::Removed) => {
                        s.remove_buffer();
                        removed = true;
                    }
                    None => {}
                }
                s.callbacks.extend(callbacks);
                let entered = std::mem::replace(&mut s.entered_output, true);
                (removed, !s.callbacks.is_empty(), entered, refused)
            })
            .unwrap_or((false, false, true, None))
        });
        if let Some(why) = refused {
            surface.post_error(wl_surface::Error::InvalidSize, why);
            return;
        }
        if !entered {
            self.output.enter(surface);
        }
        if has_callbacks && !self.cb_surfaces.contains(surface) {
            self.cb_surfaces.push(surface.clone());
        }
        // An unmapped toplevel starts over with a new initial configure.
        if removed && let Some(toplevel) = self.toplevel(surface) {
            toplevel.reset_initial_configure_sent();
        }
    }
}

/// Takes a committed buffer into the surface's mirror. Err for one larger
/// than the app takes, which would fail every replay: the client gets a
/// protocol error instead.
fn take_buffer(s: &mut Surf, buffer: &WlBuffer, damage: &[Damage], generation: u64) -> Result<(), String> {
    let app_has = s.app_has_content(generation);
    let result = shm::with_buffer_contents(buffer, |ptr, len, spec| {
        let Some(format) = wire_format(spec.format) else {
            warn!("unsupported shm format {:?}", spec.format);
            return Ok(());
        };
        let (width, height, stride) = (spec.width, spec.height, spec.stride);
        if width <= 0 || height <= 0 || stride < width * 4 {
            return Ok(());
        }
        if width as u32 > protocol::MAX_DIMENSION
            || height as u32 > protocol::MAX_DIMENSION
            || width as u64 * height as u64 * 4 > protocol::MAX_BUFFER_BYTES
        {
            return Err(format!(
                "buffer {width}x{height} is over {max} pixels a side or {bytes} bytes",
                max = protocol::MAX_DIMENSION,
                bytes = protocol::MAX_BUFFER_BYTES,
            ));
        }
        let start = spec.offset as usize;
        let size = stride as usize * height as usize;
        if start + size > len {
            return Ok(());
        }
        // SAFETY: smithay keeps the pool mapped for this closure and we
        // checked the range against its length.
        let src = unsafe { std::slice::from_raw_parts(ptr.add(start), size) };
        let rects = damage
            .iter()
            .map(|d| match d {
                Damage::Buffer(r) => *r,
                Damage::Surface(r) => {
                    surface_damage_to_buffer(*r, s.buffer_scale, s.transform, s.viewport, Some((width, height)))
                }
            })
            .filter_map(|r| {
                let x0 = r.loc.x.max(0);
                let y0 = r.loc.y.max(0);
                let x1 = r.loc.x.saturating_add(r.size.w);
                let y1 = r.loc.y.saturating_add(r.size.h);
                (x1 > x0 && y1 > y0).then(|| Rect {
                    x: x0 as u32,
                    y: y0 as u32,
                    w: (x1 - x0) as u32,
                    h: (y1 - y0) as u32,
                })
            })
            .collect();
        s.apply_buffer(src, width as u32, height as u32, stride as usize, format, rects, app_has);
        Ok(())
    });
    result.unwrap_or_else(|err| {
        warn!("buffer contents: {err:?}");
        Ok(())
    })
}

impl BufferHandler for State {
    fn buffer_destroyed(&mut self, _buffer: &WlBuffer) {}
}

impl ShmHandler for State {
    fn shm_state(&self) -> &ShmState {
        &self.shm_state
    }
}

impl XdgShellHandler for State {
    fn xdg_shell_state(&mut self) -> &mut XdgShellState {
        &mut self.xdg_shell_state
    }

    fn new_toplevel(&mut self, _surface: ToplevelSurface) {}

    fn new_popup(&mut self, surface: PopupSurface, positioner: PositionerState) {
        let (positioner, geometry) = solve_in_toplevel(surface.get_parent_surface(), positioner, |r| self.popup_space(r));
        surface.with_pending_state(|s| {
            s.positioner = positioner;
            s.geometry = geometry;
        });
    }

    fn grab(&mut self, surface: PopupSurface, _seat: WlSeat, _serial: Serial) {
        if !self.grabs.contains(&surface) {
            self.grabs.push(surface.clone());
        }
        let keyboard_window = self.keyboard_window();
        if keyboard_window.is_some() && keyboard_window == self.window_of(surface.wl_surface()) {
            self.set_keyboard_focus(Some(surface.wl_surface().clone()));
        }
    }

    fn reposition_request(&mut self, surface: PopupSurface, positioner: PositionerState, token: u32) {
        let (positioner, geometry) = solve_in_toplevel(surface.get_parent_surface(), positioner, |r| self.popup_space(r));
        surface.with_pending_state(|s| {
            s.positioner = positioner;
            s.geometry = geometry;
        });
        surface.send_repositioned(token);
    }

    fn popup_destroyed(&mut self, surface: PopupSurface) {
        self.grabs.retain(|p| p != &surface && p.alive());
        let focused = self.seat.get_keyboard().and_then(|k| k.current_focus());
        if focused.as_ref() == Some(surface.wl_surface()) {
            let target = surface
                .get_parent_surface()
                .and_then(|p| self.window_of(&p))
                .and_then(|w| self.focus_target(w));
            self.set_keyboard_focus(target);
        }
    }

    fn toplevel_destroyed(&mut self, surface: ToplevelSurface) {
        let focused = self.seat.get_keyboard().and_then(|k| k.current_focus());
        if focused.as_ref() == Some(surface.wl_surface()) {
            self.set_keyboard_focus(None);
        }
        if let Some(id) = surface_id(surface.wl_surface()) {
            self.forget_toplevel(id);
        }
    }

    fn maximize_request(&mut self, surface: ToplevelSurface) {
        self.toplevel_request(&surface, "maximize");
        surface.send_configure();
    }

    fn unmaximize_request(&mut self, surface: ToplevelSurface) {
        self.toplevel_request(&surface, "unmaximize");
        surface.send_configure();
    }

    fn fullscreen_request(&mut self, surface: ToplevelSurface, _output: Option<WlOutput>) {
        self.toplevel_request(&surface, "fullscreen");
        surface.send_configure();
    }

    fn unfullscreen_request(&mut self, surface: ToplevelSurface) {
        self.toplevel_request(&surface, "unfullscreen");
        surface.send_configure();
    }

    fn minimize_request(&mut self, surface: ToplevelSurface) {
        self.toplevel_request(&surface, "minimize");
    }

    fn move_request(&mut self, surface: ToplevelSurface, _seat: WlSeat, serial: Serial) {
        let Some(id) = surface_id(surface.wl_surface()) else { return };
        if !self.is_dialog(id) {
            self.toplevel_request(&surface, "move");
            return;
        }
        // A dialog follows the pointer while the button that started it is
        // held, or the touch point whose down it names until it lifts.
        let Some((window, parent)) = self.tree.of.get(&id).copied() else { return };
        let (touch, start) = match self.touch_point_by_serial(serial) {
            Some((point, at)) if point.0 == window => (Some(point), at),
            _ => {
                let Some(pointer) = self.seat.get_pointer() else { return };
                if self.pressed_buttons.is_empty() || self.pointer_window != Some(window) {
                    return;
                }
                (None, pointer.current_location())
            }
        };
        let origin = |id| self.tree.origin.get(&id).copied().unwrap_or_default();
        self.drag = Some(Drag {
            dialog: id,
            window,
            touch,
            start,
            start_offset: origin(id) - origin(parent),
        });
    }

    fn resize_request(
        &mut self,
        surface: ToplevelSurface,
        _seat: WlSeat,
        _serial: Serial,
        _edges: xdg_toplevel::ResizeEdge,
    ) {
        self.toplevel_request(&surface, "resize");
    }
}

impl XdgDecorationHandler for State {
    fn new_decoration(&mut self, toplevel: ToplevelSurface) {
        set_decoration(&toplevel, DecorationMode::ServerSide);
    }

    fn request_mode(&mut self, toplevel: ToplevelSurface, mode: DecorationMode) {
        set_decoration(&toplevel, mode);
    }

    fn unset_mode(&mut self, toplevel: ToplevelSurface) {
        set_decoration(&toplevel, DecorationMode::ServerSide);
    }
}

fn set_decoration(toplevel: &ToplevelSurface, mode: DecorationMode) {
    toplevel.with_pending_state(|s| s.decoration_mode = Some(mode));
    if toplevel.is_initial_configure_sent() {
        toplevel.send_pending_configure();
    }
}

impl SeatHandler for State {
    type KeyboardFocus = WlSurface;
    type PointerFocus = WlSurface;
    type TouchFocus = WlSurface;

    fn seat_state(&mut self) -> &mut SeatState<Self> {
        &mut self.seat_state
    }

    fn focus_changed(&mut self, seat: &Seat<Self>, focused: Option<&WlSurface>) {
        let client = focused.and_then(|s| self.dh.get_client(s.id()).ok());
        set_data_device_focus(&self.dh, seat, client.clone());
        set_primary_focus(&self.dh, seat, client);
        self.text_input_focus(focused.cloned());
    }

    fn cursor_image(&mut self, _seat: &Seat<Self>, image: CursorImageStatus) {
        let name = match image {
            CursorImageStatus::Hidden => "hidden".to_string(),
            CursorImageStatus::Named(icon) => icon.name().to_string(),
            // CURSOR_IMAGE is reserved, so a client-drawn cursor reports as the default.
            CursorImageStatus::Surface(_) => "default".to_string(),
        };
        let window = self.pointer_window.unwrap_or(0);
        if self.cursor != (window, name.clone()) {
            self.cursor = (window, name.clone());
            self.send_json(serde_json::json!({"type": "cursor", "window": window, "name": name}));
        }
    }
}

impl TabletSeatHandler for State {}

impl SelectionHandler for State {
    type SelectionUserData = ();
}

impl DataDeviceHandler for State {
    fn data_device_state(&self) -> &DataDeviceState {
        &self.data_device_state
    }
}

impl ClientDndGrabHandler for State {
    fn started(&mut self, _source: Option<smithay::reexports::wayland_server::protocol::wl_data_source::WlDataSource>, _icon: Option<WlSurface>, _seat: Seat<Self>) {
        self.dnd_active = true;
    }

    fn dropped(&mut self, _target: Option<WlSurface>, _validated: bool, _seat: Seat<Self>) {
        self.dnd_active = false;
    }
}

impl ServerDndGrabHandler for State {}

impl PrimarySelectionHandler for State {
    fn primary_selection_state(&self) -> &PrimarySelectionState {
        &self.primary_selection_state
    }
}

impl ext_data_control::DataControlHandler for State {
    fn data_control_state(&self) -> &ext_data_control::DataControlState {
        &self.ext_data_control_state
    }
}

impl wlr_data_control::DataControlHandler for State {
    fn data_control_state(&self) -> &wlr_data_control::DataControlState {
        &self.wlr_data_control_state
    }
}

impl OutputHandler for State {
    fn output_bound(&mut self, _output: Output, wl_output: WlOutput) {
        let Some(client) = wl_output.client() else { return };
        if let Some(scale) = self.client_scales.get(&client.id())
            && wl_output.version() >= 2
        {
            wl_output.scale(*scale);
            wl_output.done();
        }
    }
}

impl FractionalScaleHandler for State {
    fn new_fractional_scale(&mut self, surface: WlSurface) {
        let scale = self.window_of(&surface).map_or(self.output_cfg.scale, |w| self.window_scale(w));
        compositor::with_states(&surface, |data| {
            smithay::wayland::fractional_scale::with_fractional_scale(data, |fs| fs.set_preferred_scale(scale));
            with_surf(data, |s| s.applied_scale = Some(scale));
        });
    }
}

impl XdgDialogHandler for State {}

impl XdgForeignHandler for State {
    fn xdg_foreign_state(&mut self) -> &mut XdgForeignState {
        &mut self.xdg_foreign_state
    }
}

pub struct ClientState {
    pub compositor_state: CompositorClientState,
}

impl ClientData for ClientState {
    fn initialized(&self, _client_id: ClientId) {}
    fn disconnected(&self, _client_id: ClientId, _reason: DisconnectReason) {}
}

smithay::delegate_compositor!(State);
smithay::delegate_xdg_shell!(State);
smithay::delegate_xdg_decoration!(State);
smithay::delegate_shm!(State);
// The seat as smithay::delegate_seat! has it, but for wl_seat and wl_touch,
// which crate::touch dispatches to count each client's wl_touch.
smithay::reexports::wayland_server::delegate_global_dispatch!(State: [WlSeat: smithay::wayland::seat::SeatGlobalData<State>] => SeatState<State>);
smithay::reexports::wayland_server::delegate_dispatch!(State: [smithay::reexports::wayland_server::protocol::wl_pointer::WlPointer: smithay::wayland::seat::PointerUserData<State>] => SeatState<State>);
smithay::reexports::wayland_server::delegate_dispatch!(State: [smithay::reexports::wayland_server::protocol::wl_keyboard::WlKeyboard: smithay::wayland::seat::KeyboardUserData<State>] => SeatState<State>);
smithay::delegate_data_device!(State);
smithay::delegate_primary_selection!(State);
smithay::delegate_ext_data_control!(State);
smithay::delegate_data_control!(State);
smithay::delegate_output!(State);
smithay::delegate_viewporter!(State);
smithay::delegate_fractional_scale!(State);
smithay::delegate_xdg_activation!(State);
smithay::delegate_cursor_shape!(State);
smithay::delegate_xdg_dialog!(State);
smithay::delegate_xdg_foreign!(State);
