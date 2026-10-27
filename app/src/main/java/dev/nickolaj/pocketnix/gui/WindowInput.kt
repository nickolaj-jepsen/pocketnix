package dev.nickolaj.pocketnix.gui

import android.os.Handler
import android.os.Looper
import android.view.InputDevice
import android.view.KeyCharacterMap
import android.view.KeyEvent
import android.view.MotionEvent
import kotlin.math.roundToInt

/**
 * A window's mouse, touchpad and keyboard as GUI protocol input, in the
 * window's pixels (the SurfaceView's coordinates). A touchscreen acts as a
 * mouse with one button until touch input lands (gui-apps.md, step 3).
 */
internal class WindowInput(
    private val window: Int,
    private val send: (ByteArray) -> Unit,
    /** Window pixels per logical pixel, for wheel steps. */
    private val scale: () -> Float,
) {
    private val main = Handler(Looper.getMainLooper())
    private var buttons = 0
    private var x = 0f
    private var y = 0f
    private var touching = false
    private var swipeX = 0f
    private var swipeY = 0f
    private var mods = -1

    // A mouse press starts with a hover exit; only one that nothing follows is a leave.
    private val leave = Runnable { send(Gui.pointerLeave(window)) }

    /** Touch and generic motion events on the window's view; true when consumed. */
    fun motion(e: MotionEvent): Boolean {
        main.removeCallbacks(leave)
        if (e.classification == MotionEvent.CLASSIFICATION_TWO_FINGER_SWIPE) return swipe(e)
        val mouse = e.isFromSource(InputDevice.SOURCE_MOUSE) || e.getToolType(0) == MotionEvent.TOOL_TYPE_MOUSE
        when (e.actionMasked) {
            MotionEvent.ACTION_HOVER_ENTER, MotionEvent.ACTION_HOVER_MOVE -> move(e)
            MotionEvent.ACTION_HOVER_EXIT -> main.postDelayed(leave, LEAVE_DELAY_MS)
            MotionEvent.ACTION_SCROLL -> scroll(e, mouse)
            MotionEvent.ACTION_DOWN, MotionEvent.ACTION_MOVE, MotionEvent.ACTION_UP, MotionEvent.ACTION_CANCEL,
            MotionEvent.ACTION_BUTTON_PRESS, MotionEvent.ACTION_BUTTON_RELEASE,
            // A press without a button state (a touchscreen, or an injected mouse tap) is a left click.
            -> if (e.buttonState != 0 || buttons != 0) mouseButtons(e) else touch(e)
            else -> return false
        }
        return true
    }

    private fun move(e: MotionEvent) {
        if (e.x == x && e.y == y) return
        x = e.x
        y = e.y
        send(Gui.pointerMotion(window, x, y))
    }

    /** A mouse or touchpad: buttons from the event's button state, whichever path it came by. */
    private fun mouseButtons(e: MotionEvent) {
        move(e)
        val now = if (e.actionMasked == MotionEvent.ACTION_CANCEL) 0 else e.buttonState
        val changed = now xor buttons
        for ((bit, btn) in BUTTONS) {
            if (changed and bit != 0) send(Gui.pointerButton(window, x, y, btn, now and bit != 0))
        }
        buttons = now
    }

    private fun touch(e: MotionEvent) {
        when (e.actionMasked) {
            MotionEvent.ACTION_DOWN -> {
                move(e)
                touching = true
                send(Gui.pointerButton(window, x, y, Gui.BTN_LEFT, true))
            }
            MotionEvent.ACTION_MOVE -> move(e)
            MotionEvent.ACTION_UP, MotionEvent.ACTION_CANCEL -> if (touching) {
                move(e)
                touching = false
                send(Gui.pointerButton(window, x, y, Gui.BTN_LEFT, false))
            }
        }
    }

    private fun scroll(e: MotionEvent, mouse: Boolean) {
        // Android's vertical axis is positive away from the user; Wayland's is down.
        val v = -e.getAxisValue(MotionEvent.AXIS_VSCROLL)
        val h = e.getAxisValue(MotionEvent.AXIS_HSCROLL)
        if (v == 0f && h == 0f) return
        val step = WHEEL_STEP * scale()
        if (mouse && e.getToolType(0) == MotionEvent.TOOL_TYPE_MOUSE) {
            send(Gui.pointerAxis(window, x, y, Gui.AXIS_WHEEL, 0, h * step, v * step, (h * 120).roundToInt(), (v * 120).roundToInt()))
        } else {
            send(Gui.pointerAxis(window, x, y, Gui.AXIS_CONTINUOUS, 0, h * step, v * step, 0, 0))
        }
    }

    /** A touchpad's two-finger swipe: Android moves a fake finger, which scrolls the content along with it. */
    private fun swipe(e: MotionEvent): Boolean {
        when (e.actionMasked) {
            MotionEvent.ACTION_DOWN -> {
                swipeX = e.x
                swipeY = e.y
            }
            MotionEvent.ACTION_MOVE -> {
                val dx = swipeX - e.x
                val dy = swipeY - e.y
                swipeX = e.x
                swipeY = e.y
                if (dx != 0f || dy != 0f) send(Gui.pointerAxis(window, x, y, Gui.AXIS_FINGER, 0, dx, dy, 0, 0))
            }
            MotionEvent.ACTION_UP, MotionEvent.ACTION_CANCEL -> send(Gui.pointerAxis(window, x, y, Gui.AXIS_FINGER, 3, 0f, 0f, 0, 0))
        }
        return true
    }

    /** A key event for the window; true when it went to the guest. */
    fun key(e: KeyEvent): Boolean {
        // Menu is a system key, but on a keyboard it's the guest's context-menu key.
        if (e.isSystem && e.keyCode != KeyEvent.KEYCODE_MENU) return false
        val code = EvdevKeys.code(e.keyCode, e.scanCode, e.deviceId == KeyCharacterMap.VIRTUAL_KEYBOARD)
        if (code == 0) return false
        // Clients repeat keys themselves.
        if (e.repeatCount > 0) return true
        send(Gui.key(window, code, e.action == KeyEvent.ACTION_DOWN))
        modifiers(e.metaState)
        return true
    }

    private fun modifiers(metaState: Int) {
        val m = EvdevKeys.mods(metaState)
        if (m == mods) return
        mods = m
        send(Gui.modifiers(window, m))
    }

    /** The window gained focus: the guest gets the last known modifiers, as Android has no state outside an event. */
    fun focused() {
        send(Gui.modifiers(window, mods.coerceAtLeast(0)))
    }

    fun release() {
        main.removeCallbacks(leave)
    }

    private companion object {
        const val LEAVE_DELAY_MS = 50L
        // Logical pixels per wheel notch, as libinput reports them.
        const val WHEEL_STEP = 15f
        val BUTTONS = listOf(
            MotionEvent.BUTTON_PRIMARY to Gui.BTN_LEFT,
            MotionEvent.BUTTON_SECONDARY to Gui.BTN_RIGHT,
            MotionEvent.BUTTON_TERTIARY to Gui.BTN_MIDDLE,
            MotionEvent.BUTTON_BACK to Gui.BTN_SIDE,
            MotionEvent.BUTTON_FORWARD to Gui.BTN_EXTRA,
        )
    }
}
