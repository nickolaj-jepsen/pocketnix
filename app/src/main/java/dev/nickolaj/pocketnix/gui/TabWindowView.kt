package dev.nickolaj.pocketnix.gui

import android.content.Context
import android.graphics.Color
import android.view.PointerIcon
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
    val view: FrameLayout = FrameLayout(context)
    private var bound = false

    override val surface: WindowSurface = WindowSurface(context, session, this, { bound }) { attached ->
        WindowSurface.Flags(
            focused = attached && view.hasWindowFocus(),
            visible = attached,
            // Tiled into the terminal screen: no shadows or rounded corners to draw.
            maximized = true,
        )
    }

    private val focusListener = ViewTreeObserver.OnWindowFocusChangeListener { focused ->
        guard("tab window ${windowId.u}: focus") {
            surface.pushState()
            if (focused && bound) surface.input.focused()
        }
    }

    init {
        bound = session.bindTab(this, windowId)
        val s = surface.view
        s.setOnKeyListener { _, _, e -> bound && surface.input.key(e) }
        view.setBackgroundColor(Color.BLACK)
        view.addView(s)
        view.addOnAttachStateChangeListener(object : View.OnAttachStateChangeListener {
            override fun onViewAttachedToWindow(v: View) {
                v.viewTreeObserver.addOnWindowFocusChangeListener(focusListener)
                // Keys go to the focused view: take it from the terminal once Compose has placed this.
                s.post { s.requestFocus() }
            }

            override fun onViewDetachedFromWindow(v: View) {
                v.viewTreeObserver.removeOnWindowFocusChangeListener(focusListener)
            }
        })
    }

    /** The tab stopped showing. */
    fun release() = guard("tab window ${windowId.u}: release") {
        surface.release()
        if (bound) session.unbind(this, userClosed = false)
        bound = false
    }

    // The tab bar drops the tab, and with it this view.
    override fun dismiss() {
        bound = false
    }

    override fun toplevelChanged() = surface.refresh()

    override fun settingsChanged() = surface.refresh()

    override fun cursor(name: String) {
        if (bound) surface.view.pointerIcon = PointerIcon.getSystemIcon(view.context, GuiWindowActivity.pointerType(name))
    }

    // A tab has nothing to minimize to, and is no place for full screen.
    override fun minimize() {}

    override fun fullscreen(on: Boolean) {}
}
