//! The guest's keymap and typing text as key presses
//! (docs/gui-protocol.md, Keyboard layout and Typing text).

use serde::Deserialize;
use smithay::backend::input::KeyState;
use smithay::input::keyboard::FilterResult;
use smithay::input::keyboard::KeyboardHandle;
use smithay::input::keyboard::KeyboardTarget;
use smithay::input::keyboard::Keycode;
use smithay::input::keyboard::Keysym;
use smithay::input::keyboard::ModifiersState;
use smithay::input::keyboard::XkbConfig;
use smithay::input::keyboard::xkb;
use smithay::reexports::wayland_server::Resource;
use smithay::utils::SERIAL_COUNTER;
use tracing::info;
use tracing::warn;

use crate::state::State;

const KEY_TAB: u32 = 15;
const KEY_ENTER: u32 = 28;

#[derive(Deserialize, Default)]
#[serde(default)]
pub struct LayoutMsg {
    pub layout: String,
    pub variant: String,
}

#[derive(Deserialize, Default)]
#[serde(default)]
pub struct TextMsg {
    pub window: u32,
    pub text: String,
}

/// One key press: an XKB keycode, with the Shift and level-3 modifiers its
/// level needs, or None to leave them as they are.
#[derive(Clone, Copy, Debug)]
struct Stroke {
    key: u32,
    mods: Option<(bool, bool)>,
}

enum Char {
    Fixed(Stroke),
    Sym(Keysym),
}

/// The lowest keycode, then the lowest level, that types `sym` in the
/// keymap's current layout, with the Shift and level-3 modifiers that reach
/// that level under `live`'s other modifiers and locks.
fn find_key(keymap: &xkb::Keymap, live: &xkb::State, sym: Keysym) -> Option<Stroke> {
    let bit = |name: &str| {
        let i = keymap.mod_get_index(name);
        if i == xkb::MOD_INVALID { 0 } else { 1u32 << i }
    };
    let (shift, level3) = (bit(xkb::MOD_NAME_SHIFT), bit(xkb::MOD_NAME_ISO_LEVEL3_SHIFT));
    let held = live.serialize_mods(xkb::STATE_MODS_DEPRESSED) & !(shift | level3);
    let locked = live.serialize_mods(xkb::STATE_MODS_LOCKED);
    let group = live.serialize_layout(xkb::STATE_LAYOUT_EFFECTIVE);
    let mut scratch = xkb::State::new(keymap);
    for raw in keymap.min_keycode().raw()..=keymap.max_keycode().raw() {
        let key = Keycode::new(raw);
        scratch.update_mask(held, 0, locked, 0, 0, group);
        let layout = scratch.key_get_layout(key);
        for level in 0..keymap.num_levels_for_key(key, layout) {
            if !keymap.key_get_syms_by_level(key, layout, level).contains(&sym) {
                continue;
            }
            for (s, l3) in [(false, false), (true, false), (false, true), (true, true)] {
                let mods = held | if s { shift } else { 0 } | if l3 { level3 } else { 0 };
                scratch.update_mask(mods, 0, locked, 0, 0, group);
                if scratch.key_get_level(key, layout) == level {
                    return Some(Stroke { key: raw, mods: Some((s, l3)) });
                }
            }
        }
    }
    None
}

/// `keymap` with each of `syms` on a keycode that has no symbols, as
/// `wtype` does it; None when there's no room for any.
fn with_extra_syms(keymap: &xkb::Keymap, syms: &[Keysym]) -> Option<String> {
    let free = (keymap.min_keycode().raw()..=keymap.max_keycode().raw())
        .map(Keycode::new)
        .filter(|k| keymap.num_layouts_for_key(*k) == 0)
        .filter_map(|k| keymap.key_get_name(k).map(str::to_owned));
    let lines: String = free
        .zip(syms)
        .map(|(name, sym)| format!("\tkey <{name}> {{ [ {} ] }};\n", xkb::keysym_get_name(*sym)))
        .collect();
    if lines.is_empty() {
        return None;
    }
    let mut text = keymap.get_as_string(xkb::KEYMAP_FORMAT_TEXT_V1);
    let symbols = text.find("xkb_symbols")?;
    let end = symbols + text[symbols..].find("\n};")? + 1;
    text.insert_str(end, &lines);
    Some(text)
}

impl State {
    /// The `keyboard-layout` message: `""` is the compositor's own default.
    pub fn set_keyboard_layout(&mut self, m: LayoutMsg) {
        let want = (m.layout, m.variant);
        if self.keyboard_layout == want {
            return;
        }
        let Some(keyboard) = self.seat.get_keyboard() else { return };
        let config = if want.0.is_empty() {
            XkbConfig::default()
        } else {
            XkbConfig {
                rules: "evdev",
                layout: &want.0,
                variant: &want.1,
                ..Default::default()
            }
        };
        if let Err(e) = keyboard.set_xkb_config(self, config) {
            warn!("keyboard layout {:?} variant {:?}: {e}; keeping the current keymap", want.0, want.1);
            return;
        }
        info!("keyboard layout {:?} variant {:?}", want.0, want.1);
        self.keyboard_layout = want;
    }

