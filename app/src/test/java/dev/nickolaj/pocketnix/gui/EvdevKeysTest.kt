package dev.nickolaj.pocketnix.gui

import android.view.KeyEvent
import org.junit.Assert.assertEquals
import org.junit.Test

class EvdevKeysTest {
    @Test
    fun aHardwareKeyboardsScanCodeIsTheEvdevCode() {
        // KEY_102ND, which has no Android keycode of its own.
        assertEquals(86, EvdevKeys.code(KeyEvent.KEYCODE_UNKNOWN, 86, virtual = false))
    }

    @Test
    fun keysWithoutAScanCodeAreMapped() {
        assertEquals(30, EvdevKeys.code(KeyEvent.KEYCODE_A, 0, virtual = true))
        assertEquals(44, EvdevKeys.code(KeyEvent.KEYCODE_Z, 0, virtual = true))
        assertEquals(2, EvdevKeys.code(KeyEvent.KEYCODE_1, 0, virtual = true))
        assertEquals(11, EvdevKeys.code(KeyEvent.KEYCODE_0, 0, virtual = true))
        assertEquals(28, EvdevKeys.code(KeyEvent.KEYCODE_ENTER, 0, virtual = true))
        assertEquals(1, EvdevKeys.code(KeyEvent.KEYCODE_ESCAPE, 0, virtual = true))
        assertEquals(103, EvdevKeys.code(KeyEvent.KEYCODE_DPAD_UP, 0, virtual = true))
        assertEquals(88, EvdevKeys.code(KeyEvent.KEYCODE_F12, 0, virtual = true))
        assertEquals(125, EvdevKeys.code(KeyEvent.KEYCODE_META_LEFT, 0, virtual = true))
        // A virtual device's scan code means nothing.
        assertEquals(30, EvdevKeys.code(KeyEvent.KEYCODE_A, 99, virtual = true))
    }

    @Test
    fun keysTheGuestHasNoCodeForAreDropped() {
        assertEquals(0, EvdevKeys.code(KeyEvent.KEYCODE_UNKNOWN, 0, virtual = true))
    }

    @Test
    fun modifiersFromMetaState() {
        assertEquals(Gui.MOD_SHIFT or Gui.MOD_CTRL, EvdevKeys.mods(KeyEvent.META_SHIFT_ON or KeyEvent.META_SHIFT_LEFT_ON or KeyEvent.META_CTRL_ON))
        assertEquals(Gui.MOD_ALT or Gui.MOD_LOGO or Gui.MOD_CAPS or Gui.MOD_NUM, EvdevKeys.mods(KeyEvent.META_ALT_ON or KeyEvent.META_META_ON or KeyEvent.META_CAPS_LOCK_ON or KeyEvent.META_NUM_LOCK_ON))
        assertEquals(0, EvdevKeys.mods(0))
    }
}
