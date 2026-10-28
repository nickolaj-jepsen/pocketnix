//! `zwp_text_input_manager_v3` with the compositor's own dispatch, bridged
//! to Android's IME (docs/gui-protocol.md, Text input). Smithay's own
//! serves text input only to an input-method-v2 client.

use serde::Deserialize;
use serde_json::Value;
use serde_json::json;
use smithay::reexports::wayland_protocols::wp::text_input::zv3::server::zwp_text_input_manager_v3;
use smithay::reexports::wayland_protocols::wp::text_input::zv3::server::zwp_text_input_manager_v3::ZwpTextInputManagerV3;
use smithay::reexports::wayland_protocols::wp::text_input::zv3::server::zwp_text_input_v3;
use smithay::reexports::wayland_protocols::wp::text_input::zv3::server::zwp_text_input_v3::ZwpTextInputV3;
use smithay::reexports::wayland_server::Client;
use smithay::reexports::wayland_server::DataInit;
use smithay::reexports::wayland_server::Dispatch;
use smithay::reexports::wayland_server::DisplayHandle;
use smithay::reexports::wayland_server::GlobalDispatch;
use smithay::reexports::wayland_server::New;
use smithay::reexports::wayland_server::Resource;
use smithay::reexports::wayland_server::WEnum;
use smithay::reexports::wayland_server::backend::ClientId;
use smithay::reexports::wayland_server::backend::ObjectId;
use smithay::reexports::wayland_server::protocol::wl_surface::WlSurface;
use smithay::utils::Logical;
use smithay::utils::Rectangle;

use crate::state::State;

const PURPOSES: [&str; 14] = [
    "normal", "alpha", "digits", "number", "phone", "url", "email", "name", "password", "pin", "date", "time",
    "datetime", "terminal",
];

/// Content hint bits, lowest first.
const HINTS: [&str; 10] = [
    "completion",
    "spellcheck",
    "auto-capitalization",
    "lowercase",
    "uppercase",
    "titlecase",
    "hidden-text",
    "sensitive-data",
    "latin",
    "multiline",
];

/// A text input's double-buffered state.
#[derive(Clone, Default)]
struct Fields {
    enabled: bool,
    surrounding: Option<(String, i32, i32)>,
    input_method_cause: bool,
    hint: u32,
    purpose: u32,
    cursor_rect: Option<Rectangle<i32, Logical>>,
}

struct Instance {
    resource: ZwpTextInputV3,
    pending: Fields,
    /// An `enable` since the last commit.
    enabling: bool,
    current: Fields,
    /// Counts committed enables across all text inputs; the latest wins.
    enabled_at: u64,
    /// `commit` requests received, which `done` echoes.
    commits: u32,
    /// The surface this text input was last sent `enter` for.
    entered: Option<WlSurface>,
    /// The last `ime` serial sent to it, and that serial when it last committed.
    sent_serial: u32,
    applied: u32,
}

#[derive(Default)]
pub struct TextInputs {
    instances: Vec<Instance>,
    /// The keyboard focus, which text input follows.
    focus: Option<WlSurface>,
    enables: u64,
    /// The text input and enable the current session is for.
    active: Option<(ObjectId, u64)>,
    session: u32,
    sessions: u32,
    /// The last `text-input` message sent on the active connection.
    sent: Option<Value>,
    /// Set while a replay hasn't reached its `text-input` yet.
    pub hold: bool,
}

#[derive(Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct ImeMsg {
    session: u32,
    serial: u32,
    delete_before: u32,
    delete_after: u32,
    commit: String,
    preedit: String,
    preedit_cursor_begin: Option<i32>,
    preedit_cursor_end: Option<i32>,
}

impl Default for ImeMsg {
    fn default() -> Self {
        Self {
            session: 0,
            serial: 0,
            delete_before: 0,
            delete_after: 0,
            commit: String::new(),
            preedit: String::new(),
            preedit_cursor_begin: None,
            preedit_cursor_end: None,
        }
    }
}

pub fn create_global(dh: &DisplayHandle) {
    dh.create_global::<State, ZwpTextInputManagerV3, ()>(1, ());
}

fn same_client(a: &impl Resource, b: &impl Resource) -> bool {
    a.id().same_client_as(&b.id())
}

impl TextInputs {
    /// The text input that counts: the last enabled one on the focused surface.
    fn active_index(&self) -> Option<usize> {
        let focus = self.focus.as_ref().filter(|s| s.is_alive())?;
        self.instances
            .iter()
            .enumerate()
            .filter(|(_, i)| i.resource.is_alive() && i.current.enabled && i.entered.as_ref() == Some(focus))
            .max_by_key(|(_, i)| i.enabled_at)
            .map(|(n, _)| n)
    }

