//! The compositor's state and its smithay protocol handlers.

use std::collections::HashMap;
use std::collections::HashSet;
use std::sync::Mutex;
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
use smithay::wayland::shell::xdg::decoration::XdgDecorationHandler;
use smithay::wayland::shell::xdg::decoration::XdgDecorationState;
use smithay::wayland::shm;
use smithay::wayland::shm::ShmHandler;
use smithay::wayland::shm::ShmState;
use smithay::wayland::tablet_manager::TabletSeatHandler;
use smithay::wayland::viewporter::ViewportCachedState;
use smithay::wayland::viewporter::ViewporterState;
use smithay::wayland::xdg_activation::XdgActivationHandler;
use smithay::wayland::xdg_activation::XdgActivationState;
use smithay::wayland::xdg_activation::XdgActivationToken;
use smithay::wayland::xdg_activation::XdgActivationTokenData;
use tracing::debug;
use tracing::warn;

use crate::damage::Viewport;
use crate::damage::surface_damage_to_buffer;
use crate::flush::Layer;
use crate::gui::Gui;
use crate::popup::solve_in_toplevel;
use crate::protocol::Rect;
use crate::surface::Surf;
use crate::surface::SurfCell;
use crate::surface::wire_format;
use crate::surface::with_surf;

/// The display a new window opens on, from the app's `output` message.
#[derive(Clone, Copy, Debug)]
pub struct OutputCfg {
    pub width: i32,
    pub height: i32,
    pub scale: f64,
    pub refresh: i32,
}

