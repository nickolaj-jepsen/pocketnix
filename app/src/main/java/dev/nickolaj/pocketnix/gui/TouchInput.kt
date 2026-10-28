package dev.nickolaj.pocketnix.gui

import android.view.MotionEvent
import kotlin.math.abs
import kotlin.math.hypot

/** One touchscreen MotionEvent as the gestures read it: positions in view pixels. */
internal class Touches(
    /** MotionEvent's masked action. */
    val action: Int,
    /** The pointer the action is about, for POINTER_DOWN and POINTER_UP. */
    val index: Int,
    val ids: IntArray,
    val xs: FloatArray,
    val ys: FloatArray,
    val time: Long,
) {
    val count: Int get() = ids.size

    companion object {
        fun of(e: MotionEvent): Touches {
            val n = e.pointerCount
            return Touches(
                e.actionMasked, e.actionIndex,
                IntArray(n) { e.getPointerId(it) }, FloatArray(n) { e.getX(it) }, FloatArray(n) { e.getY(it) }, e.eventTime,
            )
        }
    }
}

/** Delayed work for the gestures: the main looper, or a test's clock. */
internal interface Timer {
    fun schedule(delayMs: Long, r: Runnable)

    fun cancel(r: Runnable)
}

/** What one input mode does with touches. */
internal interface TouchMode {
    fun on(t: Touches)
}

/**
 * Touch mode (gui-protocol.md, Touch): every point as a Wayland touch
 * point, by Android's pointer id, at its window position.
 */
internal class TouchPoints(private val window: Int, private val send: (ByteArray) -> Unit, private val zoom: Zoom) : TouchMode {
    // The window position last sent for each point that's down.
    private val down = HashMap<Int, Pair<Float, Float>>()

    override fun on(t: Touches) {
        when (t.action) {
            MotionEvent.ACTION_DOWN, MotionEvent.ACTION_POINTER_DOWN -> {
                val id = t.ids[t.index]
                val p = at(t, t.index)
                down[id] = p
                send(Gui.touchDown(window, id, p.first, p.second))
                send(Gui.touchFrame(window))
            }
            MotionEvent.ACTION_MOVE -> {
                var moved = false
                for (i in 0 until t.count) {
                    val id = t.ids[i]
                    val last = down[id] ?: continue
                    val p = at(t, i)
                    if (p == last) continue
                    down[id] = p
                    send(Gui.touchMotion(window, id, p.first, p.second))
                    moved = true
                }
                if (moved) send(Gui.touchFrame(window))
            }
            MotionEvent.ACTION_UP, MotionEvent.ACTION_POINTER_UP -> {
                val id = t.ids[t.index]
                val last = down.remove(id) ?: return
                val p = at(t, t.index)
                if (p != last) send(Gui.touchMotion(window, id, p.first, p.second))
                send(Gui.touchUp(window, id))
                send(Gui.touchFrame(window))
            }
            // A system gesture took the touch; its positions mean nothing.
            MotionEvent.ACTION_CANCEL -> if (down.isNotEmpty()) {
                down.clear()
                send(Gui.touchCancel(window))
            }
        }
    }

    private fun at(t: Touches, i: Int): Pair<Float, Float> = zoom.windowX(t.xs[i]) to zoom.windowY(t.ys[i])
}

/**
 * Two or more fingers in Pointer and Trackpad modes: a drag scrolls the
 * window at [scrollAt], a pinch zooms and pans the view (gui-apps.md,
 * Input). Which one is settled by the first movement past the slop.
 */
