//! Touch input (docs/gui-protocol.md, Touch): the TOUCH_* frames as
//! `wl_touch` events, and which clients have a `wl_touch`.

use std::collections::HashMap;

use smithay::backend::input::TouchSlot;
use smithay::input::SeatState;
use smithay::input::touch::DownEvent;
use smithay::input::touch::GrabStartData;
use smithay::input::touch::MotionEvent;
use smithay::input::touch::OrientationEvent;
use smithay::input::touch::ShapeEvent;
use smithay::input::touch::TouchGrab;
use smithay::input::touch::TouchInnerHandle;
use smithay::input::touch::UpEvent;
use smithay::reexports::wayland_server::Client;
use smithay::reexports::wayland_server::DataInit;
use smithay::reexports::wayland_server::Dispatch;
use smithay::reexports::wayland_server::DisplayHandle;
use smithay::reexports::wayland_server::Resource;
use smithay::reexports::wayland_server::backend::ClientId;
use smithay::reexports::wayland_server::protocol::wl_seat;
use smithay::reexports::wayland_server::protocol::wl_seat::WlSeat;
use smithay::reexports::wayland_server::protocol::wl_surface::WlSurface;
use smithay::reexports::wayland_server::protocol::wl_touch;
use smithay::reexports::wayland_server::protocol::wl_touch::WlTouch;
use smithay::utils::Logical;
use smithay::utils::Point;
use smithay::utils::SERIAL_COUNTER;
use smithay::utils::Serial;
use smithay::wayland::seat::SeatUserData;
use smithay::wayland::seat::TouchUserData;

use crate::state::State;

/// A point that is down, by window and Android pointer id.
pub struct TouchPoint {
    pub slot: u32,
    /// Where it is now, window logical.
    pub at: Point<f64, Logical>,
    pub serial: Serial,
}

/// The touch points that are down and the clients that have a `wl_touch`.
#[derive(Default)]
pub struct Touch {
    pub points: HashMap<(u32, u32), TouchPoint>,
    /// Live `wl_touch` objects per client.
    pub clients: HashMap<ClientId, usize>,
}

impl Touch {
    pub fn has_touch(&self, client: &ClientId) -> bool {
        self.clients.get(client).is_some_and(|n| *n > 0)
    }

    /// The lowest slot no point holds.
    fn free_slot(&self) -> u32 {
        (0..).find(|s| !self.points.values().any(|p| p.slot == *s)).unwrap_or(0)
    }
}

/// Each point keeps the surface it went down on: smithay's default grab
/// would send every later point to the first one's.
pub struct PerPointGrab(GrabStartData<State>);

impl PerPointGrab {
    pub fn new() -> Self {
        Self(GrabStartData {
            focus: None,
            slot: TouchSlot::from(None),
            location: Point::default(),
        })
    }
}

impl TouchGrab<State> for PerPointGrab {
    fn down(
        &mut self,
        data: &mut State,
        handle: &mut TouchInnerHandle<'_, State>,
        focus: Option<(WlSurface, Point<f64, Logical>)>,
        event: &DownEvent,
        seq: Serial,
    ) {
        handle.down(data, focus, event, seq);
    }

    fn up(&mut self, data: &mut State, handle: &mut TouchInnerHandle<'_, State>, event: &UpEvent, seq: Serial) {
        handle.up(data, event, seq);
    }

    fn motion(
        &mut self,
        data: &mut State,
        handle: &mut TouchInnerHandle<'_, State>,
        focus: Option<(WlSurface, Point<f64, Logical>)>,
        event: &MotionEvent,
        seq: Serial,
    ) {
        handle.motion(data, focus, event, seq);
    }

    fn frame(&mut self, data: &mut State, handle: &mut TouchInnerHandle<'_, State>, seq: Serial) {
        handle.frame(data, seq);
    }

    fn cancel(&mut self, data: &mut State, handle: &mut TouchInnerHandle<'_, State>, seq: Serial) {
        handle.cancel(data, seq);
    }

    fn shape(&mut self, data: &mut State, handle: &mut TouchInnerHandle<'_, State>, event: &ShapeEvent, seq: Serial) {
        handle.shape(data, event, seq);
    }

    fn orientation(
        &mut self,
        data: &mut State,
        handle: &mut TouchInnerHandle<'_, State>,
        event: &OrientationEvent,
        seq: Serial,
    ) {
        handle.orientation(data, event, seq);
    }

    fn start_data(&self) -> &GrabStartData<State> {
        &self.0
    }

    fn unset(&mut self, _data: &mut State) {}
}

impl State {
    pub fn client_has_touch(&self, surface: &WlSurface) -> bool {
        surface.client().is_some_and(|c| self.touch.has_touch(&c.id()))
    }

    pub fn touch_down(&mut self, window: u32, id: u32, x: f32, y: f32) {
        if self.touch.points.contains_key(&(window, id)) {
            self.touch_up(window, id);
        }
        let scale = self.window_scale(window);
        let p: Point<f64, Logical> = (x as f64 / scale, y as f64 / scale).into();
        let hit = self.hit_layer(window, p);
        if hit.as_ref().is_some_and(|(_, _, t)| self.blocked_by_modal(*t)) {
            return;
        }
        self.grabs.retain(|p| p.alive());
        if !self.grabs.is_empty() && !hit.as_ref().is_some_and(|(s, _, _)| self.in_grab(s)) {
            // Outside a grabbing popup: it dismisses the popups and goes no further.
            self.dismiss_grabs(window);
            return;
        }
        let Some((surface, origin, toplevel)) = hit else { return };
        self.pressed_on(window, toplevel);
        if !self.client_has_touch(&surface) {
            return;
        }
        let Some(touch) = self.seat.get_touch() else { return };
        let slot = self.touch.free_slot();
        let serial = SERIAL_COUNTER.next_serial();
        let event = DownEvent {
            slot: Some(slot).into(),
            location: p,
            serial,
            time: self.time_ms(),
        };
        touch.down(self, Some((surface.clone(), origin)), &event);
        let keyboard = self.seat.get_keyboard().and_then(|k| k.current_focus());
        if let Some(client) = surface.client()
            && keyboard.and_then(|k| k.client()).is_some_and(|k| k.id() == client.id())
        {
            self.input_serials.push(serial, client.id());
        }
        self.touch.points.insert((window, id), TouchPoint { slot, at: p, serial });
    }

