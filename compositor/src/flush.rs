//! Turns the compositor's state into GUI protocol messages after each
//! dispatch: window metadata, surface membership and one group of commits
//! per changed window. A new connection gets the same treatment from a
//! blank slate, which is its replay.

use std::collections::HashSet;
use std::time::Duration;

use serde_json::Value;
use serde_json::json;
use smithay::reexports::calloop::timer::TimeoutAction;
use smithay::reexports::calloop::timer::Timer;
use smithay::reexports::wayland_protocols::xdg::decoration::zv1::server::zxdg_toplevel_decoration_v1::Mode as DecorationMode;
use smithay::reexports::wayland_server::Resource;
use smithay::reexports::wayland_server::protocol::wl_callback::WlCallback;
use smithay::reexports::wayland_server::protocol::wl_output;
use smithay::reexports::wayland_server::protocol::wl_surface::WlSurface;
use smithay::utils::Logical;
use smithay::utils::Point;
use smithay::utils::Size;
use smithay::utils::Transform;
use smithay::wayland::compositor;
use smithay::wayland::compositor::RegionAttributes;
use smithay::wayland::compositor::SubsurfaceCachedState;
use smithay::wayland::compositor::SurfaceAttributes;
use smithay::wayland::compositor::TraversalAction;
use smithay::wayland::shell::xdg::SurfaceCachedState;
use smithay::wayland::shell::xdg::XdgPopupSurfaceData;
use smithay::wayland::shell::xdg::XdgToplevelSurfaceData;

use crate::protocol;
use crate::protocol::Commit;
use crate::protocol::Rect;
use crate::state::State;
use crate::state::toplevel_root;
use crate::state::window_geometry;
use crate::surface::Placement;
use crate::surface::Sent;
use crate::surface::Surf;
use crate::surface::with_surf;
use crate::windows::Member;

/// A surface as one layer of a window, bottom to top.
pub struct Layer {
    pub surface: WlSurface,
    pub id: u32,
    /// The toplevel whose tree it's in.
    pub toplevel: u32,
    pub role: &'static str,
    pub parent: u32,
    /// Origin in the window's logical coordinates (origin at the toplevel's
    /// window geometry).
    pub loc: Point<i32, Logical>,
    pub size: Size<i32, Logical>,
    pub mapped: bool,
    pub input: Option<RegionAttributes>,
}

fn to_px(v: i32, scale: f64) -> i32 {
    (v as f64 * scale).round() as i32
}

impl State {
    pub fn flush(&mut self) {
        self.grabs.retain(|p| p.alive());
        self.update_tree();
        let windows: Vec<(u32, Vec<Member>)> = self.tree.windows.iter().map(|w| (w.id, w.members.clone())).collect();
        let mut layouts = std::collections::HashMap::new();
        for (window, members) in &windows {
            let mut layers = Vec::new();
            for m in members {
                let origin = self.tree.origin.get(&m.id).copied().unwrap_or_default();
                self.collect_layers(m, origin, &mut layers);
            }
            let scale = self.window_scale(*window);
            for layer in &layers {
                self.apply_scale(&layer.surface, scale, layer.role == "toplevel");
            }
            layouts.insert(*window, layers);
        }
        self.layouts = layouts;

        if self.gui.active_conn().is_some() && !self.gui.backlogged() {
            self.emit_removals();
            for (window, members) in &windows {
                self.emit_window(*window, members);
            }
        }
        self.windowless_callbacks();
    }

    /// Tells the app about toplevels it knows that went away or now draw
    /// in another window. A window's removal takes its dialogs with it.
    fn emit_removals(&mut self) {
        let current: std::collections::HashMap<u32, u32> = self.tree.of.iter().map(|(id, (w, _))| (*id, *w)).collect();
        let Some(conn) = self.gui.active_conn_mut() else { return };
        let mut gone_windows = Vec::new();
        let windows: Vec<u32> = conn.toplevels.iter().filter(|(id, (w, _))| *id == w).map(|(id, _)| *id).collect();
        for window in windows {
            if current.get(&window) != Some(&window) {
                conn.toplevels.retain(|_, (w, _)| *w != window);
                conn.surfaces.remove(&window);
                conn.send(protocol::json_frame(&json!({"type": "toplevel-removed", "id": window})));
                gone_windows.push(window);
            }
        }
        let dialogs: Vec<(u32, u32)> = conn.toplevels.iter().map(|(id, (w, _))| (*id, *w)).filter(|(id, w)| id != w).collect();
        for (dialog, window) in dialogs {
            if current.get(&dialog) != Some(&window) {
                conn.toplevels.remove(&dialog);
                if let Some(surfaces) = conn.surfaces.get_mut(&window) {
                    surfaces.retain(|(_, t)| *t != dialog);
                }
                conn.send(protocol::json_frame(&json!({"type": "toplevel-removed", "id": dialog})));
            }
        }
        // Nothing will present an unmapped window.
        let time = self.time_ms();
        for window in gone_windows {
            for (_, cb) in self.held.remove(&window).unwrap_or_default() {
                cb.done(time);
            }
        }
    }