internal class TwoFingers(
    private val window: Int,
    private val send: (ByteArray) -> Unit,
    private val zoom: Zoom,
    private val zoomed: () -> Unit,
    private val slop: Float,
) {
    private enum class Kind { UNDECIDED, SCROLL, PINCH }

    private var kind = Kind.UNDECIDED
    private var start = Spread.NONE
    private var last = Spread.NONE
    private var at = 0f to 0f

    /** Whether the fingers did anything yet: a two-finger tap didn't. */
    val moved: Boolean get() = kind != Kind.UNDECIDED

    /** Fingers went down; scrolling goes to window position [scrollAt]. */
    fun start(t: Touches, scrollAt: Pair<Float, Float>) {
        kind = Kind.UNDECIDED
        at = scrollAt
        track(t, -1)
    }

    /** A finger came or went ([skip]: the one lifting): carry on from where the rest are. */
    fun track(t: Touches, skip: Int) {
        last = Spread.of(t, skip)
        if (kind == Kind.UNDECIDED) start = last
    }

    fun move(t: Touches) {
        val now = Spread.of(t)
        if (kind == Kind.UNDECIDED) {
            kind = when {
                abs(now.span - start.span) > slop -> Kind.PINCH
                hypot(now.x - start.x, now.y - start.y) > slop -> Kind.SCROLL
                else -> return
            }
        }
        when (kind) {
            Kind.SCROLL -> {
                // The content follows the fingers: up scrolls down.
                val dx = (last.x - now.x) / zoom.scale
                val dy = (last.y - now.y) / zoom.scale
                if (dx != 0f || dy != 0f) send(Gui.pointerAxis(window, at.first, at.second, Gui.AXIS_FINGER, 0, dx, dy, 0, 0))
            }
            Kind.PINCH -> if (last.pinch(zoom, now)) zoomed()
            Kind.UNDECIDED -> {}
        }
        last = now
    }

    /** The fingers lifted, or a system gesture took them. */
    fun end() {
        if (kind == Kind.SCROLL) send(Gui.pointerAxis(window, at.first, at.second, Gui.AXIS_FINGER, 3, 0f, 0f, 0, 0))
        kind = Kind.UNDECIDED
    }
}

/** The fingers' centre, in view pixels, and their mean distance from it: 0 for one finger. */
internal data class Spread(val x: Float, val y: Float, val span: Float) {
    /** Pans and zooms [zoom] as the fingers went from here to [to]: the content follows them; whether it moved. */
    fun pinch(zoom: Zoom, to: Spread): Boolean {
        var changed = zoom.panBy(to.x - x, to.y - y)
        if (span > 0f && to.span > 0f) changed = zoom.zoomBy(to.span / span, to.x, to.y) || changed
        return changed
    }

    companion object {
        val NONE = Spread(0f, 0f, 0f)

        /** [t]'s fingers but the one at [skip]: one lifting is still in its event, and counting it makes the content jump. */
        fun of(t: Touches, skip: Int = -1): Spread {
            var n = 0
            var sx = 0f
            var sy = 0f
            for (i in 0 until t.count) {
                if (i == skip) continue
                sx += t.xs[i]
                sy += t.ys[i]
                n++
            }
            if (n == 0) return NONE
            val x = sx / n
            val y = sy / n
            if (n < 2) return Spread(x, y, 0f)
            var d = 0f
            for (i in 0 until t.count) if (i != skip) d += hypot(t.xs[i] - x, t.ys[i] - y)
            return Spread(x, y, d / n)
        }
    }
}

/** How far a finger may move and still tap, and how long a press is long. */
internal class TouchSlop(val slop: Float, val longPressMs: Long, val tapMs: Long)

/**
 * Pointer mode: the finger marks the spot. A tap clicks there, a long press
 * right-clicks, a drag drags with the left button, and two fingers scroll
 * or pinch (gui-protocol.md, Pointer and Trackpad modes).
 */
