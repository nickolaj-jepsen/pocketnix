//! The window model (docs/gui-protocol.md, Windows and surfaces, Dialogs and
//! Where a window goes): which mapped toplevels are windows and which are
//! dialogs drawn inside one, where a dialog sits, which toplevel of a window
//! has focus, and what a window is bound to when it first maps.

use std::collections::HashMap;
use std::collections::HashSet;

use smithay::reexports::wayland_protocols::xdg::shell::server::xdg_toplevel;
use smithay::reexports::wayland_server::Resource;
use smithay::reexports::wayland_server::protocol::wl_surface::WlSurface;
use smithay::utils::Logical;
use smithay::utils::Point;
use smithay::utils::Size;
use smithay::wayland::compositor;
use smithay::wayland::shell::xdg::ToplevelSurface;
use smithay::wayland::shell::xdg::XdgToplevelSurfaceData;

use crate::launch;
use crate::launch::Outcome;
use crate::state::State;
use crate::state::surface_id;
use crate::state::toplevel_root;
use crate::state::window_area;
use crate::state::window_geometry;
use crate::surface::with_surf;

/// A toplevel in a window's stack.
#[derive(Clone)]
pub struct Member {
    pub id: u32,
    pub toplevel: ToplevelSurface,
    /// The nearest mapped ancestor's id; 0 for the window itself.
    pub parent: u32,
}

/// A window and its dialogs, bottom to top: the window first, each
/// toplevel's dialogs above it in the order they were last raised.
pub struct WindowTree {
    pub id: u32,
    pub members: Vec<Member>,
}

/// The mapped toplevels as the last flush laid them out.
#[derive(Default)]
pub struct Tree {
    pub windows: Vec<WindowTree>,
    /// Toplevel id → (window id, parent id or 0).
    pub of: HashMap<u32, (u32, u32)>,
    /// Toplevel id → its window geometry's origin in its window.
    pub origin: HashMap<u32, Point<i32, Logical>>,
}

/// What a toplevel keeps for its life.
#[derive(Default)]
pub struct Meta {
    pub launch: String,
    /// The window's tab; for a dialog, its window's, kept for when it
    /// becomes a window.
    pub tab: String,
    pub mapped_before: bool,
}

#[derive(Default)]
pub struct Dialog {
    /// Set once the user moved it: its offset from its parent's origin.
    pub offset: Option<Point<i32, Logical>>,
    pub raised: u64,
}

/// A dialog following the pointer, or a touch point, after its `move` request.
pub struct Drag {
    pub dialog: u32,
    pub window: u32,
    /// The touch point it follows, by window and id; None for the pointer.
    pub touch: Option<(u32, u32)>,
    pub start: Point<f64, Logical>,
    pub start_offset: Point<i32, Logical>,
}

fn is_modal(toplevel: &ToplevelSurface) -> bool {
    compositor::with_states(toplevel.wl_surface(), |d| {
        d.data_map
            .get::<XdgToplevelSurfaceData>()
            .is_some_and(|t| t.lock().unwrap().modal)
    })
}

pub fn app_id(toplevel: &ToplevelSurface) -> String {
    compositor::with_states(toplevel.wl_surface(), |d| {
        d.data_map
            .get::<XdgToplevelSurfaceData>()
            .and_then(|t| t.lock().unwrap().app_id.clone())
            .unwrap_or_default()
    })
}

impl State {
    /// The window `surface` draws in, as of the last flush (its own
    /// toplevel's id before that toplevel first maps).
    pub fn window_of(&self, surface: &WlSurface) -> Option<u32> {
        let id = toplevel_root(surface).and_then(|r| surface_id(&r))?;
        Some(self.tree.of.get(&id).map_or(id, |(w, _)| *w))
    }

    /// Whether a toplevel draws inside another one's window.
    pub fn is_dialog(&self, id: u32) -> bool {
        match self.tree.of.get(&id) {
            Some((w, _)) => *w != id,
            None => self.toplevel_by_id(id).is_some_and(|t| t.parent().is_some()),
        }
    }