    /// A toplevel's layers: its own surface tree, then the popups it opened.
    fn collect_layers(&self, member: &Member, origin: Point<i32, Logical>, layers: &mut Vec<Layer>) {
        let root = member.toplevel.wl_surface();
        let geometry = window_geometry(root);
        let first = layers.len();
        push_tree(root, origin - geometry.loc, "toplevel", 0, member.id, layers);
        for popup in self.xdg_shell_state.popup_surfaces() {
            let surface = popup.wl_surface();
            if !popup.alive() || toplevel_root(surface).as_ref() != Some(root) {
                continue;
            }
            let Some(parent) = popup.get_parent_surface() else { continue };
            let Some((parent_loc, parent_id)) =
                layers[first..].iter().find(|l| l.surface == parent).map(|l| (l.loc, l.id))
            else {
                continue;
            };
            let parent_geometry = window_geometry(&parent);
            let popup_geometry = compositor::with_states(surface, |d| {
                d.data_map
                    .get::<XdgPopupSurfaceData>()
                    .map(|p| p.lock().unwrap().current.geometry.loc)
                    .unwrap_or_default()
            });
            let own = window_geometry(surface);
            let origin = parent_loc + parent_geometry.loc + popup_geometry - own.loc;
            push_tree(surface, origin, "popup", parent_id, member.id, layers);
        }
    }

    /// Tells a surface the scale its window renders at, and the client its
    /// integer scale, when they changed.
    fn apply_scale(&mut self, surface: &WlSurface, scale: f64, root: bool) {
        let changed = compositor::with_states(surface, |d| {
            let changed = with_surf(d, |s| {
                let changed = s.applied_scale != Some(scale);
                s.applied_scale = Some(scale);
                changed
            })
            .unwrap_or(false);
            if changed {
                smithay::wayland::fractional_scale::with_fractional_scale(d, |fs| fs.set_preferred_scale(scale));
                compositor::send_surface_state(surface, d, scale.ceil() as i32, Transform::Normal);
            }
            changed
        });
        let Some(client) = surface.client().filter(|_| root) else { return };
        let integer = scale.ceil().max(1.0) as i32;
        if (changed || !self.client_scales.contains_key(&client.id()))
            && self.client_scales.get(&client.id()) != Some(&integer)
        {
            self.client_scales.insert(client.id(), integer);
            for output in self.output.client_outputs(&client) {
                if output.version() >= 2 {
                    output.scale(integer);
                    if output.version() >= wl_output::EVT_DONE_SINCE {
                        output.done();
                    }
                }
            }
        }
    }

    fn toplevel_json(&self, window: u32, member: &Member) -> Value {
        let root = member.toplevel.wl_surface();
        let scale = self.window_scale(window);
        let (title, app_id) = compositor::with_states(root, |d| {
            d.data_map
                .get::<XdgToplevelSurfaceData>()
                .map(|t| {
                    let t = t.lock().unwrap();
                    (t.title.clone(), t.app_id.clone())
                })
                .unwrap_or_default()
        });
        let app_id = app_id.unwrap_or_default();
        let geometry = window_geometry(root);
        let (min, max) = compositor::with_states(root, |d| {
            let mut guard = d.cached_state.get::<SurfaceCachedState>();
            let c = guard.current();
            (c.min_size, c.max_size)
        });
        let decoration = match member.toplevel.current_state().decoration_mode {
            Some(DecorationMode::ServerSide) => "server",
            _ => "client",
        };
        let meta = self.meta.get(&member.id);
        let tab = if member.parent == 0 { meta.map(|m| m.tab.as_str()).unwrap_or_default() } else { "" };
        json!({
            "type": "toplevel",
            "id": member.id,
            "window": window,
            "parent": member.parent,
            "title": title.unwrap_or_default(),
            "appId": app_id,
            "app": self.app_of(member.id, &app_id),
            "launch": meta.map(|m| m.launch.as_str()).unwrap_or_default(),
            "tab": tab,
            "width": to_px(geometry.size.w, scale),
            "height": to_px(geometry.size.h, scale),
            "minWidth": to_px(min.w, scale),
            "minHeight": to_px(min.h, scale),
            "maxWidth": to_px(max.w, scale),
            "maxHeight": to_px(max.h, scale),
            "decoration": decoration,
        })
    }