    fn key(&self, n: usize) -> (ObjectId, u64) {
        (self.instances[n].resource.id(), self.instances[n].enabled_at)
    }
}

impl State {
    /// Keyboard focus moved: text input leaves the old surface and enters the new one.
    pub fn text_input_focus(&mut self, focus: Option<WlSurface>) {
        if self.ti.focus == focus {
            return;
        }
        for i in &mut self.ti.instances {
            if let Some(old) = i.entered.take() {
                if old.is_alive() && i.resource.is_alive() {
                    i.resource.leave(&old);
                }
                i.current.enabled = false;
                i.pending.enabled = false;
            }
            if let Some(new) = focus.as_ref().filter(|s| same_client(*s, &i.resource)) {
                i.resource.enter(new);
                i.entered = Some(new.clone());
            }
        }
        self.ti.focus = focus;
    }

    /// A new app connection: sessions and `ime` serials start over.
    pub fn reset_text_input(&mut self) {
        self.ti.active = None;
        self.ti.session = 0;
        self.ti.sessions = 0;
        self.ti.sent = None;
        self.ti.hold = true;
        for i in &mut self.ti.instances {
            i.sent_serial = 0;
            i.applied = 0;
        }
    }

    /// Sends the app the text input's state when it changed.
    pub fn sync_text_input(&mut self) {
        if self.ti.hold || self.gui.active_conn().is_none() {
            return;
        }
        self.ti.instances.retain(|i| i.resource.is_alive());
        let active = self.ti.active_index();
        let key = active.map(|n| self.ti.key(n));
        if key != self.ti.active {
            self.ti.active = key;
            self.ti.session = match active {
                Some(_) => {
                    self.ti.sessions += 1;
                    self.ti.sessions
                }
                None => 0,
            };
        }
        let msg = match active {
            Some(n) => self.text_input_json(n),
            None => json!({
                "type": "text-input", "window": 0, "session": 0, "applied": 0, "text": null,
                "cursor": 0, "anchor": 0, "cause": "other", "purpose": "normal", "hints": [], "cursorRect": null,
            }),
        };
        if self.ti.sent.as_ref() != Some(&msg) {
            self.send_json(msg.clone());
            self.ti.sent = Some(msg);
        }
    }

    fn text_input_json(&self, n: usize) -> Value {
        let i = &self.ti.instances[n];
        let focus = self.ti.focus.as_ref();
        let window = focus.and_then(|f| self.window_of(f)).unwrap_or(0);
        let c = &i.current;
        let (text, cursor, anchor) = match &c.surrounding {
            Some((text, cursor, anchor)) => (Value::from(text.as_str()), *cursor, *anchor),
            None => (Value::Null, 0, 0),
        };
        let hints: Vec<&str> = HINTS
            .iter()
            .enumerate()
            .filter(|(bit, _)| c.hint & (1 << bit) != 0)
            .map(|(_, h)| *h)
            .collect();
        let rect = c.cursor_rect.and_then(|r| {
            let layer = self.layouts.get(&window)?.iter().find(|l| Some(&l.surface) == focus)?;
            let scale = self.window_scale(window);
            let px = |v: i32| (v as f64 * scale).round() as i32;
            let (x0, y0) = (layer.loc.x + r.loc.x, layer.loc.y + r.loc.y);
            let (x1, y1) = (x0 + r.size.w, y0 + r.size.h);
            Some(json!({"x": px(x0), "y": px(y0), "width": px(x1) - px(x0), "height": px(y1) - px(y0)}))
        });
        json!({
            "type": "text-input",
            "window": window,
            "session": self.ti.session,
            "applied": i.applied,
            "text": text,
            "cursor": cursor,
            "anchor": anchor,
            "cause": if c.input_method_cause { "input-method" } else { "other" },
            "purpose": PURPOSES.get(c.purpose as usize).copied().unwrap_or("normal"),
            "hints": hints,
            "cursorRect": rect,
        })
    }