    pub fn touch_motion(&mut self, window: u32, id: u32, x: f32, y: f32) {
        let scale = self.window_scale(window);
        let p: Point<f64, Logical> = (x as f64 / scale, y as f64 / scale).into();
        let Some(point) = self.touch.points.get_mut(&(window, id)) else { return };
        point.at = p;
        let slot = point.slot;
        if let Some(drag) = &self.drag
            && drag.touch == Some((window, id))
        {
            let offset = drag.start_offset + (p - drag.start).to_i32_round();
            self.dialogs.entry(drag.dialog).or_default().offset = Some(offset);
        }
        let Some(touch) = self.seat.get_touch() else { return };
        let event = MotionEvent {
            slot: Some(slot).into(),
            location: p,
            time: self.time_ms(),
        };
        touch.motion(self, None, &event);
    }

    pub fn touch_up(&mut self, window: u32, id: u32) {
        let Some(point) = self.touch.points.remove(&(window, id)) else { return };
        if self.drag.as_ref().is_some_and(|d| d.touch == Some((window, id))) {
            self.drag = None;
        }
        let Some(touch) = self.seat.get_touch() else { return };
        let event = UpEvent {
            slot: Some(point.slot).into(),
            serial: SERIAL_COUNTER.next_serial(),
            time: self.time_ms(),
        };
        touch.up(self, &event);
    }

    pub fn touch_frame(&mut self) {
        if let Some(touch) = self.seat.get_touch() {
            touch.frame(self);
        }
    }

    /// Ends every point in `window` without effect.
    pub fn touch_cancel(&mut self, window: u32) {
        let points: Vec<(u32, u32)> = self.touch.points.keys().filter(|(w, _)| *w == window).copied().collect();
        if points.is_empty() {
            return;
        }
        if self.drag.as_ref().is_some_and(|d| d.touch.is_some_and(|(w, _)| w == window)) {
            self.drag = None;
        }
        let Some(touch) = self.seat.get_touch() else { return };
        // Smithay cancels only the points with events since the last frame:
        // a motion where each point already is gives every one of them one.
        for key in points {
            let Some(point) = self.touch.points.remove(&key) else { continue };
            let event = MotionEvent {
                slot: Some(point.slot).into(),
                location: point.at,
                time: self.time_ms(),
            };
            touch.motion(self, None, &event);
        }
        touch.cancel(self);
    }

    /// Cancels the points of windows that are gone.
    pub fn touch_unmapped(&mut self) {
        let gone: Vec<u32> = self
            .touch
            .points
            .keys()
            .map(|(w, _)| *w)
            .filter(|w| !self.tree.windows.iter().any(|t| t.id == *w))
            .collect();
        for window in gone {
            self.touch_cancel(window);
        }
    }

    /// The point a touch serial belongs to, for `xdg_toplevel.move`.
    pub fn touch_point_by_serial(&self, serial: Serial) -> Option<((u32, u32), Point<f64, Logical>)> {
        self.touch
            .points
            .iter()
            .find(|(_, p)| p.serial == serial)
            .map(|(k, p)| (*k, p.at))
    }
}

// The seat's own dispatch, plus counting each client's wl_touch objects.

impl Dispatch<WlSeat, SeatUserData<State>> for State {
    fn request(
        state: &mut State,
        client: &Client,
        resource: &WlSeat,
        request: wl_seat::Request,
        data: &SeatUserData<State>,
        dh: &DisplayHandle,
        data_init: &mut DataInit<'_, State>,
    ) {
        if matches!(request, wl_seat::Request::GetTouch { .. }) {
            *state.touch.clients.entry(client.id()).or_default() += 1;
        }
        <SeatState<State> as Dispatch<WlSeat, SeatUserData<State>, State>>::request(
            state, client, resource, request, data, dh, data_init,
        );
    }

    fn destroyed(state: &mut State, client: ClientId, resource: &WlSeat, data: &SeatUserData<State>) {
        <SeatState<State> as Dispatch<WlSeat, SeatUserData<State>, State>>::destroyed(state, client, resource, data);
    }
}

impl Dispatch<WlTouch, TouchUserData<State>> for State {
    fn request(
        state: &mut State,
        client: &Client,
        resource: &WlTouch,
        request: wl_touch::Request,
        data: &TouchUserData<State>,
        dh: &DisplayHandle,
        data_init: &mut DataInit<'_, State>,
    ) {
        <SeatState<State> as Dispatch<WlTouch, TouchUserData<State>, State>>::request(
            state, client, resource, request, data, dh, data_init,
        );
    }

    fn destroyed(state: &mut State, client: ClientId, resource: &WlTouch, data: &TouchUserData<State>) {
        if let Some(n) = state.touch.clients.get_mut(&client) {
            *n = n.saturating_sub(1);
            if *n == 0 {
                state.touch.clients.remove(&client);
            }
        }
        <SeatState<State> as Dispatch<WlTouch, TouchUserData<State>, State>>::destroyed(state, client, resource, data);
    }
}
