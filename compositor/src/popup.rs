// Copyright 2024 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// From the wprs fork's smithay_handlers.rs (the spike's popup constraint).

use smithay::reexports::wayland_protocols::xdg::shell::server::xdg_positioner;
use smithay::reexports::wayland_server::protocol::wl_surface::WlSurface;
use smithay::utils::Logical;
use smithay::utils::Point;
use smithay::utils::Rectangle;
use smithay::wayland::compositor;
use smithay::wayland::shell::xdg::PositionerState;
use smithay::wayland::shell::xdg::XdgPopupSurfaceData;
use smithay::wayland::shell::xdg::XdgToplevelSurfaceData;

use crate::state::window_geometry;

/// Solves a popup's positioner against its root toplevel's window geometry
/// and returns an already-solved positioner and the geometry it gives:
/// Android clips child layers to the window, so a popup must fit inside it.
pub fn solve_in_toplevel(
    parent: Option<WlSurface>,
    positioner: PositionerState,
) -> (PositionerState, Rectangle<i32, Logical>) {
    // the parent's offset from the toplevel's window geometry, and that size
    let mut offset = Point::<i32, Logical>::from((0, 0));
    let mut root = None;
    let mut next = parent;
    while let Some(surface) = next {
        next = compositor::with_states(&surface, |states| {
            if let Some(popup) = states.data_map.get::<XdgPopupSurfaceData>() {
                let popup = popup.lock().unwrap();
                offset += popup.current.geometry.loc;
                popup.parent.clone()
            } else {
                if states.data_map.get::<XdgToplevelSurfaceData>().is_some() {
                    root = Some(surface.clone());
                }
                None
            }
        });
    }
    // The size the window shows, not the one it was configured with: the
    // client may have drawn smaller.
    let size = root.map(|r| window_geometry(&r).size);
    let Some(size) = size.filter(|s| s.w > 0 && s.h > 0) else {
        return (positioner, positioner.get_geometry());
    };
    let target = Rectangle::new((-offset.x, -offset.y).into(), size);
    let mut geometry = positioner.get_unconstrained_geometry(target);
    // The client's own adjustments may not suffice (or it allowed none):
    // shrink, then slide, whatever it asked for.
    geometry.size.w = geometry.size.w.min(target.size.w);
    geometry.size.h = geometry.size.h.min(target.size.h);
    geometry.loc.x = geometry
        .loc
        .x
        .clamp(target.loc.x, target.loc.x + target.size.w - geometry.size.w);
    geometry.loc.y = geometry
        .loc
        .y
        .clamp(target.loc.y, target.loc.y + target.size.h - geometry.size.h);
    let solved = PositionerState {
        rect_size: geometry.size,
        anchor_rect: Rectangle::new(geometry.loc, (1, 1).into()),
        anchor_edges: xdg_positioner::Anchor::TopLeft,
        gravity: xdg_positioner::Gravity::BottomRight,
        constraint_adjustment: xdg_positioner::ConstraintAdjustment::empty(),
        offset: (0, 0).into(),
        ..positioner
    };
    (solved, geometry)
}
