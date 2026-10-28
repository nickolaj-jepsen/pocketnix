package dev.nickolaj.pocketnix.gui

import android.content.Context
import android.graphics.Rect
import android.os.Handler
import android.os.Looper
import android.view.InputDevice
import android.view.MotionEvent
import android.view.SurfaceControl
import android.view.SurfaceHolder
import android.view.SurfaceView
import android.view.WindowInsets
import kotlin.math.max

/**
 * The view a guest window shows in, for either host: its layers, its size
 * and scale (the size preset and the automatic fit), its zoom, and the
 * touches and mouse events that reach it. Main thread only.
 */
class WindowSurface internal constructor(
    context: Context,
    private val session: GuiSession,
    private val host: WindowHost,
    /** Whether [host] still shows the window: a dismissed host's surface tells the session nothing. */
    private val bound: () -> Boolean,
    /** The host's part of the `window` message, given whether the surface exists. */
    private val flags: (attached: Boolean) -> Flags,
) {
    data class Flags(val focused: Boolean, val visible: Boolean, val maximized: Boolean, val fullscreen: Boolean = false)

    private val id = host.windowId
    private val main = Handler(Looper.getMainLooper())
    private val push = Runnable { pushState() }
    val view = SurfaceView(context)
    val zoom = Zoom()
    private val gestures = ZoomGestures(zoom) { place() }

    /** The window's scale as last sent: pixels per logical pixel. */
    var scale = density
        private set
    internal val input = WindowInput(id, session::send, { scale }, zoom)

    /** The view's size in pixels: the window's, before zoom. */
    var width = 0
        private set
    var height = 0
        private set
    var attached = false
        private set
    private var layers: Layers? = null

    /** Touch mode's zoom toggle: touches pan and pinch the view instead of reaching the app. */
    var zooming = false

    /** Pixels the phone keyboard takes off the view's height: the fit measures the window without it. */
    var keyboardHeight = 0
        set(v) {
            if (field == v) return
            field = v
            pushState()
        }

    /** While set, a swipe up from the view's bottom edge calls it, and that touch never reaches the app. */
    var onEdgeSwipe: (() -> Unit)? = null
    private var edge = Edge.NONE
    private var edgeStartY = 0f

    private enum class Edge { NONE, TRACKING, DONE }

    private val density: Float get() = view.resources.displayMetrics.densityDpi / 160f

    private val callback = object : SurfaceHolder.Callback {
        override fun surfaceCreated(holder: SurfaceHolder) {}

        override fun surfaceChanged(holder: SurfaceHolder, format: Int, w: Int, h: Int) = guard("window ${id.u}: surface") {
            val resized = w != width || h != height
            width = w
            height = h
            attached = true
            val sc = view.surfaceControl
            if (layers?.parent !== sc) {
                layers?.release()
                layers = Layers(sc)
            }
            relayout()
            // Docked moves and resizes come as two sizes in a row (the caption inset lands second), and
            // the keyboard resizes a tab in steps.
            main.removeCallbacks(push)
            if (resized) main.postDelayed(push, RESIZE_DEBOUNCE_MS) else pushState()
        }

        override fun surfaceDestroyed(holder: SurfaceHolder) = guard("window ${id.u}: surface gone") {
            attached = false
            if (bound()) session.detach(host)
            layers?.release()
            layers = null
            main.removeCallbacks(push)
            pushState()
        }
    }

    init {
        view.apply {
            holder.addCallback(callback)
            isFocusable = true
            isFocusableInTouchMode = true
            // A hardware key leaves touch mode, and the focus highlight would tint the whole window.
            defaultFocusHighlightEnabled = false
            setOnTouchListener { v, e ->
                // The listener consumes the touch, so the view never takes focus itself.
                if (!v.isFocused) v.requestFocus()
                guard("window ${id.u}: touch", false) { bound() && touch(e) }
            }
            setOnGenericMotionListener { _, e -> guard("window ${id.u}: motion", false) { bound() && input.motion(e) } }
        }
    }

    private fun touch(e: MotionEvent): Boolean {
        if (edgeSwipe(e)) return true
        // A mouse reaches the app in every mode.
        if (zooming && !e.isFromSource(InputDevice.SOURCE_MOUSE)) return gestures.onTouch(e)
        return input.motion(e)
    }

    /** Pinch and pan on the view, for input modes where those are pocketnix's; consumes [e]. */
    internal fun zoomGesture(e: MotionEvent): Boolean = gestures.onTouch(e)

    fun resetZoom() {
        if (zoom.reset()) place()
    }

    private fun edgeSwipe(e: MotionEvent): Boolean {
        when (e.actionMasked) {
            MotionEvent.ACTION_DOWN -> {
                val zone = EDGE_DP * view.resources.displayMetrics.density +
                    (view.rootWindowInsets?.getInsets(WindowInsets.Type.mandatorySystemGestures())?.bottom ?: 0)
                edge = if (onEdgeSwipe != null && e.y >= height - zone) Edge.TRACKING else Edge.NONE
                edgeStartY = e.y
            }
            MotionEvent.ACTION_MOVE -> if (edge == Edge.TRACKING && edgeStartY - e.y > EDGE_DP * view.resources.displayMetrics.density) {
                edge = Edge.DONE
                onEdgeSwipe?.invoke()
            }
            MotionEvent.ACTION_UP, MotionEvent.ACTION_CANCEL -> if (edge != Edge.NONE) {
                edge = Edge.NONE
                return true
            }
        }
        return edge != Edge.NONE
    }

    /** The toplevel or its settings changed: its size, minimum or preset. */
    fun refresh() {
        relayout()
        main.removeCallbacks(push)
        pushState()
    }

    /** Sends the window's state, with the scale its preset and the fit give it. */
    fun pushState() {
        if (!bound()) return
        val f = flags(attached)
        scale = session.fitScale(id, width, height + keyboardHeight, density)
        session.windowState(
            host,
            // Hidden, the window keeps its last size: the client needn't redraw at another for nobody.
            WindowState(id, width, height, scale, f.focused, f.visible, f.maximized, f.fullscreen),
        )
    }

    /** Fits the zoom to the view and the content, which may be larger when the client's minimum is. */
    private fun relayout() {
        val l = layers ?: return
        val (cw, ch) = session.contentSize(id)
        zoom.layout(width, height, cw, ch)
        place()
        if (bound()) session.attach(host, l.zoom, max(width, cw), max(height, ch))
    }

    private fun place() {
        val l = layers ?: return
        SurfaceControl.Transaction().also { l.place(it, width, height, zoom) }.apply()
    }

    /** The host stops showing the window. */
    fun release() {
        main.removeCallbacks(push)
        input.release()
    }

    /**
     * Two layers between the view's surface and the window's own: one clips
     * to the view, the other carries the zoom, so a zoom never waits on the
     * session's lock.
     */
    private class Layers(val parent: SurfaceControl) {
        private val clip = SurfaceControl.Builder().setName("gui-clip").setParent(parent).build()
        val zoom: SurfaceControl = SurfaceControl.Builder().setName("gui-zoom").setParent(clip).build()

        fun place(t: SurfaceControl.Transaction, width: Int, height: Int, z: Zoom) {
            t.setCrop(clip, Rect(0, 0, width, height))
            t.setScale(zoom, z.scale, z.scale)
            t.setPosition(zoom, z.x, z.y)
            t.setVisibility(clip, true)
            t.setVisibility(zoom, true)
        }

        fun release() {
            SurfaceControl.Transaction().reparent(clip, null).apply()
            zoom.release()
            clip.release()
        }
    }

    private companion object {
        const val RESIZE_DEBOUNCE_MS = 150L
        // How far above the system's own gesture area a swipe may start, and how far up it must go.
        const val EDGE_DP = 32
    }
}