    /// An edit from Android's IME, as one text-input-v3 `done`.
    pub fn on_ime(&mut self, m: ImeMsg) {
        let Some(n) = self.ti.active_index() else { return };
        // The session the app knows must still be this text input's.
        if m.session == 0 || m.session != self.ti.session || Some(self.ti.key(n)) != self.ti.active {
            return;
        }
        let i = &mut self.ti.instances[n];
        if m.delete_before != 0 || m.delete_after != 0 {
            i.resource.delete_surrounding_text(m.delete_before, m.delete_after);
        }
        if !m.commit.is_empty() {
            i.resource.commit_string(Some(m.commit));
        }
        if !m.preedit.is_empty() {
            let end = m.preedit.len() as i32;
            let begin = m.preedit_cursor_begin.unwrap_or(end);
            let end = m.preedit_cursor_end.unwrap_or(end);
            i.resource.preedit_string(Some(m.preedit), begin, end);
        }
        i.resource.done(i.commits);
        i.sent_serial = m.serial;
    }

    fn text_input_created(&mut self, resource: ZwpTextInputV3) {
        let entered = self.ti.focus.clone().filter(|f| f.is_alive() && same_client(f, &resource));
        if let Some(focus) = &entered {
            resource.enter(focus);
        }
        self.ti.instances.push(Instance {
            resource,
            pending: Fields::default(),
            enabling: false,
            current: Fields::default(),
            enabled_at: 0,
            commits: 0,
            entered,
            sent_serial: 0,
            applied: 0,
        });
    }

    fn text_input_request(&mut self, resource: &ZwpTextInputV3, request: zwp_text_input_v3::Request) {
        let ti = &mut self.ti;
        let enables = &mut ti.enables;
        let Some(i) = ti.instances.iter_mut().find(|i| i.resource == *resource) else { return };
        use zwp_text_input_v3::Request;
        match request {
            Request::Enable => {
                // Enabling resets everything an earlier enable set.
                i.pending = Fields { enabled: true, ..Default::default() };
                i.enabling = true;
            }
            Request::Disable => {
                i.pending.enabled = false;
                i.enabling = false;
            }
            Request::SetSurroundingText { text, cursor, anchor } => i.pending.surrounding = Some((text, cursor, anchor)),
            Request::SetTextChangeCause { cause } => {
                i.pending.input_method_cause = matches!(cause, WEnum::Value(zwp_text_input_v3::ChangeCause::InputMethod));
            }
            Request::SetContentType { hint, purpose } => {
                i.pending.hint = match hint {
                    WEnum::Value(h) => h.bits(),
                    WEnum::Unknown(h) => h,
                };
                i.pending.purpose = match purpose {
                    WEnum::Value(p) => p as u32,
                    WEnum::Unknown(p) => p,
                };
            }
            Request::SetCursorRectangle { x, y, width, height } => {
                i.pending.cursor_rect = Some(Rectangle::new((x, y).into(), (width, height).into()));
            }
            Request::Commit => {
                i.commits = i.commits.wrapping_add(1);
                // Until it's entered again, a text input's requests don't count.
                if i.entered.is_none() {
                    return;
                }
                i.current = i.pending.clone();
                if std::mem::take(&mut i.enabling) {
                    *enables += 1;
                    i.enabled_at = *enables;
                }
                i.applied = i.sent_serial;
            }
            Request::Destroy => {}
            _ => {}
        }
    }
}

impl GlobalDispatch<ZwpTextInputManagerV3, ()> for State {
    fn bind(
        _state: &mut State,
        _dh: &DisplayHandle,
        _client: &Client,
        resource: New<ZwpTextInputManagerV3>,
        _data: &(),
        data_init: &mut DataInit<'_, State>,
    ) {
        data_init.init(resource, ());
    }
}

impl Dispatch<ZwpTextInputManagerV3, ()> for State {
    fn request(
        state: &mut State,
        _client: &Client,
        _resource: &ZwpTextInputManagerV3,
        request: zwp_text_input_manager_v3::Request,
        _data: &(),
        _dh: &DisplayHandle,
        data_init: &mut DataInit<'_, State>,
    ) {
        if let zwp_text_input_manager_v3::Request::GetTextInput { id, .. } = request {
            let resource = data_init.init(id, ());
            state.text_input_created(resource);
        }
    }
}

impl Dispatch<ZwpTextInputV3, ()> for State {
    fn request(
        state: &mut State,
        _client: &Client,
        resource: &ZwpTextInputV3,
        request: zwp_text_input_v3::Request,
        _data: &(),
        _dh: &DisplayHandle,
        _data_init: &mut DataInit<'_, State>,
    ) {
        state.text_input_request(resource, request);
    }

    fn destroyed(state: &mut State, _client: ClientId, resource: &ZwpTextInputV3, _data: &()) {
        state.ti.instances.retain(|i| i.resource != *resource);
    }
}