    /// Where a toplevel's popups may go: its origin in its window, and the
    /// part of the window Android shows.
    pub fn popup_space(&self, root: &WlSurface) -> Option<(Point<i32, Logical>, Size<i32, Logical>)> {
        let id = surface_id(root)?;
        let window = self.tree.of.get(&id).map_or(id, |(w, _)| *w);
        let origin = self.tree.origin.get(&id).copied().unwrap_or_default();
        let area = window_area(self.toplevel_by_id(window)?.wl_surface());
        Some((origin, area))
    }

    /// The logical size a dialog of `parent`'s window may take.
    pub fn dialog_bounds(&self, parent: &WlSurface) -> Option<Size<i32, Logical>> {
        let id = surface_id(parent)?;
        let window = self.tree.of.get(&id).map_or(id, |(w, _)| *w);
        Some(window_area(self.toplevel_by_id(window)?.wl_surface())).filter(|s| s.w > 0 && s.h > 0)
    }

    fn mapped(&self) -> HashMap<u32, ToplevelSurface> {
        self.xdg_shell_state
            .toplevel_surfaces()
            .iter()
            .filter(|t| t.alive() && t.is_initial_configure_sent())
            .filter_map(|t| {
                let (id, mapped) = compositor::with_states(t.wl_surface(), |d| with_surf(d, |s| (s.id, s.mirror.is_some())))?;
                mapped.then(|| (id, t.clone()))
            })
            .collect()
    }

    fn build_tree(&self) -> Tree {
        let mapped = self.mapped();
        // Each toplevel's nearest mapped ancestor.
        let mut parent_of: HashMap<u32, u32> = HashMap::new();
        for (id, t) in &mapped {
            let mut p = t.parent();
            for _ in 0..32 {
                let Some(surface) = p else { break };
                match surface_id(&surface) {
                    Some(pid) if mapped.contains_key(&pid) && pid != *id => {
                        parent_of.insert(*id, pid);
                        break;
                    }
                    _ => p = self.toplevel(&surface).and_then(|t| t.parent()),
                }
            }
        }
        let mut children: HashMap<u32, Vec<u32>> = HashMap::new();
        for (child, parent) in &parent_of {
            children.entry(*parent).or_default().push(*child);
        }
        for list in children.values_mut() {
            list.sort_by_key(|c| (self.dialogs.get(c).map_or(0, |d| d.raised), *c));
        }
        let mut roots: Vec<u32> = mapped.keys().filter(|id| !parent_of.contains_key(id)).copied().collect();
        roots.sort();
        let mut tree = Tree::default();
        for root in roots {
            let mut members = Vec::new();
            let mut stack = vec![root];
            while let Some(id) = stack.pop() {
                if members.iter().any(|m: &Member| m.id == id) {
                    continue;
                }
                members.push(Member {
                    id,
                    toplevel: mapped[&id].clone(),
                    parent: parent_of.get(&id).copied().unwrap_or(0),
                });
                if let Some(list) = children.get(&id) {
                    stack.extend(list.iter().rev());
                }
            }
            for m in &members {
                tree.of.insert(m.id, (root, m.parent));
            }
            tree.windows.push(WindowTree { id: root, members });
        }
        tree
    }

