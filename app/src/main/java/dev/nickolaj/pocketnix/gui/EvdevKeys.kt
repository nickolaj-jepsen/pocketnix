package dev.nickolaj.pocketnix.gui

import android.view.KeyEvent

/** Android key events as the GUI protocol's KEY and MODIFIERS carry them: Linux evdev codes. */
object EvdevKeys {
    /**
     * The evdev code for a key: a hardware keyboard's [scanCode] is one
     * already; [keyCode] is mapped for events without one ([virtual], or 0).
     * 0 for a key the guest has no code for.
     */
    fun code(keyCode: Int, scanCode: Int, virtual: Boolean): Int {
        if (!virtual && scanCode in 1..MAX_CODE) return scanCode
        return codes[keyCode] ?: 0
    }

    /** MODIFIERS' bits from Android's meta state. */
    fun mods(metaState: Int): Int {
        var m = 0
        if (metaState and KeyEvent.META_SHIFT_ON != 0) m = m or Gui.MOD_SHIFT
        if (metaState and KeyEvent.META_CTRL_ON != 0) m = m or Gui.MOD_CTRL
        if (metaState and KeyEvent.META_ALT_ON != 0) m = m or Gui.MOD_ALT
        if (metaState and KeyEvent.META_META_ON != 0) m = m or Gui.MOD_LOGO
        if (metaState and KeyEvent.META_CAPS_LOCK_ON != 0) m = m or Gui.MOD_CAPS
        if (metaState and KeyEvent.META_NUM_LOCK_ON != 0) m = m or Gui.MOD_NUM
        return m
    }

    // KEY_MAX in linux/input-event-codes.h.
    private const val MAX_CODE = 0x2ff

    private val codes: Map<Int, Int> = buildMap {
        val letters = "QWERTYUIOP" to 16
        val home = "ASDFGHJKL" to 30
        val bottom = "ZXCVBNM" to 44
        for ((row, first) in listOf(letters, home, bottom)) {
            row.forEachIndexed { i, c -> put(KeyEvent.KEYCODE_A + (c - 'A'), first + i) }
        }
        // KEY_1..KEY_9 are 2..10, KEY_0 is 11.
        for (d in 1..9) put(KeyEvent.KEYCODE_0 + d, 1 + d)
        put(KeyEvent.KEYCODE_0, 11)
        // KEY_F1..KEY_F10 are 59..68; F11, F12 are 87, 88.
        for (f in 0..9) put(KeyEvent.KEYCODE_F1 + f, 59 + f)
        put(KeyEvent.KEYCODE_F11, 87)
        put(KeyEvent.KEYCODE_F12, 88)
        put(KeyEvent.KEYCODE_ESCAPE, 1)
        put(KeyEvent.KEYCODE_MINUS, 12)
        put(KeyEvent.KEYCODE_EQUALS, 13)
        put(KeyEvent.KEYCODE_DEL, 14)
        put(KeyEvent.KEYCODE_TAB, 15)
        put(KeyEvent.KEYCODE_LEFT_BRACKET, 26)
        put(KeyEvent.KEYCODE_RIGHT_BRACKET, 27)
        put(KeyEvent.KEYCODE_ENTER, 28)
        put(KeyEvent.KEYCODE_CTRL_LEFT, 29)
        put(KeyEvent.KEYCODE_SEMICOLON, 39)
        put(KeyEvent.KEYCODE_APOSTROPHE, 40)
        put(KeyEvent.KEYCODE_GRAVE, 41)
        put(KeyEvent.KEYCODE_SHIFT_LEFT, 42)
        put(KeyEvent.KEYCODE_BACKSLASH, 43)
        put(KeyEvent.KEYCODE_COMMA, 51)
        put(KeyEvent.KEYCODE_PERIOD, 52)
        put(KeyEvent.KEYCODE_SLASH, 53)
        put(KeyEvent.KEYCODE_SHIFT_RIGHT, 54)
        put(KeyEvent.KEYCODE_NUMPAD_MULTIPLY, 55)
        put(KeyEvent.KEYCODE_ALT_LEFT, 56)
        put(KeyEvent.KEYCODE_SPACE, 57)
        put(KeyEvent.KEYCODE_CAPS_LOCK, 58)
        put(KeyEvent.KEYCODE_NUM_LOCK, 69)
        put(KeyEvent.KEYCODE_SCROLL_LOCK, 70)
        put(KeyEvent.KEYCODE_NUMPAD_7, 71)
        put(KeyEvent.KEYCODE_NUMPAD_8, 72)
        put(KeyEvent.KEYCODE_NUMPAD_9, 73)
        put(KeyEvent.KEYCODE_NUMPAD_SUBTRACT, 74)
        put(KeyEvent.KEYCODE_NUMPAD_4, 75)
        put(KeyEvent.KEYCODE_NUMPAD_5, 76)
        put(KeyEvent.KEYCODE_NUMPAD_6, 77)
        put(KeyEvent.KEYCODE_NUMPAD_ADD, 78)
        put(KeyEvent.KEYCODE_NUMPAD_1, 79)
        put(KeyEvent.KEYCODE_NUMPAD_2, 80)
        put(KeyEvent.KEYCODE_NUMPAD_3, 81)
        put(KeyEvent.KEYCODE_NUMPAD_0, 82)
        put(KeyEvent.KEYCODE_NUMPAD_DOT, 83)
        put(KeyEvent.KEYCODE_NUMPAD_ENTER, 96)
        put(KeyEvent.KEYCODE_CTRL_RIGHT, 97)
        put(KeyEvent.KEYCODE_NUMPAD_DIVIDE, 98)
        put(KeyEvent.KEYCODE_SYSRQ, 99)
        put(KeyEvent.KEYCODE_ALT_RIGHT, 100)
        put(KeyEvent.KEYCODE_MOVE_HOME, 102)
        put(KeyEvent.KEYCODE_DPAD_UP, 103)
        put(KeyEvent.KEYCODE_PAGE_UP, 104)
        put(KeyEvent.KEYCODE_DPAD_LEFT, 105)
        put(KeyEvent.KEYCODE_DPAD_RIGHT, 106)
        put(KeyEvent.KEYCODE_MOVE_END, 107)
        put(KeyEvent.KEYCODE_DPAD_DOWN, 108)
        put(KeyEvent.KEYCODE_PAGE_DOWN, 109)
        put(KeyEvent.KEYCODE_INSERT, 110)
        put(KeyEvent.KEYCODE_FORWARD_DEL, 111)
        put(KeyEvent.KEYCODE_NUMPAD_EQUALS, 117)
        put(KeyEvent.KEYCODE_BREAK, 119)
        put(KeyEvent.KEYCODE_NUMPAD_COMMA, 121)
        put(KeyEvent.KEYCODE_META_LEFT, 125)
        put(KeyEvent.KEYCODE_META_RIGHT, 126)
        put(KeyEvent.KEYCODE_MENU, 127)
    }
}