internal class PointerGestures(
    private val window: Int,
    private val send: (ByteArray) -> Unit,
    private val zoom: Zoom,
    zoomed: () -> Unit,
    private val timer: Timer,
    private val config: TouchSlop,
) : TouchMode {
    private enum class State { IDLE, PRESSED, DRAGGING, LONG, TWO, DONE }

    private var state = State.IDLE
    private var downX = 0f
    private var downY = 0f
    private var x = 0f
    private var y = 0f
    private var twoAt = 0L
    private val two = TwoFingers(window, send, zoom, zoomed, config.slop)
    private val longPress = Runnable {
        if (state == State.PRESSED) {
            state = State.LONG
            click(Gui.BTN_RIGHT)
        }
    }

    override fun on(t: Touches) {
        when (t.action) {
            MotionEvent.ACTION_DOWN -> {
                downX = t.xs[0]
                downY = t.ys[0]
                moveTo(t, 0)
                state = State.PRESSED
                timer.schedule(config.longPressMs, longPress)
            }
            MotionEvent.ACTION_POINTER_DOWN -> {
                timer.cancel(longPress)
                if (state == State.DRAGGING) send(Gui.pointerButton(window, x, y, Gui.BTN_LEFT, false))
                if (state == State.TWO) {
                    two.track(t, -1)
                } else if (state != State.DONE) {
                    state = State.TWO
                    twoAt = t.time
                    two.start(t, x to y)
                }
            }
            MotionEvent.ACTION_MOVE -> when (state) {
                State.PRESSED -> if (hypot(t.xs[0] - downX, t.ys[0] - downY) > config.slop) {
                    timer.cancel(longPress)
                    state = State.DRAGGING
                    send(Gui.pointerButton(window, x, y, Gui.BTN_LEFT, true))
                    moveTo(t, 0)
                }
                State.DRAGGING -> moveTo(t, 0)
                State.TWO -> two.move(t)
                else -> {}
            }
            MotionEvent.ACTION_POINTER_UP -> if (state == State.TWO) {
                if (t.count > 2) {
                    two.track(t, t.index)
                } else {
                    // A two-finger tap is a right click too.
                    if (!two.moved && t.time - twoAt <= config.tapMs) click(Gui.BTN_RIGHT)
                    two.end()
                    state = State.DONE
                }
            }
            MotionEvent.ACTION_UP -> {
                timer.cancel(longPress)
                when (state) {
                    State.PRESSED -> click(Gui.BTN_LEFT)
                    State.DRAGGING -> {
                        moveTo(t, 0)
                        send(Gui.pointerButton(window, x, y, Gui.BTN_LEFT, false))
                    }
                    State.TWO -> two.end()
                    else -> {}
                }
                state = State.IDLE
            }
            // A system gesture took the touch: let go without acting, and nowhere new.
            MotionEvent.ACTION_CANCEL -> {
                timer.cancel(longPress)
                when (state) {
                    State.DRAGGING -> send(Gui.pointerButton(window, x, y, Gui.BTN_LEFT, false))
                    State.TWO -> two.end()
                    else -> {}
                }
                state = State.IDLE
            }
        }
    }

    private fun moveTo(t: Touches, i: Int) {
        val wx = zoom.windowX(t.xs[i])
        val wy = zoom.windowY(t.ys[i])
        if (wx == x && wy == y && state != State.IDLE) return
        x = wx
        y = wy
        send(Gui.pointerMotion(window, x, y))
    }

    private fun click(button: Int) {
        send(Gui.pointerButton(window, x, y, button, true))
        send(Gui.pointerButton(window, x, y, button, false))
    }
}

/**
 * Trackpad mode: a cursor the finger moves by its relative motion, which
 * the app draws. A tap clicks at the cursor; a long press right-clicks, or,
 * moved after, drags with the left button; two fingers scroll there or
 * pinch, and a two-finger tap right-clicks.
 */
