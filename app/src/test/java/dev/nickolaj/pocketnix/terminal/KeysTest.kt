package dev.nickolaj.pocketnix.terminal

import android.view.KeyEvent
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class KeysTest {
    @Test
    fun namedKeysAreKeyCodes() {
        assertEquals(KeyAction.Code(KeyEvent.KEYCODE_ESCAPE), ExtraKeys.action("ESC"))
        assertEquals(KeyAction.Code(KeyEvent.KEYCODE_DPAD_UP), ExtraKeys.action("UP"))
        assertEquals(KeyAction.Code(KeyEvent.KEYCODE_PAGE_DOWN), ExtraKeys.action("PGDN"))
        assertEquals(KeyAction.Code(KeyEvent.KEYCODE_F12), ExtraKeys.action("F12"))
        // Termux's aliases and any case.
        assertEquals(KeyAction.Code(KeyEvent.KEYCODE_PAGE_UP), ExtraKeys.action("page_up"))
        assertEquals(KeyAction.Code(KeyEvent.KEYCODE_DEL), ExtraKeys.action("BACKSPACE"))
    }

    @Test
    fun modifierNamesToggle() {
        assertEquals(KeyAction.Toggle(Modifier.CTRL), ExtraKeys.action("CTRL"))
        assertEquals(KeyAction.Toggle(Modifier.CTRL), ExtraKeys.action("control"))
        assertEquals(KeyAction.Toggle(Modifier.FN), ExtraKeys.action("FN"))
    }

    @Test
    fun anythingElseIsTypedLiterally() {
        assertEquals(KeyAction.Text("-"), ExtraKeys.action("-"))
        assertEquals(KeyAction.Text("|"), ExtraKeys.action("|"))
        assertEquals(KeyAction.Text("ls -la"), ExtraKeys.action("ls -la"))
    }

    @Test
    fun labels() {
        assertEquals("↑", ExtraKeys.label("UP"))
        assertEquals("ESC", ExtraKeys.label("escape"))
        assertEquals("CTRL", ExtraKeys.label("ctrl"))
        assertEquals("~", ExtraKeys.label("~"))
    }

    @Test
    fun fnCombinationsFollowTermux() {
        assertEquals(KeyAction.Code(KeyEvent.KEYCODE_DPAD_UP), FnKeys.action('w'.code))
        assertEquals(KeyAction.Code(KeyEvent.KEYCODE_DPAD_LEFT), FnKeys.action('A'.code))
        assertEquals(KeyAction.Code(KeyEvent.KEYCODE_F1), FnKeys.action('1'.code))
        assertEquals(KeyAction.Code(KeyEvent.KEYCODE_F9), FnKeys.action('9'.code))
        assertEquals(KeyAction.Code(KeyEvent.KEYCODE_F10), FnKeys.action('0'.code))
        assertEquals(KeyAction.Code(KeyEvent.KEYCODE_ESCAPE), FnKeys.action('e'.code))
        assertEquals(KeyAction.Text("|"), FnKeys.action('l'.code))
        assertEquals(KeyAction.Text("\u001bb"), FnKeys.action('b'.code))
        assertNull(FnKeys.action('z'.code))
    }

    @Test
    fun aTappedModifierAppliesToOneKey() {
        val m = Modifiers()
        m.tap(Modifier.CTRL)
        assertTrue(m.active(Modifier.CTRL))
        assertTrue(m.consume(Modifier.CTRL))
        assertFalse(m.consume(Modifier.CTRL))
    }

    @Test
    fun aDoubleTappedModifierLocksUntilTappedAgain() {
        val m = Modifiers()
        m.tap(Modifier.ALT)
        m.tap(Modifier.ALT)
        assertEquals(Modifiers.State.LOCKED, m.state(Modifier.ALT))
        assertTrue(m.consume(Modifier.ALT))
        assertTrue(m.consume(Modifier.ALT))
        m.tap(Modifier.ALT)
        assertFalse(m.consume(Modifier.ALT))
    }

    @Test
    fun aHeldVolumeKeyAppliesWhileDown() {
        val m = Modifiers()
        m.hold(Modifier.CTRL, true)
        assertTrue(m.consume(Modifier.CTRL))
        assertTrue(m.consume(Modifier.CTRL))
        m.hold(Modifier.CTRL, false)
        assertFalse(m.consume(Modifier.CTRL))
    }
}