    /// Recomputes the window model; called at the start of every flush.
    pub fn update_tree(&mut self) {
        let mut tree = self.build_tree();
        let old = std::mem::take(&mut self.tree.of);
        // A dialog that maps, or arrives in another window, is raised and focused.
        let arrived: Vec<(u32, u32)> = tree
            .of
            .iter()
            .filter(|(id, (w, p))| *p != 0 && old.get(id).is_none_or(|(ow, _)| ow != w))
            .map(|(id, (w, _))| (*w, *id))
            .collect();
        if !arrived.is_empty() {
            for (w, id) in arrived {
                self.raise(w, id);
            }
            tree = self.build_tree();
        }

        for w in &tree.windows {
            let window_tab = self.meta.get(&w.id).map(|m| m.tab.clone());
            for m in &w.members {
                let first = !self.meta.get(&m.id).is_some_and(|meta| meta.mapped_before);
                if first {
                    self.meta.entry(m.id).or_default().mapped_before = true;
                    if m.parent == 0 {
                        self.bind_window(m);
                    } else if let Some(launch) = self.activations.remove(&m.id) {
                        // A launch's token activating a dialog ends it like a window would.
                        self.end_launch(&launch, Outcome::Window(m.id));
                        self.send_json(serde_json::json!({"type": "activate", "window": w.id, "launch": launch}));
                    }
                }
                if m.parent != 0
                    && let Some(tab) = &window_tab
                {
                    self.meta.entry(m.id).or_default().tab = tab.clone();
                }
            }
        }
        self.place(&mut tree);
        self.tree = tree;
        self.update_focus(&old);
        self.sync_states();
    }

    /// Binds a window that first maps to a launch, and decides its tab.
    fn bind_window(&mut self, m: &Member) {
        let pending: Vec<String> = {
            let mut l: Vec<&launch::Launch> = self.launches.values().filter(|l| l.outcome == Outcome::Pending).collect();
            l.sort_by_key(|l| l.started);
            l.into_iter().map(|l| l.id.clone()).collect()
        };
        let creds = m
            .toplevel
            .wl_surface()
            .client()
            .and_then(|c| self.dh.backend_handle().get_client_credentials(c.id()).ok());
        let app_id = app_id(&m.toplevel);
        let by_token = self.activations.remove(&m.id).filter(|l| pending.contains(l));
        let by_scope = || {
            let pid = creds.as_ref()?.pid as u32;
            pending.iter().find(|l| launch::in_scope(pid, &self.launches[*l].unit)).cloned()
        };
        let by_app_id = || {
            pending
                .iter()
                .find(|l| {
                    let app = &self.launches[*l].app;
                    *app == app_id || self.apps.get(app).and_then(|e| e.wm_class.as_deref()) == Some(app_id.as_str())
                })
                .cloned()
        };
        let bound = by_token.or_else(by_scope).or_else(|| (!app_id.is_empty()).then(by_app_id).flatten());
        let tab = match &bound {
            Some(l) => {
                let tab = self.launches[l].tab.clone();
                self.end_launch(l, Outcome::Window(m.id));
                tab
            }
            None => self.client_tab(&m.toplevel).unwrap_or_default(),
        };
        let meta = self.meta.entry(m.id).or_default();
        meta.launch = bound.unwrap_or_default();
        meta.tab = tab;
    }

    /// The entry a toplevel that hasn't mapped yet belongs to, as far as
    /// is known when its first configure goes out: a pending launch whose
    /// token it activated with or whose scope it runs in, else its app id's.
    pub fn entry_for_new(&self, id: u32, toplevel: &ToplevelSurface) -> Option<String> {
        let pending = |l: &&launch::Launch| l.outcome == Outcome::Pending;
        if let Some(l) = self.activations.get(&id).and_then(|l| self.launches.get(l)).filter(pending) {
            return Some(l.app.clone());
        }
        let pid = toplevel
            .wl_surface()
            .client()
            .and_then(|c| self.dh.backend_handle().get_client_credentials(c.id()).ok())
            .map(|c| c.pid as u32);
        if let Some(pid) = pid {
            let mut launches: Vec<&launch::Launch> = self.launches.values().filter(pending).collect();
            launches.sort_by_key(|l| l.started);
            if let Some(l) = launches.into_iter().find(|l| launch::in_scope(pid, &l.unit)) {
                return Some(l.app.clone());
            }
        }
        self.apps.for_app_id(&app_id(toplevel)).map(|e| e.id.clone())
    }