internal class TrackpadGestures(
    private val window: Int,
    private val send: (ByteArray) -> Unit,
    private val zoom: Zoom,
    zoomed: () -> Unit,
    private val timer: Timer,
    private val config: TouchSlop,
    /** The window's size in window pixels, which the cursor stays within. */
    private val bounds: () -> Pair<Int, Int>,
    /** The cursor moved, or appeared. */
    private val cursorMoved: () -> Unit,
) : TouchMode {
    private enum class State { IDLE, PRESSED, MOVING, LONG, DRAGGING, TWO, DONE }

    private var state = State.IDLE
    private var downX = 0f
    private var downY = 0f
    private var lastX = 0f
    private var lastY = 0f
    private var twoAt = 0L
    private val two = TwoFingers(window, send, zoom, zoomed, config.slop)

    /** The cursor in window pixels; NaN until first placed. */
    var x = Float.NaN
        private set
    var y = Float.NaN
        private set

    private val longPress = Runnable { if (state == State.PRESSED) state = State.LONG }

    /** Puts the cursor at the window's centre if it has no place yet. */
    fun place() {
        if (!x.isNaN()) return
        val (w, h) = bounds()
        x = w / 2f
        y = h / 2f
        send(Gui.pointerMotion(window, x, y))
        cursorMoved()
    }

    override fun on(t: Touches) {
        when (t.action) {
            MotionEvent.ACTION_DOWN -> {
                place()
                downX = t.xs[0]
                downY = t.ys[0]
                lastX = downX
                lastY = downY
                state = State.PRESSED
                timer.schedule(config.longPressMs, longPress)
            }
            MotionEvent.ACTION_POINTER_DOWN -> {
                timer.cancel(longPress)
                if (state == State.DRAGGING) send(Gui.pointerButton(window, x, y, Gui.BTN_LEFT, false))
                if (state == State.TWO) {
                    two.track(t, -1)
                } else if (state != State.DONE) {
                    state = State.TWO
                    twoAt = t.time
                    two.start(t, x to y)
                }
            }
            MotionEvent.ACTION_MOVE -> when (state) {
                State.PRESSED, State.LONG -> if (hypot(t.xs[0] - downX, t.ys[0] - downY) > config.slop) {
                    timer.cancel(longPress)
                    if (state == State.LONG) {
                        state = State.DRAGGING
                        send(Gui.pointerButton(window, x, y, Gui.BTN_LEFT, true))
                    } else {
                        state = State.MOVING
                    }
                    follow(t)
                }
                State.MOVING, State.DRAGGING -> follow(t)
                State.TWO -> two.move(t)
                else -> {}
            }
            MotionEvent.ACTION_POINTER_UP -> if (state == State.TWO) {
                if (t.count > 2) {
                    two.track(t, t.index)
                } else {
                    if (!two.moved && t.time - twoAt <= config.tapMs) click(Gui.BTN_RIGHT)
                    two.end()
                    state = State.DONE
                }
            }
            MotionEvent.ACTION_UP -> {
                timer.cancel(longPress)
                when (state) {
                    State.PRESSED -> click(Gui.BTN_LEFT)
                    State.LONG -> click(Gui.BTN_RIGHT)
                    State.DRAGGING -> send(Gui.pointerButton(window, x, y, Gui.BTN_LEFT, false))
                    State.TWO -> two.end()
                    else -> {}
                }
                state = State.IDLE
            }
            MotionEvent.ACTION_CANCEL -> {
                timer.cancel(longPress)
                when (state) {
                    State.DRAGGING -> send(Gui.pointerButton(window, x, y, Gui.BTN_LEFT, false))
                    State.TWO -> two.end()
                    else -> {}
                }
                state = State.IDLE
            }
        }
    }

    /** Moves the cursor by the finger's motion since the last event. */
    private fun follow(t: Touches) {
        val dx = (t.xs[0] - lastX) / zoom.scale * SPEED
        val dy = (t.ys[0] - lastY) / zoom.scale * SPEED
        lastX = t.xs[0]
        lastY = t.ys[0]
        val (w, h) = bounds()
        val nx = (x + dx).coerceIn(0f, (w - 1).coerceAtLeast(0).toFloat())
        val ny = (y + dy).coerceIn(0f, (h - 1).coerceAtLeast(0).toFloat())
        if (nx == x && ny == y) return
        x = nx
        y = ny
        send(Gui.pointerMotion(window, x, y))
        cursorMoved()
    }

    private fun click(button: Int) {
        send(Gui.pointerButton(window, x, y, button, true))
        send(Gui.pointerButton(window, x, y, button, false))
    }

    private companion object {
        // Window pixels per finger pixel at 1:1 zoom: a phone's screen is a small pad.
        const val SPEED = 1.5f
    }
}
