package dev.nickolaj.pocketnix.gui

import android.view.MotionEvent
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.nio.ByteBuffer

class TouchInputTest {
    private val window = 7
    private val frames = ArrayList<String>()
    private val send: (ByteArray) -> Unit = { frames += decode(it) }
    private val zoom = Zoom().apply { layout(1000, 2000, 1000, 2000) }
    private val timer = ManualTimer()
    private val slop = TouchSlop(slop = 10f, longPressMs = 400, tapMs = 300)

    /** A delay that runs only when the test says time passed. */
    private class ManualTimer : Timer {
        private val due = LinkedHashMap<Runnable, Long>()
        var now = 0L

        override fun schedule(delayMs: Long, r: Runnable) {
            due[r] = now + delayMs
        }

        override fun cancel(r: Runnable) {
            due.remove(r)
        }

        fun advance(ms: Long) {
            now += ms
            for ((r, at) in due.entries.toList()) if (at <= now) {
                due.remove(r)
                r.run()
            }
        }
    }

    /** The wire frames as short text, positions in window pixels. */
    private fun decode(f: ByteArray): String {
        val b = ByteBuffer.wrap(f)
        val type = b.get().toInt()
        b.getInt()
        assertEquals("every frame is for the window", window, b.getInt())
        fun xy() = "${b.getFloat().toInt()},${b.getFloat().toInt()}"
        return when (type) {
            Gui.POINTER_MOTION -> "motion ${xy()}"
            Gui.POINTER_BUTTON -> {
                val at = xy()
                val button = when (b.getInt()) {
                    Gui.BTN_LEFT -> "left"
                    Gui.BTN_RIGHT -> "right"
                    else -> "other"
                }
                "$button ${if (b.get().toInt() == 1) "down" else "up"} $at"
            }
            Gui.POINTER_AXIS -> {
                val at = xy()
                val source = b.get().toInt()
                val stop = b.get().toInt()
                "axis $source $at ${b.getFloat()},${b.getFloat()}${if (stop != 0) " stop" else ""}"
            }
            Gui.TOUCH_DOWN -> "touch-down ${b.getInt()} ${xy()}"
            Gui.TOUCH_MOTION -> "touch-motion ${b.getInt()} ${xy()}"
            Gui.TOUCH_UP -> "touch-up ${b.getInt()}"
            Gui.TOUCH_CANCEL -> "touch-cancel"
            Gui.TOUCH_FRAME -> "frame"
            else -> "0x${type.toString(16)}"
        }
    }

    // One finger, or two: (id, x, y) each.
    private var time = 0L

    private fun ev(action: Int, vararg points: Triple<Int, Float, Float>, index: Int = 0) = Touches(
        action, index, IntArray(points.size) { points[it].first }, FloatArray(points.size) { points[it].second },
        FloatArray(points.size) { points[it].third }, time,
    )

    private fun p(id: Int, x: Number, y: Number) = Triple(id, x.toFloat(), y.toFloat())

    private fun pointer() = PointerGestures(window, send, zoom, {}, timer, slop)

    @Test
    fun touchModeSendsEveryPointByItsId() {
        val m = TouchPoints(window, send, zoom)
        m.on(ev(MotionEvent.ACTION_DOWN, p(0, 100, 200)))
        m.on(ev(MotionEvent.ACTION_POINTER_DOWN, p(0, 100, 200), p(3, 300, 400), index = 1))
        m.on(ev(MotionEvent.ACTION_MOVE, p(0, 100, 200), p(3, 310, 400)))
        m.on(ev(MotionEvent.ACTION_POINTER_UP, p(0, 100, 200), p(3, 310, 400), index = 0))
        m.on(ev(MotionEvent.ACTION_UP, p(3, 310, 400)))
        assertEquals(
            listOf(
                "touch-down 0 100,200", "frame",
                "touch-down 3 300,400", "frame",
                // Only the point that moved.
                "touch-motion 3 310,400", "frame",
                "touch-up 0", "frame",
                "touch-up 3", "frame",
            ),
            frames,
        )
    }

