package dev.nickolaj.pocketnix.terminal

import android.view.KeyEvent

enum class Modifier { CTRL, ALT, SHIFT, FN }

/** What pressing a key does. */
sealed interface KeyAction {
    /** A key the terminal encodes itself (arrows, F-keys, …), sent as a key event. */
    data class Code(val keyCode: Int) : KeyAction

    /** Text typed as code points, with the active modifiers applied. */
    data class Text(val text: String) : KeyAction

    data class Toggle(val modifier: Modifier) : KeyAction
}

/** The module's extra-keys vocabulary: Termux's key names, or a literal character. */
object ExtraKeys {
    private val codes = mapOf(
        "ESC" to KeyEvent.KEYCODE_ESCAPE,
        "TAB" to KeyEvent.KEYCODE_TAB,
        "HOME" to KeyEvent.KEYCODE_MOVE_HOME,
        "END" to KeyEvent.KEYCODE_MOVE_END,
        "PGUP" to KeyEvent.KEYCODE_PAGE_UP,
        "PGDN" to KeyEvent.KEYCODE_PAGE_DOWN,
        "INS" to KeyEvent.KEYCODE_INSERT,
        "DEL" to KeyEvent.KEYCODE_FORWARD_DEL,
        "BKSP" to KeyEvent.KEYCODE_DEL,
        "UP" to KeyEvent.KEYCODE_DPAD_UP,
        "DOWN" to KeyEvent.KEYCODE_DPAD_DOWN,
        "LEFT" to KeyEvent.KEYCODE_DPAD_LEFT,
        "RIGHT" to KeyEvent.KEYCODE_DPAD_RIGHT,
        "ENTER" to KeyEvent.KEYCODE_ENTER,
        "SPACE" to KeyEvent.KEYCODE_SPACE
    ) + (1..12).associate { "F$it" to KeyEvent.KEYCODE_F1 + it - 1 }

    private val aliases = mapOf(
        "ESCAPE" to "ESC",
        "CONTROL" to "CTRL",
        "SHFT" to "SHIFT",
        "FUNCTION" to "FN",
        "PAGEUP" to "PGUP",
        "PAGE_UP" to "PGUP",
        "PAGEDOWN" to "PGDN",
        "PAGE_DOWN" to "PGDN",
        "BACKSPACE" to "BKSP",
        "DELETE" to "DEL",
        "INSERT" to "INS",
        "RETURN" to "ENTER",
        "LT" to "LEFT",
        "RT" to "RIGHT",
        "DN" to "DOWN"
    )

    private val labels = mapOf(
        "UP" to "↑",
        "DOWN" to "↓",
        "LEFT" to "←",
        "RIGHT" to "→",
        "BKSP" to "⌫",
        "ENTER" to "⏎",
        "TAB" to "⇥",
        "SPACE" to "␣"
    )

    private fun canonical(name: String): String = name.uppercase().let { aliases[it] ?: it }

    /** The action for a key name; anything unknown is typed literally. */
    fun action(name: String): KeyAction {
        val n = canonical(name)
        codes[n]?.let { return KeyAction.Code(it) }
        Modifier.entries.firstOrNull { it.name == n }?.let { return KeyAction.Toggle(it) }
        return KeyAction.Text(name)
    }

    fun label(name: String): String {
        val n = canonical(name)
        return labels[n] ?: if (codes.containsKey(n) || Modifier.entries.any { it.name == n }) n else name
    }
}

/**
 * Volume up held as Termux's "fn": with a letter it sends the key Termux
 * users know (W/A/S/D arrows, 1–0 F1–F10, …). Volume down is Ctrl.
 */
object FnKeys {
    private val codes = mapOf(
        'w' to KeyEvent.KEYCODE_DPAD_UP,
        'a' to KeyEvent.KEYCODE_DPAD_LEFT,
        's' to KeyEvent.KEYCODE_DPAD_DOWN,
        'd' to KeyEvent.KEYCODE_DPAD_RIGHT,
        'p' to KeyEvent.KEYCODE_PAGE_UP,
        'n' to KeyEvent.KEYCODE_PAGE_DOWN,
        't' to KeyEvent.KEYCODE_TAB,
        'i' to KeyEvent.KEYCODE_INSERT,
        'e' to KeyEvent.KEYCODE_ESCAPE,
        '0' to KeyEvent.KEYCODE_F10
    ) + ('1'..'9').associateWith { KeyEvent.KEYCODE_F1 + (it - '1') }

    private val text = mapOf(
        'h' to "~",
        'u' to "_",
        'l' to "|",
        // Ctrl+\ (SIGQUIT).
        '.' to "\u001c",
        // Alt+B/F/X: readline word motion and M-x.
        'b' to "\u001bb",
        'f' to "\u001bf",
        'x' to "\u001bx"
    )

    /** The action for [codePoint] typed with fn held, or null to type it unchanged. */
    fun action(codePoint: Int): KeyAction? {
        if (codePoint > 0xffff) return null
        val c = codePoint.toChar().lowercaseChar()
        codes[c]?.let { return KeyAction.Code(it) }
        text[c]?.let { return KeyAction.Text(it) }
        return null
    }
}

/**
 * The extra-keys modifier buttons. A tap arms a modifier for the next key, a
 * second tap locks it, a third releases it. Holding a volume key holds its
 * modifier for as long as it's down.
 */
class Modifiers {
    enum class State { OFF, ONCE, LOCKED }

    private val states = Modifier.entries.associateWith { State.OFF }.toMutableMap()
    private val held = mutableSetOf<Modifier>()

    @Synchronized
    fun tap(m: Modifier) {
        states[m] = when (states.getValue(m)) {
            State.OFF -> State.ONCE
            State.ONCE -> State.LOCKED
            State.LOCKED -> State.OFF
        }
    }

    @Synchronized
    fun state(m: Modifier): State = states.getValue(m)

    @Synchronized
    fun hold(m: Modifier, down: Boolean) {
        if (down) held.add(m) else held.remove(m)
    }

    /** Lets go of held keys whose key-up will go elsewhere (a dialog, the lock screen took focus). */
    @Synchronized
    fun releaseHeld() {
        held.clear()
    }

    /** Whether [m] would apply to the next key, without using up a one-shot modifier. */
    @Synchronized
    fun active(m: Modifier): Boolean = m in held || states.getValue(m) != State.OFF

    /** Whether [m] applies to the key being typed; a one-shot modifier is used up. */
    @Synchronized
    fun consume(m: Modifier): Boolean {
        if (m in held) return true
        return when (states.getValue(m)) {
            State.OFF -> false
            State.ONCE -> true.also { states[m] = State.OFF }
            State.LOCKED -> true
        }
    }
}
