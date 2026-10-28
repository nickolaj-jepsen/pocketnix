package dev.nickolaj.pocketnix.gui

import android.content.Context
import android.graphics.Rect
import android.os.Handler
import android.os.Looper
import android.view.InputDevice
import android.view.KeyEvent
import android.view.MotionEvent
import android.view.SurfaceControl
import android.view.SurfaceHolder
import android.view.SurfaceView
import android.view.View
import android.view.ViewConfiguration
import android.view.WindowInsets
import android.view.inputmethod.EditorInfo
import android.view.inputmethod.InputConnection
import android.widget.FrameLayout
import kotlin.math.max

/**
 * The view a guest window shows in, for either host: its layers, its size
 * and scale (the size preset and the automatic fit), its zoom, the touches,
 * mouse events and keys that reach it, and its phone keyboard. Main thread
 * only.
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
    val view: SurfaceView = GuiView(context)
    private val cursorView = TrackpadCursor(context).apply { visibility = View.GONE }

    /** What a host puts on screen: the window's view, with Trackpad mode's cursor over it. */
    val root: FrameLayout = FrameLayout(context).apply {
        addView(view, FrameLayout.LayoutParams(FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT))
        addView(cursorView)
    }
    val zoom = Zoom()
    private val gestures = ZoomGestures(zoom) { place() }

    /** The window's scale as last sent: pixels per logical pixel. */
    var scale = density
        private set
    internal val input = WindowInput(id, session::send, { scale }, zoom)
    internal val ime = WindowIme(view, id, session, input, zoom)

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

    /** Pixels the phone keyboard (and the row above it) takes off the view's height: the fit measures the window without it. */
    var keyboardHeight = 0
        set(v) {
            if (field == v) return
            field = v
            // It comes with a resize, which the push waits out.
            main.removeCallbacks(push)
            main.postDelayed(push, RESIZE_DEBOUNCE_MS)
        }

    /** While set, a swipe up from the view's bottom edge calls it, and that touch never reaches the app. */
    var onEdgeSwipe: (() -> Unit)? = null
    private var edge = Edge.NONE
    private var edgeStartY = 0f

    private enum class Edge { NONE, TRACKING, DONE }

    private val density: Float get() = view.resources.displayMetrics.densityDpi / 160f

    // The touch modes, and the one the current gesture started in.
    private val timer = object : Timer {
        override fun schedule(delayMs: Long, r: Runnable) {
            val guarded = Runnable { guard("window ${id.u}: touch timer") { r.run() } }
            timers[r] = guarded
            main.postDelayed(guarded, delayMs)
        }

        override fun cancel(r: Runnable) {
            timers.remove(r)?.let(main::removeCallbacks)
        }
    }
    private val timers = HashMap<Runnable, Runnable>()
    private val slop = ViewConfiguration.get(context).let {
        TouchSlop(it.scaledTouchSlop.toFloat(), ViewConfiguration.getLongPressTimeout().toLong(), TAP_MS)
    }
    private val touchPoints = TouchPoints(id, session::send, zoom)
    private val pointer = PointerGestures(id, session::send, zoom, { place() }, timer, slop)
    private val trackpad = TrackpadGestures(id, session::send, zoom, { place() }, timer, slop, ::windowBounds) { placeCursor() }
    private var gesture: ((MotionEvent) -> Boolean)? = null

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
            settingsChanged()
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
        main.post { guard("window ${id.u}: text input") { if (bound()) textInput(session.currentTextInput) } }
    }

    private fun touch(e: MotionEvent): Boolean {
        if (edgeSwipe(e)) return true
        // A mouse or touchpad reaches the app in every mode.
        if (!e.isFromSource(InputDevice.SOURCE_TOUCHSCREEN) || e.getToolType(0) == MotionEvent.TOOL_TYPE_MOUSE) return input.motion(e)
        // A gesture keeps the mode it started in.
        if (e.actionMasked == MotionEvent.ACTION_DOWN || gesture == null) gesture = pick()
        val g = gesture ?: return false
        val r = g(e)
        if (e.actionMasked == MotionEvent.ACTION_UP || e.actionMasked == MotionEvent.ACTION_CANCEL) gesture = null
        return r
    }

    private fun pick(): (MotionEvent) -> Boolean {
        val mode = session.inputMode(id)
        if (zooming && mode == InputMode.TOUCH) return gestures::onTouch
        val m: TouchMode = when (mode) {
            InputMode.TOUCH -> touchPoints
            InputMode.POINTER -> pointer
            InputMode.TRACKPAD -> trackpad
        }
        return { e -> m.on(Touches.of(e)).let { true } }
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

    /** A key event for the window; true when it went to the guest. */
    fun key(e: KeyEvent): Boolean = ime.key(e)

    /** The focused text input changed, whichever window it's in. */
    internal fun textInput(s: TextInputState) {
        ime.textInput(s)
        if (s.active && s.window == id) s.cursorRect?.let(::reveal)
    }

    /** Pans a clipped or zoomed window so the text cursor at [r] (window pixels) shows. */
    private fun reveal(r: PixelRect) {
        if (width <= 0 || height <= 0) return
        val margin = REVEAL_MARGIN_DP * view.resources.displayMetrics.density
        val left = zoom.x + r.x * zoom.scale
        val top = zoom.y + r.y * zoom.scale
        val right = zoom.x + (r.x + r.width) * zoom.scale
        val bottom = zoom.y + (r.y + r.height) * zoom.scale
        val dx = when {
            left < 0 -> -left + margin
            right > width -> width - right - margin
            else -> 0f
        }
        val dy = when {
            top < 0 -> -top + margin
            bottom > height -> height - bottom - margin
            else -> 0f
        }
        if ((dx != 0f || dy != 0f) && zoom.panBy(dx, dy)) place()
    }

    /** The toplevel or its settings changed: its size, minimum or preset. */
    fun refresh() {
        settingsChanged()
        relayout()
        main.removeCallbacks(push)
        pushState()
    }

    /** The input mode may have changed: Trackpad mode's cursor shows or goes. */
    private fun settingsChanged() {
        val trackpadMode = session.inputMode(id) == InputMode.TRACKPAD
        if (trackpadMode && width > 0) trackpad.place()
        cursorView.visibility = if (trackpadMode) View.VISIBLE else View.GONE
        placeCursor()
    }

    /** The client's cursor name, for the cursor Trackpad mode draws. */
    fun cursor(name: String) {
        cursorView.shape = name
    }

    /** Sends the window's state, with the scale its preset and the fit give it. */
    fun pushState() {
        // Before the view has a size, 0 × 0 would let the client pick one, which can be past what the
        // compositor accepts (GTK's natural size at the output's integer scale): wait for the first.
        if (!bound() || width <= 0 || height <= 0) return
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
        placeCursor()
        val l = layers ?: return
        SurfaceControl.Transaction().also { l.place(it, width, height, zoom) }.apply()
    }

    private fun placeCursor() {
        if (cursorView.visibility != View.VISIBLE || trackpad.x.isNaN()) return
        cursorView.moveTo(zoom.x + trackpad.x * zoom.scale, zoom.y + trackpad.y * zoom.scale)
    }

    /** The window's size in window pixels: the view's, or the client's geometry where that's larger. */
    private fun windowBounds(): Pair<Int, Int> {
        val (cw, ch) = session.contentSize(id)
        return max(width, cw) to max(height, ch)
    }

    /** The host stops showing the window. */
    fun release() {
        main.removeCallbacks(push)
        for (r in timers.values) main.removeCallbacks(r)
        timers.clear()
        input.release()
        ime.release()
    }

    /** The window's view: a text editor to Android, whose keyboard input goes through [ime]. */
    private inner class GuiView(context: Context) : SurfaceView(context) {
        override fun onCheckIsTextEditor(): Boolean = true

        override fun onCreateInputConnection(outAttrs: EditorInfo): InputConnection? =
            guard("window ${id.u}: input connection", null) { ime.connect(outAttrs) }
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
        // The longest two-finger tap that still right-clicks.
        const val TAP_MS = 300L
        // Room kept around a text cursor panned into view.
        const val REVEAL_MARGIN_DP = 16
    }
}