    /// The `text` message: types `text` into `window` as key presses.
    pub fn type_text(&mut self, m: TextMsg) {
        self.focus_window(m.window);
        let Some(keyboard) = self.seat.get_keyboard() else { return };
        if keyboard.current_focus().is_none() {
            return;
        }
        let chars: Vec<Char> = m
            .text
            .chars()
            .filter_map(|c| match c {
                '\n' => Some(Char::Fixed(Stroke { key: KEY_ENTER + 8, mods: None })),
                '\t' => Some(Char::Fixed(Stroke { key: KEY_TAB + 8, mods: None })),
                c if c.is_control() => None,
                c => Some(Char::Sym(xkb::utf32_to_keysym(c as u32))),
            })
            .collect();
        let syms: Vec<Keysym> = chars
            .iter()
            .filter_map(|c| match c {
                Char::Sym(s) => Some(*s),
                Char::Fixed(_) => None,
            })
            .collect();
        let mut found = self.find_keys(&keyboard, &syms);
        let mut missing: Vec<Keysym> = syms.iter().zip(&found).filter(|(_, k)| k.is_none()).map(|(s, _)| *s).collect();
        missing.sort_by_key(|s| s.raw());
        missing.dedup();

        // Missing characters go on spare keycodes, and the keymap comes back after.
        let mut original = None;
        if !missing.is_empty() {
            let (current, extended) = keyboard.with_xkb_state(self, |ctx| {
                let xkb = ctx.xkb().lock().unwrap();
                // SAFETY: the keymap reference doesn't outlive this closure.
                let keymap = unsafe { xkb.keymap() };
                (keymap.get_as_string(xkb::KEYMAP_FORMAT_TEXT_V1), with_extra_syms(keymap, &missing))
            });
            match extended.map(|k| keyboard.set_keymap_from_string(self, k)) {
                Some(Ok(())) => {
                    original = Some(current);
                    found = self.find_keys(&keyboard, &syms);
                }
                Some(Err(e)) => warn!("typing: the keymap with extra symbols didn't compile: {e}"),
                None => warn!("typing: no spare keycode for {} symbols", missing.len()),
            }
        }

        let mut found = found.into_iter();
        for c in &chars {
            let stroke = match c {
                Char::Fixed(s) => Some(*s),
                Char::Sym(_) => found.next().flatten(),
            };
            if let Some(stroke) = stroke {
                self.press(&keyboard, stroke);
            }
        }
        if let Some(original) = original
            && let Err(e) = keyboard.set_keymap_from_string(self, original)
        {
            warn!("typing: restoring the keymap: {e}");
        }
    }

    fn find_keys(&mut self, keyboard: &KeyboardHandle<State>, syms: &[Keysym]) -> Vec<Option<Stroke>> {
        keyboard.with_xkb_state(self, |ctx| {
            let xkb = ctx.xkb().lock().unwrap();
            // SAFETY: the keymap and state references don't outlive this closure.
            let (keymap, live) = unsafe { (xkb.keymap(), xkb.state()) };
            syms.iter().map(|s| find_key(keymap, live, *s)).collect()
        })
    }

    fn press(&mut self, keyboard: &KeyboardHandle<State>, stroke: Stroke) {
        let saved = keyboard.modifier_state();
        if let Some((shift, level3)) = stroke.mods {
            let mut m = saved;
            m.shift = shift;
            m.iso_level3_shift = level3;
            self.set_modifiers(keyboard, m);
        }
        for state in [KeyState::Pressed, KeyState::Released] {
            let serial = SERIAL_COUNTER.next_serial();
            let time = self.time_ms();
            keyboard.input::<(), _>(self, stroke.key.into(), state, serial, time, |_, _, _| FilterResult::Forward);
            if state == KeyState::Pressed
                && let Some(client) = keyboard.current_focus().and_then(|f| f.client())
            {
                self.input_serials.push(serial, client.id());
            }
        }
        if stroke.mods.is_some() {
            self.set_modifiers(keyboard, saved);
        }
    }

    /// Sets the modifier state and tells the focused client when it changed.
    pub fn set_modifiers(&mut self, keyboard: &KeyboardHandle<State>, m: ModifiersState) {
        if keyboard.set_modifier_state(m) != 0
            && let Some(focus) = keyboard.current_focus()
        {
            let seat = self.seat.clone();
            focus.modifiers(&seat, self, keyboard.modifier_state(), SERIAL_COUNTER.next_serial());
        }
    }
}