    @Test
    fun aCancelledTouchEndsEveryPointAndGoesNowhere() {
        val m = TouchPoints(window, send, zoom)
        m.on(ev(MotionEvent.ACTION_DOWN, p(0, 100, 200)))
        frames.clear()
        // Android reports a cancel at 0, 0.
        m.on(ev(MotionEvent.ACTION_CANCEL, p(0, 0, 0)))
        assertEquals(listOf("touch-cancel"), frames)
    }

    @Test
    fun touchesMapThroughTheZoom() {
        zoom.zoomBy(2f, 0f, 0f)
        val m = TouchPoints(window, send, zoom)
        m.on(ev(MotionEvent.ACTION_DOWN, p(0, 100, 200)))
        assertEquals("touch-down 0 50,100", frames.first())
    }

    @Test
    fun aTapIsALeftClickWhereTheFingerIs() {
        val g = pointer()
        g.on(ev(MotionEvent.ACTION_DOWN, p(0, 100, 200)))
        g.on(ev(MotionEvent.ACTION_MOVE, p(0, 103, 202)))
        g.on(ev(MotionEvent.ACTION_UP, p(0, 103, 202)))
        assertEquals(listOf("motion 100,200", "left down 100,200", "left up 100,200"), frames)
    }

    @Test
    fun aLongPressIsARightClickAndNothingElse() {
        val g = pointer()
        g.on(ev(MotionEvent.ACTION_DOWN, p(0, 100, 200)))
        timer.advance(400)
        g.on(ev(MotionEvent.ACTION_UP, p(0, 100, 200)))
        assertEquals(listOf("motion 100,200", "right down 100,200", "right up 100,200"), frames)
    }

    @Test
    fun aDragHoldsTheLeftButton() {
        val g = pointer()
        g.on(ev(MotionEvent.ACTION_DOWN, p(0, 100, 200)))
        g.on(ev(MotionEvent.ACTION_MOVE, p(0, 150, 200)))
        timer.advance(1000)
        g.on(ev(MotionEvent.ACTION_MOVE, p(0, 200, 200)))
        g.on(ev(MotionEvent.ACTION_UP, p(0, 200, 200)))
        assertEquals(
            listOf("motion 100,200", "left down 100,200", "motion 150,200", "motion 200,200", "left up 200,200"),
            frames,
        )
    }

    @Test
    fun aCancelledDragLetsGoWhereItWas() {
        val g = pointer()
        g.on(ev(MotionEvent.ACTION_DOWN, p(0, 100, 200)))
        g.on(ev(MotionEvent.ACTION_MOVE, p(0, 150, 200)))
        g.on(ev(MotionEvent.ACTION_CANCEL, p(0, 0, 0)))
        assertEquals("left up 150,200", frames.last())
        assertTrue(frames.none { it.startsWith("motion 0,0") })
    }

    @Test
    fun twoFingersDraggingScrollAndStopWhenLifted() {
        val g = pointer()
        g.on(ev(MotionEvent.ACTION_DOWN, p(0, 100, 500)))
        g.on(ev(MotionEvent.ACTION_POINTER_DOWN, p(0, 100, 500), p(1, 200, 500), index = 1))
        // Up by 50, then 30: the content follows, so it scrolls down.
        g.on(ev(MotionEvent.ACTION_MOVE, p(0, 100, 450), p(1, 200, 450)))
        g.on(ev(MotionEvent.ACTION_MOVE, p(0, 100, 420), p(1, 200, 420)))
        g.on(ev(MotionEvent.ACTION_POINTER_UP, p(0, 100, 420), p(1, 200, 420), index = 1))
        g.on(ev(MotionEvent.ACTION_UP, p(0, 100, 420)))
        assertEquals(
            listOf("motion 100,500", "axis 1 100,500 0.0,50.0", "axis 1 100,500 0.0,30.0", "axis 1 100,500 0.0,0.0 stop"),
            frames,
        )
        assertEquals(1f, zoom.scale, 0f)
    }

