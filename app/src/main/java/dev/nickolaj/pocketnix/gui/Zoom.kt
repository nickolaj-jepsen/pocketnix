package dev.nickolaj.pocketnix.gui

import android.view.MotionEvent
import kotlin.math.hypot
import kotlin.math.max
import kotlin.math.min

/**
 * A window's visual zoom (gui-apps.md, Scale): its layers magnified by
 * [scale] and moved so the window's origin shows at ([x], [y]) in the view.
 * The client's layout never changes; input maps back through [windowX] and
 * [windowY]. The content may be larger than the view (a window clipped at
 * the fit's floor), and then panning reaches all of it and zooming out
 * shows it whole. Main thread only.
 */
class Zoom {
    var scale = 1f
        private set
    var x = 0f
        private set
    var y = 0f
        private set
    private var viewWidth = 0
    private var viewHeight = 0
    private var contentWidth = 0
    private var contentHeight = 0

    /** Whether the view shows anything but the window's top-left corner at 1:1. */
    val zoomed: Boolean get() = scale != 1f || x != 0f || y != 0f

    /** The view's size, and the window content's (at least the view's), in pixels; returns whether the zoom moved. */
    fun layout(viewWidth: Int, viewHeight: Int, contentWidth: Int, contentHeight: Int): Boolean {
        this.viewWidth = viewWidth
        this.viewHeight = viewHeight
        this.contentWidth = max(contentWidth, viewWidth)
        this.contentHeight = max(contentHeight, viewHeight)
        return moves {}
    }

    /** Magnifies by [factor] around the view point ([focusX], [focusY]), which stays put unless a limit is reached. */
    fun zoomBy(factor: Float, focusX: Float, focusY: Float): Boolean = moves {
        if (factor > 0f && factor.isFinite()) {
            val s = (scale * factor).coerceIn(minScale(), MAX_SCALE)
            val f = s / scale
            x = focusX - (focusX - x) * f
            y = focusY - (focusY - y) * f
            scale = s
        }
    }

    /** Moves the content by ([dx], [dy]) view pixels, as far as it goes. */
    fun panBy(dx: Float, dy: Float): Boolean = moves {
        x += dx
        y += dy
    }

    fun reset(): Boolean = moves {
        scale = 1f
        x = 0f
        y = 0f
    }

    private inline fun moves(change: () -> Unit): Boolean {
        val s = scale
        val ox = x
        val oy = y
        change()
        clamp()
        return s != scale || ox != x || oy != y
    }

    /** A view position's x in window pixels. */
    fun windowX(viewX: Float): Float = (viewX - x) / scale

    fun windowY(viewY: Float): Float = (viewY - y) / scale

    // Out far enough to see a clipped window whole, never smaller than 1:1 otherwise.
    private fun minScale(): Float {
        if (viewWidth <= 0 || viewHeight <= 0) return 1f
        return min(1f, min(viewWidth.toFloat() / contentWidth, viewHeight.toFloat() / contentHeight))
    }

    /** Keeps the view covered, or the content centred where it's smaller. */
    private fun clamp() {
        scale = scale.coerceIn(minScale(), MAX_SCALE)
        x = axis(x, viewWidth, contentWidth * scale)
        y = axis(y, viewHeight, contentHeight * scale)
    }

    private fun axis(pos: Float, view: Int, extent: Float): Float =
        if (extent >= view) pos.coerceIn(view - extent, 0f) else (view - extent) / 2

    companion object {
        const val MAX_SCALE = 5f
    }
}

/**
 * Pinch and pan for a [Zoom]: two fingers zoom around their midpoint, and
 * any number of fingers drag the content. Consumes every event it's given.
 */
internal class ZoomGestures(private val zoom: Zoom, private val changed: () -> Unit) {
    private var lastX = 0f
    private var lastY = 0f
    private var lastSpan = 0f

    fun onTouch(e: MotionEvent): Boolean {
        when (e.actionMasked) {
            MotionEvent.ACTION_DOWN, MotionEvent.ACTION_POINTER_DOWN -> track(e, skip = -1)
            // The lifting finger is still in this event; leave it out, or the content jumps.
            MotionEvent.ACTION_POINTER_UP -> track(e, skip = e.actionIndex)
            MotionEvent.ACTION_MOVE -> {
                val fx = focusX(e, -1)
                val fy = focusY(e, -1)
                val span = span(e, fx, fy, -1)
                var moved = zoom.panBy(fx - lastX, fy - lastY)
                if (lastSpan > 0f && span > 0f) moved = zoom.zoomBy(span / lastSpan, fx, fy) || moved
                lastX = fx
                lastY = fy
                lastSpan = span
                if (moved) changed()
            }
        }
        return true
    }

    private fun track(e: MotionEvent, skip: Int) {
        lastX = focusX(e, skip)
        lastY = focusY(e, skip)
        lastSpan = span(e, lastX, lastY, skip)
    }

    private fun focusX(e: MotionEvent, skip: Int): Float = average(e, skip) { e.getX(it) }

    private fun focusY(e: MotionEvent, skip: Int): Float = average(e, skip) { e.getY(it) }

    /** The fingers' mean distance from their centre; 0 for one finger. */
    private fun span(e: MotionEvent, fx: Float, fy: Float, skip: Int): Float {
        val n = e.pointerCount - if (skip >= 0) 1 else 0
        if (n < 2) return 0f
        return average(e, skip) { hypot(e.getX(it) - fx, e.getY(it) - fy) }
    }

    private inline fun average(e: MotionEvent, skip: Int, value: (Int) -> Float): Float {
        var sum = 0f
        var n = 0
        for (i in 0 until e.pointerCount) {
            if (i == skip) continue
            sum += value(i)
            n++
        }
        return if (n == 0) 0f else sum / n
    }
}