    /// `POCKETNIX_TAB` from the client's own environment, read once per client.
    fn client_tab(&mut self, toplevel: &ToplevelSurface) -> Option<String> {
        let client = toplevel.wl_surface().client()?;
        if let Some(tab) = self.client_tabs.get(&client.id()) {
            return tab.clone();
        }
        let creds = self.dh.backend_handle().get_client_credentials(client.id()).ok()?;
        // SAFETY: getuid can't fail.
        let tab = (creds.uid == unsafe { libc::getuid() })
            .then(|| launch::environ_tab(creds.pid as u32))
            .flatten();
        let dh = self.dh.clone();
        self.client_tabs.retain(|id, _| dh.backend_handle().get_client_data(id.clone()).is_ok());
        self.client_tabs.insert(client.id(), tab.clone());
        tab
    }

    /// Puts a dialog on top of its siblings and gives it its window's focus.
    pub fn raise(&mut self, window: u32, id: u32) {
        self.raise_seq += 1;
        self.dialogs.entry(id).or_default().raised = self.raise_seq;
        self.focus.insert(window, id);
    }

    /// Lays out each window's dialogs: centred on their parent, or where
    /// the user moved them, inside the part of the window Android shows.
    fn place(&mut self, tree: &mut Tree) {
        for w in &tree.windows {
            let area = window_area(w.members[0].toplevel.wl_surface());
            tree.origin.insert(w.id, Point::from((0, 0)));
            for m in w.members.iter().skip(1) {
                let parent_origin = tree.origin.get(&m.parent).copied().unwrap_or_default();
                let parent_size = tree
                    .windows
                    .iter()
                    .flat_map(|w| &w.members)
                    .find(|p| p.id == m.parent)
                    .map(|p| window_geometry(p.toplevel.wl_surface()).size)
                    .unwrap_or_default();
                let size = window_geometry(m.toplevel.wl_surface()).size;
                let mut loc = match self.dialogs.get(&m.id).and_then(|d| d.offset) {
                    Some(offset) => parent_origin + offset,
                    None => parent_origin
                        + Point::from(((parent_size.w - size.w) / 2, (parent_size.h - size.h) / 2)),
                };
                loc.x = if size.w >= area.w { 0 } else { loc.x.clamp(0, area.w - size.w) };
                loc.y = if size.h >= area.h { 0 } else { loc.y.clamp(0, area.h - size.h) };
                tree.origin.insert(m.id, loc);

                // Larger than the window: ask for the window's size once.
                if area.w > 0 && area.h > 0 && (size.w > area.w || size.h > area.h) {
                    let fit = Size::from((size.w.min(area.w), size.h.min(area.h)));
                    m.toplevel.with_pending_state(|s| s.size = Some(fit));
                }
            }
        }
    }

    /// Keeps one focused toplevel per window: a dialog that went away
    /// hands focus to its parent, and a modal dialog keeps it from its
    /// parent. Moves the keyboard along within the window that has it.
    fn update_focus(&mut self, old: &HashMap<u32, (u32, u32)>) {
        let windows: HashSet<u32> = self.tree.windows.iter().map(|w| w.id).collect();
        self.focus.retain(|w, _| windows.contains(w));
        for w in &self.tree.windows {
            let members: Vec<u32> = w.members.iter().map(|m| m.id).collect();
            let mut f = self.focus.get(&w.id).copied().unwrap_or(w.id);
            for _ in 0..32 {
                if members.contains(&f) {
                    break;
                }
                f = old.get(&f).map_or(w.id, |(_, p)| if *p == 0 { w.id } else { *p });
            }
            if !members.contains(&f) {
                f = w.id;
            }
            for _ in 0..32 {
                let modal = w
                    .members
                    .iter()
                    .rev()
                    .find(|m| m.parent == f && is_modal(&m.toplevel))
                    .map(|m| m.id);
                match modal {
                    Some(child) => f = child,
                    None => break,
                }
            }
            self.focus.insert(w.id, f);
        }
        // A destroyed dialog took the keyboard with it; the window Android focuses gets it back.
        let keyboard_window = self.keyboard_window().or_else(|| {
            self.tree
                .windows
                .iter()
                .map(|w| w.id)
                .find(|w| self.windows.get(w).is_some_and(|c| c.focused))
        });
        if let Some(window) = keyboard_window {
            let target = self.focus_target(window);
            let current = self.seat.get_keyboard().and_then(|k| k.current_focus());
            let in_grab = current.as_ref().is_some_and(|c| self.grabs.iter().any(|p| p.wl_surface() == c));
            if !in_grab && target.is_some() && current != target {
                self.set_keyboard_focus(target);
            }
        }
    }

