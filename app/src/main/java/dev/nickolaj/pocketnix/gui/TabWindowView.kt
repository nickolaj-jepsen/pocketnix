package dev.nickolaj.pocketnix.gui

import android.content.Context
import android.graphics.Color
import android.os.Handler
import android.os.Looper
import android.view.PointerIcon
import android.view.SurfaceHolder
import android.view.SurfaceView
import android.view.View
import android.view.ViewTreeObserver
import android.widget.FrameLayout

/**
 * A guest window in a terminal tab, shown in place of the TerminalView
 * while its tab is selected: what [GuiWindowActivity] does for a window
 * with its own task. Made when the tab shows and [release]d when it stops
 * showing; the window itself stays in the session.
 */
internal class TabWindowView(context: Context, private val session: GuiSession, override val windowId: Int) : WindowHost {
    private val surface = SurfaceView(context)
    val view: FrameLayout = FrameLayout(context)
    private val input = WindowInput(windowId, session::send) { context.resources.displayMetrics.densityDpi / 160f }
    private val main = Handler(Looper.getMainLooper())
    private val pushState = Runnable { pushState() }
    private var width = 0
    private var height = 0
    private var attached = false

    private val callback = object : SurfaceHolder.Callback {
        override fun surfaceCreated(holder: SurfaceHolder) {}

        override fun surfaceChanged(holder: SurfaceHolder, format: Int, w: Int, h: Int) = guard("tab window ${windowId.u}: surface") {
            val resized = w != width || h != height
            width = w
            height = h
            attached = true
            if (bound) session.attach(this@TabWindowView, surface.surfaceControl, w, h)
            main.removeCallbacks(pushState)
            // The keyboard coming up resizes the tab in steps.
            if (resized) main.postDelayed(pushState, RESIZE_DEBOUNCE_MS) else pushState()
        }

        override fun surfaceDestroyed(holder: SurfaceHolder) = guard("tab window ${windowId.u}: surface gone") {
            attached = false
            if (bound) session.detach(this@TabWindowView)
            main.removeCallbacks(pushState)
            pushState()
        }
    }

    private val focusListener = ViewTreeObserver.OnWindowFocusChangeListener { focused ->
        guard("tab window ${windowId.u}: focus") {
            pushState()
            if (focused && bound) input.focused()
        }
    }

    private var bound = session.bindTab(this, windowId)

    init {
        surface.apply {
            holder.addCallback(callback)
            isFocusable = true
            isFocusableInTouchMode = true
            defaultFocusHighlightEnabled = false
            setOnTouchListener { v, e ->
                // The listener consumes the touch, so the view never takes focus itself.
                if (!v.isFocused) v.requestFocus()
                bound && input.motion(e)
            }
            setOnGenericMotionListener { _, e -> bound && input.motion(e) }
            setOnKeyListener { _, _, e -> bound && input.key(e) }
        }
        view.setBackgroundColor(Color.BLACK)
        view.addView(surface)
        view.addOnAttachStateChangeListener(object : View.OnAttachStateChangeListener {
            override fun onViewAttachedToWindow(v: View) {
                v.viewTreeObserver.addOnWindowFocusChangeListener(focusListener)
                // Keys go to the focused view: take it from the terminal once Compose has placed this.
                surface.post { surface.requestFocus() }
            }

            override fun onViewDetachedFromWindow(v: View) {
                v.viewTreeObserver.removeOnWindowFocusChangeListener(focusListener)
            }
        })
    }

    private fun pushState() {
        if (!bound) return
        session.windowState(
            this,
            WindowState(
                id = windowId,
                width = width,
                height = height,
                scale = view.resources.displayMetrics.densityDpi / 160f,
                focused = attached && view.hasWindowFocus(),
                visible = attached,
                // Tiled into the terminal screen: no shadows or rounded corners to draw.
                maximized = true,
            ),
        )
    }

    /** The tab stopped showing. */
    fun release() = guard("tab window ${windowId.u}: release") {
        main.removeCallbacks(pushState)
        input.release()
        if (bound) session.unbind(this, userClosed = false)
        bound = false
    }

    // The tab bar drops the tab, and with it this view.
    override fun dismiss() {
        bound = false
    }

    override fun toplevelChanged() {}

    override fun cursor(name: String) {
        if (bound) surface.pointerIcon = PointerIcon.getSystemIcon(view.context, GuiWindowActivity.pointerType(name))
    }

    // A tab has nothing to minimize to.
    override fun minimize() {}

    private companion object {
        const val RESIZE_DEBOUNCE_MS = 150L
    }
}