    @Test
    fun twoFingersSpreadingZoomTheViewAndSendNothing() {
        val g = pointer()
        g.on(ev(MotionEvent.ACTION_DOWN, p(0, 400, 1000)))
        g.on(ev(MotionEvent.ACTION_POINTER_DOWN, p(0, 400, 1000), p(1, 600, 1000), index = 1))
        g.on(ev(MotionEvent.ACTION_MOVE, p(0, 300, 1000), p(1, 700, 1000)))
        g.on(ev(MotionEvent.ACTION_MOVE, p(0, 200, 1000), p(1, 800, 1000)))
        g.on(ev(MotionEvent.ACTION_POINTER_UP, p(0, 200, 1000), p(1, 800, 1000), index = 1))
        g.on(ev(MotionEvent.ACTION_UP, p(0, 200, 1000)))
        assertTrue("zoomed in: ${zoom.scale}", zoom.scale > 1.5f)
        assertEquals(listOf("motion 400,1000"), frames)
    }

    @Test
    fun aTwoFingerTapIsARightClick() {
        val g = pointer()
        g.on(ev(MotionEvent.ACTION_DOWN, p(0, 100, 200)))
        g.on(ev(MotionEvent.ACTION_POINTER_DOWN, p(0, 100, 200), p(1, 160, 200), index = 1))
        time += 100
        g.on(ev(MotionEvent.ACTION_POINTER_UP, p(0, 100, 200), p(1, 160, 200), index = 1))
        g.on(ev(MotionEvent.ACTION_UP, p(0, 100, 200)))
        assertEquals(listOf("motion 100,200", "right down 100,200", "right up 100,200"), frames)
    }

    private fun trackpad(moved: () -> Unit = {}) = TrackpadGestures(window, send, zoom, {}, timer, slop, { 1000 to 2000 }, moved)

    @Test
    fun theTrackpadMovesTheCursorByTheFingersMotionAndClicksThere() {
        var moves = 0
        val g = trackpad { moves++ }
        g.on(ev(MotionEvent.ACTION_DOWN, p(0, 100, 100)))
        g.on(ev(MotionEvent.ACTION_MOVE, p(0, 120, 100)))
        g.on(ev(MotionEvent.ACTION_MOVE, p(0, 140, 110)))
        g.on(ev(MotionEvent.ACTION_UP, p(0, 140, 110)))
        // It starts at the window's centre and moves 1.5 pixels per finger pixel.
        assertEquals(listOf("motion 500,1000", "motion 530,1000", "motion 560,1015"), frames)
        assertTrue(moves >= 3)
        frames.clear()
        g.on(ev(MotionEvent.ACTION_DOWN, p(0, 900, 1900)))
        g.on(ev(MotionEvent.ACTION_UP, p(0, 900, 1900)))
        assertEquals("a tap clicks at the cursor, not the finger", listOf("left down 560,1015", "left up 560,1015"), frames)
    }

    @Test
    fun theTrackpadCursorStaysInTheWindow() {
        val g = trackpad()
        g.on(ev(MotionEvent.ACTION_DOWN, p(0, 0, 0)))
        g.on(ev(MotionEvent.ACTION_MOVE, p(0, -5000, 5000)))
        assertEquals(0f to 1999f, g.x to g.y)
    }

    @Test
    fun aLongPressThenAMoveDragsWithTheTrackpad() {
        val g = trackpad()
        g.on(ev(MotionEvent.ACTION_DOWN, p(0, 100, 100)))
        timer.advance(400)
        g.on(ev(MotionEvent.ACTION_MOVE, p(0, 140, 100)))
        g.on(ev(MotionEvent.ACTION_UP, p(0, 140, 100)))
        assertEquals(listOf("motion 500,1000", "left down 500,1000", "motion 560,1000", "left up 560,1000"), frames)
    }
}