    fn emit_window(&mut self, window: u32, members: &[Member]) {
        let infos: Vec<(u32, Value)> = members.iter().map(|m| (m.id, self.toplevel_json(window, m))).collect();
        let scale = self.window_scale(window);
        let generation = self.gui.generation;
        let log_hashes = self.gui.log_hashes;
        let Some(layers) = self.layouts.get(&window) else { return };
        let Some(conn) = self.gui.active.and_then(|id| self.gui.conns.get_mut(&id)) else { return };
        let want_hash = conn.hashes;

        for (id, info) in infos {
            if conn.toplevels.get(&id).map(|(w, v)| (*w, v)) != Some((window, &info)) {
                conn.send(protocol::json_frame(&info));
                conn.toplevels.insert(id, (window, info));
            }
        }
        let ids: Vec<(u32, u32)> = layers.iter().map(|l| (l.id, l.toplevel)).collect();
        let announced = conn.surfaces.remove(&window).unwrap_or_default();
        for (id, _) in announced.iter().filter(|(id, _)| !ids.iter().any(|(i, _)| i == id)) {
            conn.send(protocol::json_frame(&json!({"type": "surface-removed", "id": id})));
        }
        let mut fresh = HashSet::new();
        for layer in layers.iter().filter(|l| !announced.iter().any(|(id, _)| *id == l.id)) {
            fresh.insert(layer.id);
            conn.send(protocol::json_frame(&json!({
                "type": "surface",
                "id": layer.id,
                "window": window,
                "toplevel": layer.toplevel,
                "role": layer.role,
                "parent": layer.parent,
            })));
        }
        conn.surfaces.insert(window, ids);

        let mut group: Vec<(Commit, Vec<u8>, Option<u64>)> = Vec::new();
        let mut callbacks: Vec<WlCallback> = Vec::new();
        for (z, layer) in layers.iter().enumerate() {
            let built = compositor::with_states(&layer.surface, |d| {
                with_surf(d, |s| {
                    if fresh.contains(&layer.id) {
                        s.sent = None;
                    }
                    let mut mine = std::mem::take(&mut s.callbacks);
                    let built =
                        build_commit(s, layer, window, z as u16, scale, generation, want_hash || log_hashes, !mine.is_empty());
                    callbacks.append(&mut mine);
                    built
                })
                .flatten()
            });
            if let Some(mut commit) = built {
                if !want_hash {
                    commit.0.flags &= !protocol::FLAG_HASH;
                    commit.0.hash = 0;
                }
                group.push(commit);
            }
        }
        if group.is_empty() {
            debug_assert!(callbacks.is_empty());
            return;
        }
        let last = group.len() - 1;
        let mut seq = 0;
        for (i, (mut commit, data, hash)) in group.into_iter().enumerate() {
            seq = conn.next_seq();
            commit.seq = seq;
            if i < last {
                commit.flags |= protocol::FLAG_GROUPED;
            }
            if log_hashes && let Some(hash) = hash {
                eprintln!(
                    "hash surface={} seq={} {}x{} {:016x}",
                    commit.surface, seq, commit.width, commit.height, hash
                );
            }
            for frame in commit.encode(&data) {
                conn.send(frame);
            }
        }
        if !callbacks.is_empty() {
            self.held.entry(window).or_default().extend(callbacks.into_iter().map(|cb| (seq, cb)));
        }
    }

    /// Fires the callbacks of surfaces no window shows, at about the
    /// display's pace, while an app is connected.
    fn windowless_callbacks(&mut self) {
        self.cb_surfaces.retain(|s| {
            s.is_alive() && compositor::with_states(s, |d| with_surf(d, |s| !s.callbacks.is_empty())).unwrap_or(false)
        });
        if self.gui.active.is_none() {
            return;
        }
        let shown: HashSet<&WlSurface> = self.layouts.values().flatten().map(|l| &l.surface).collect();
        let mut fire = Vec::new();
        for surface in self.cb_surfaces.iter().filter(|s| !shown.contains(s)) {
            compositor::with_states(surface, |d| with_surf(d, |s| fire.append(&mut s.callbacks)));
        }
        if fire.is_empty() {
            return;
        }
        let interval = Duration::from_micros(1_000_000_000 / self.output_cfg.refresh.max(1000) as u64);
        let timer = self.lh.insert_source(Timer::from_duration(interval), move |_, _, state| {
            let time = state.time_ms();
            for cb in fire.drain(..) {
                if cb.is_alive() {
                    cb.done(time);
                }
            }
            TimeoutAction::Drop
        });
        if let Err(e) = timer {
            tracing::warn!("frame callback timer: {e}");
        }
    }
}