impl OutputCfg {
    pub fn logical_size(&self) -> Size<i32, Logical> {
        Size::from((
            (self.width as f64 / self.scale).round() as i32,
            (self.height as f64 / self.scale).round() as i32,
        ))
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
    /// Globals no handler asks for; they live as long as the compositor.
    _globals: (
        XdgDecorationState,
        ViewporterState,
        FractionalScaleManagerState,
        OutputManagerState,
        CursorShapeManagerState,
    ),

    pub seat: Seat<State>,
    pub output: Output,
    pub output_cfg: OutputCfg,
    next_surface_id: u32,

    pub gui: Gui,
    /// Per window id, from the app.
    pub windows: HashMap<u32, WindowCfg>,
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
}

impl State {
    pub fn new(dh: &DisplayHandle, lh: LoopHandle<'static, State>, gui: Gui) -> anyhow::Result<Self> {
        let mut seat_state = SeatState::new();
        let mut seat = seat_state.new_wl_seat(dh, "seat0");
        // An empty XkbConfig lets libxkbcommon read XKB_DEFAULT_LAYOUT and friends.
        seat.add_keyboard(Default::default(), 400, 25)
            .map_err(|e| anyhow::anyhow!("keyboard: {e:?}"))?;
        seat.add_pointer();

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
            _globals: (
                XdgDecorationState::new::<Self>(dh),
                ViewporterState::new::<Self>(dh),
                FractionalScaleManagerState::new::<Self>(dh),
                OutputManagerState::new_with_xdg_output::<Self>(dh),
                CursorShapeManagerState::new::<Self>(dh),
            ),
            seat,
            output,
            output_cfg,
            next_surface_id: 1,
            gui,
            windows: HashMap::new(),
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

    /// The scale a window's surfaces render at: the app's, or the output's
    /// until the app has said.
    pub fn window_scale(&self, window: u32) -> f64 {
        self.windows
            .get(&window)
            .map(|w| w.scale)
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

    fn initial_configure(&self, toplevel: &ToplevelSurface) {
        let id = surface_id(toplevel.wl_surface()).unwrap_or(0);
        let cfg = self.windows.get(&id).copied();
        let output = self.output_cfg.logical_size();
        let has_parent = toplevel.parent().is_some();
        toplevel.with_pending_state(|s| {
            s.bounds = Some(output);
            match cfg {
                Some(cfg) => apply_window_cfg(s, &cfg),
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

    fn toplevel_request(&mut self, toplevel: &ToplevelSurface, request: &str) {
        let Some(id) = surface_id(toplevel.wl_surface()) else { return };
        if self.gui.active_conn().is_some_and(|c| c.windows.contains_key(&id)) {
            self.send_json(serde_json::json!({"type": "toplevel-request", "id": id, "request": request}));
        }
    }

    /// The window the keyboard should focus for `window`: its topmost
    /// grabbing popup, or the toplevel.
    pub fn focus_target(&self, window: u32) -> Option<WlSurface> {
        self.grabs
            .iter()
            .rev()
            .find(|p| p.alive() && window_root(p.wl_surface()).and_then(|r| surface_id(&r)) == Some(window))
            .map(|p| p.wl_surface().clone())
            .or_else(|| self.toplevel_by_id(window).map(|t| t.wl_surface().clone()))
    }

    /// Moves keyboard focus, first releasing the keys held in the window it
    /// leaves.
    pub fn set_keyboard_focus(&mut self, target: Option<WlSurface>) {
        let Some(keyboard) = self.seat.get_keyboard() else { return };
        let current = keyboard.current_focus();
        if current == target {
            return;
        }
        let old_window = current.as_ref().and_then(window_root);
        let new_window = target.as_ref().and_then(window_root);
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

pub fn apply_window_cfg(s: &mut ToplevelState, cfg: &WindowCfg) {
    let scale = if cfg.scale > 0.0 { cfg.scale } else { 1.0 };
    s.size = (cfg.width > 0 && cfg.height > 0).then(|| {
        Size::from((
            (cfg.width as f64 / scale).round().max(1.0) as i32,
            (cfg.height as f64 / scale).round().max(1.0) as i32,
        ))
    });
    for (state, on) in [
        (xdg_toplevel::State::Activated, cfg.focused),
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

/// The root surface of the toplevel whose window `surface` draws in.
pub fn window_root(surface: &WlSurface) -> Option<WlSurface> {
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
        let (removed, has_callbacks, entered) = compositor::with_states(surface, |data| {
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
                match buffer {
                    Some(BufferAssignment::NewBuffer(buffer)) => {
                        take_buffer(s, &buffer, &damage, generation);
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
                (removed, !s.callbacks.is_empty(), entered)
            })
            .unwrap_or((false, false, true))
        });
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

fn take_buffer(s: &mut Surf, buffer: &WlBuffer, damage: &[Damage], generation: u64) {
    let app_has = s.app_has_content(generation);
    let result = shm::with_buffer_contents(buffer, |ptr, len, spec| {
        let Some(format) = wire_format(spec.format) else {
            warn!("unsupported shm format {:?}", spec.format);
            return;
        };
        let (width, height, stride) = (spec.width, spec.height, spec.stride);
        if width <= 0 || height <= 0 || stride < width * 4 {
            return;
        }
        let start = spec.offset as usize;
        let size = stride as usize * height as usize;
        if start + size > len {
            return;
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
    });
    if let Err(err) = result {
        warn!("buffer contents: {err:?}");
    }
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
        let (positioner, geometry) = solve_in_toplevel(surface.get_parent_surface(), positioner);
        surface.with_pending_state(|s| {
            s.positioner = positioner;
            s.geometry = geometry;
        });
    }

    fn grab(&mut self, surface: PopupSurface, _seat: WlSeat, _serial: Serial) {
        if !self.grabs.contains(&surface) {
            self.grabs.push(surface.clone());
        }
        let keyboard_window = self
            .seat
            .get_keyboard()
            .and_then(|k| k.current_focus())
            .and_then(|f| window_root(&f));
        if keyboard_window.is_some() && keyboard_window == window_root(surface.wl_surface()) {
            self.set_keyboard_focus(Some(surface.wl_surface().clone()));
        }
    }

    fn reposition_request(&mut self, surface: PopupSurface, positioner: PositionerState, token: u32) {
        let (positioner, geometry) = solve_in_toplevel(surface.get_parent_surface(), positioner);
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
                .and_then(|p| window_root(&p))
                .and_then(|r| surface_id(&r))
                .and_then(|w| self.focus_target(w));
            self.set_keyboard_focus(target);
        }
    }

    fn toplevel_destroyed(&mut self, surface: ToplevelSurface) {
        let focused = self.seat.get_keyboard().and_then(|k| k.current_focus());
        if focused.as_ref() == Some(surface.wl_surface()) {
            self.set_keyboard_focus(None);
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

    fn move_request(&mut self, surface: ToplevelSurface, _seat: WlSeat, _serial: Serial) {
        self.toplevel_request(&surface, "move");
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
    }

    fn cursor_image(&mut self, _seat: &Seat<Self>, image: CursorImageStatus) {
        let name = match image {
            CursorImageStatus::Hidden => "hidden".to_string(),
            CursorImageStatus::Named(icon) => icon.name().to_string(),
            // A client-drawn cursor reaches the app as CURSOR_IMAGE, later.
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
        let scale = window_root(&surface)
            .and_then(|r| surface_id(&r))
            .map_or(self.output_cfg.scale, |w| self.window_scale(w));
        compositor::with_states(&surface, |data| {
            smithay::wayland::fractional_scale::with_fractional_scale(data, |fs| fs.set_preferred_scale(scale));
            with_surf(data, |s| s.applied_scale = Some(scale));
        });
    }
}

impl XdgActivationHandler for State {
    fn activation_state(&mut self) -> &mut XdgActivationState {
        &mut self.xdg_activation_state
    }

    fn request_activation(&mut self, token: XdgActivationToken, _data: XdgActivationTokenData, _surface: WlSurface) {
        debug!("activation request with token {}", token.as_str());
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
smithay::delegate_seat!(State);
smithay::delegate_data_device!(State);
smithay::delegate_primary_selection!(State);
smithay::delegate_ext_data_control!(State);
smithay::delegate_data_control!(State);
smithay::delegate_output!(State);
smithay::delegate_viewporter!(State);
smithay::delegate_fractional_scale!(State);
smithay::delegate_xdg_activation!(State);
smithay::delegate_cursor_shape!(State);