    /// Whether a toplevel has a modal dialog over it, which takes its
    /// focus, buttons and scrolls.
    pub fn blocked_by_modal(&self, toplevel: u32) -> bool {
        self.tree
            .windows
            .iter()
            .flat_map(|w| &w.members)
            .any(|m| m.parent == toplevel && is_modal(&m.toplevel))
    }

    /// `activated` for each window's focused toplevel while the window is
    /// focused, `suspended` while it isn't visible.
    fn sync_states(&mut self) {
        for w in &self.tree.windows {
            let cfg = self.windows.get(&w.id).copied();
            let focused = self.focus.get(&w.id).copied();
            for m in &w.members {
                let activated = cfg.is_some_and(|c| c.focused) && focused == Some(m.id);
                let suspended = cfg.is_some_and(|c| !c.visible);
                m.toplevel.with_pending_state(|s| {
                    for (state, on) in [
                        (xdg_toplevel::State::Activated, activated),
                        (xdg_toplevel::State::Suspended, suspended),
                    ] {
                        if on {
                            s.states.set(state);
                        } else {
                            s.states.unset(state);
                        }
                    }
                });
                m.toplevel.send_pending_configure();
            }
        }
    }

    pub fn keyboard_window(&self) -> Option<u32> {
        let focus = self.seat.get_keyboard().and_then(|k| k.current_focus())?;
        self.window_of(&focus)
    }

    /// The surface the keyboard goes to for `window`: its topmost grabbing
    /// popup, or else its focused toplevel.
    pub fn focus_target(&self, window: u32) -> Option<WlSurface> {
        self.grabs
            .iter()
            .rev()
            .find(|p| p.alive() && self.window_of(p.wl_surface()) == Some(window))
            .map(|p| p.wl_surface().clone())
            .or_else(|| {
                let id = self.focus.get(&window).copied().unwrap_or(window);
                let toplevel = self.toplevel_by_id(id).or_else(|| self.toplevel_by_id(window))?;
                Some(toplevel.wl_surface().clone())
            })
    }

    /// A pointer press on a toplevel's surface raises and focuses it.
    pub fn pressed_on(&mut self, window: u32, toplevel: u32) {
        if self.blocked_by_modal(toplevel) {
            return;
        }
        self.raise(window, toplevel);
        if self.keyboard_window() == Some(window) {
            let target = self.focus_target(window);
            self.set_keyboard_focus(target);
        }
    }

    /// The `toplevel` message's `app`: its launch's entry, else the entry
    /// its app id names.
    pub fn app_of(&self, id: u32, app_id: &str) -> String {
        let launch = self.meta.get(&id).map(|m| m.launch.as_str()).unwrap_or_default();
        if let Some(l) = self.launches.get(launch) {
            return l.app.clone();
        }
        self.apps.for_app_id(app_id).map(|e| e.id.clone()).unwrap_or_default()
    }

    /// Forgets what a destroyed toplevel kept for its life.
    pub fn forget_toplevel(&mut self, id: u32) {
        self.meta.remove(&id);
        self.initial_scales.remove(&id);
        self.dialogs.remove(&id);
        self.activations.remove(&id);
        if self.drag.as_ref().is_some_and(|d| d.dialog == id) {
            self.drag = None;
        }
    }
}