fn push_tree(
    surface: &WlSurface,
    origin: Point<i32, Logical>,
    role: &'static str,
    parent: u32,
    toplevel: u32,
    out: &mut Vec<Layer>,
) {
    fn offset(d: &compositor::SurfaceData) -> Point<i32, Logical> {
        if d.role == Some("subsurface") {
            d.cached_state.get::<SubsurfaceCachedState>().current().location
        } else {
            Point::from((0, 0))
        }
    }
    compositor::with_surface_tree_downward(
        surface,
        (origin, parent),
        |_, d, (loc, _)| {
            let id = with_surf(d, |s| s.id).unwrap_or(0);
            TraversalAction::DoChildren((*loc + offset(d), id))
        },
        |s, d, (loc, parent)| {
            let Some((id, size, mapped)) = with_surf(d, |st| (st.id, st.logical_size(), st.mirror.is_some())) else {
                return;
            };
            let input = d.cached_state.get::<SurfaceAttributes>().current().input_region.clone();
            out.push(Layer {
                surface: s.clone(),
                id,
                toplevel,
                role: if s == surface { role } else { "subsurface" },
                parent: *parent,
                loc: *loc + offset(d),
                size,
                mapped,
                input,
            });
        },
        |_, _, _| true,
    );
}

/// The COMMIT a surface needs, if any, with its encoded data and, when
/// asked for, its content hash. `force` (pending frame callbacks) asks for
/// one even when nothing changed.
#[allow(clippy::too_many_arguments)]
fn build_commit(
    s: &mut Surf,
    layer: &Layer,
    window: u32,
    z: u16,
    scale: f64,
    generation: u64,
    hash: bool,
    force: bool,
) -> Option<(Commit, Vec<u8>, Option<u64>)> {
    let x = to_px(layer.loc.x, scale);
    let y = to_px(layer.loc.y, scale);
    let placement = Placement {
        z,
        x,
        y,
        dst_width: (to_px(layer.loc.x + layer.size.w, scale) - x).max(0) as u32,
        dst_height: (to_px(layer.loc.y + layer.size.h, scale) - y).max(0) as u32,
        src: s.src_rect(),
        transform: wl_output::Transform::from(s.transform) as u8,
    };
    let sent = if s.sent_gen == generation { s.sent.clone() } else { None };
    let mut commit = Commit {
        surface: layer.id,
        window,
        z,
        x,
        y,
        ..Default::default()
    };
    let Some(m) = &s.mirror else {
        let had = sent.as_ref().is_some_and(|p| p.content.is_some());
        if !had && !force {
            return None;
        }
        commit.flags = protocol::FLAG_HIDDEN;
        s.sent = Some(Sent { content: None, placement });
        s.sent_gen = generation;
        return Some((commit, Vec::new(), None));
    };
    commit.format = m.format;
    commit.width = m.width;
    commit.height = m.height;
    commit.dst_width = placement.dst_width;
    commit.dst_height = placement.dst_height;
    (commit.src_x, commit.src_y, commit.src_width, commit.src_height) = placement.src;
    commit.transform = placement.transform;

    let content = (m.width, m.height, m.format);
    let app_has = sent.as_ref().is_some_and(|p| p.content == Some(content));
    let mut data = Vec::new();
    if !app_has || s.pending.full {
        commit.flags = protocol::FLAG_BUFFER | protocol::FLAG_FULL;
        commit.rects = vec![Rect { x: 0, y: 0, w: m.width, h: m.height }];
        data = m.pack(&commit.rects);
    } else if s.pending.scroll.is_some() || !s.pending.rects.is_empty() {
        commit.flags = protocol::FLAG_BUFFER;
        if let Some(dy) = s.pending.scroll {
            commit.flags |= protocol::FLAG_SCROLL;
            commit.scroll = dy;
        }
        commit.rects = std::mem::take(&mut s.pending.rects);
        data = m.pack(&commit.rects);
    } else if sent.as_ref().map(|p| p.placement) == Some(placement) && !force {
        return None;
    }
    s.pending = Default::default();
    let mut content_hash = None;
    if commit.flags & protocol::FLAG_BUFFER != 0 && hash {
        let h = m.hash();
        commit.flags |= protocol::FLAG_HASH;
        commit.hash = h;
        content_hash = Some(h);
    }
    commit.encoding = protocol::ENCODING_RAW;
    if !data.is_empty() {
        let packed = protocol::compress(&data);
        if packed.len() < data.len() {
            commit.encoding = protocol::ENCODING_ZSTD;
            data = packed;
        }
    }
    s.sent = Some(Sent { content: Some(content), placement });
    s.sent_gen = generation;
    Some((commit, data, content_hash))
}
