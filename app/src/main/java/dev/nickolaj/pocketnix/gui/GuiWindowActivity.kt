package dev.nickolaj.pocketnix.gui

import android.app.Activity
import android.app.ActivityManager
import android.content.Context
import android.content.Intent
import android.graphics.Color
import android.net.Uri
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.view.KeyEvent
import android.view.PointerIcon
import android.view.SurfaceHolder
import android.view.SurfaceView
import android.view.WindowInsets
import android.widget.FrameLayout
import android.window.OnBackInvokedDispatcher
import dev.nickolaj.pocketnix.vm

/**
 * One guest toplevel as an Android window: its own document task, so it has
 * its own Recents card and, docked, its own freeform window. The session
 * owns the window; this only shows it while Android keeps the activity.
 */
class GuiWindowActivity : Activity() {
    var windowId = 0
        private set
    private var bound = false
    private var closedByGuest = false
    private lateinit var view: SurfaceView
    private lateinit var input: WindowInput
    private val handler = Handler(Looper.getMainLooper())
    private var width = 0
    private var height = 0
    private var attached = false
    private var focused = false
    private var started = false
    private val pushState = Runnable { pushState() }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val target = intent.data?.let(::parse)
        // A card from an earlier session, or a window that closed while its notification waited.
        if (target == null || !vm.gui.bind(this, target.first, target.second)) {
            finishAndRemoveTask()
            return
        }
        windowId = target.second
        bound = true
        input = WindowInput(windowId, vm.gui::send) { resources.displayMetrics.densityDpi / 160f }
        view = SurfaceView(this).apply {
            holder.addCallback(surfaceCallback)
            isFocusable = true
            isFocusableInTouchMode = true
            // A hardware key leaves touch mode, and the focus highlight would tint the whole window.
            defaultFocusHighlightEnabled = false
            setOnTouchListener { _, e -> input.motion(e) }
            setOnGenericMotionListener { _, e -> input.motion(e) }
        }
        val root = FrameLayout(this).apply {
            setBackgroundColor(Color.BLACK)
            addView(view)
            // Edge to edge is enforced: keep the guest's window out from under the bars, cutout and caption.
            setOnApplyWindowInsetsListener { v, insets ->
                val i = insets.getInsets(WindowInsets.Type.systemBars() or WindowInsets.Type.displayCutout())
                v.setPadding(i.left, i.top, i.right, i.bottom)
                WindowInsets.CONSUMED
            }
        }
        setContentView(root)
        onBackInvokedDispatcher.registerOnBackInvokedCallback(OnBackInvokedDispatcher.PRIORITY_DEFAULT) { moveTaskToBack(true) }
        toplevelChanged()
    }

    @Deprecated("Deprecated in Java")
    override fun onBackPressed() {
        moveTaskToBack(true)
    }

    private val surfaceCallback = object : SurfaceHolder.Callback {
        override fun surfaceCreated(holder: SurfaceHolder) {}

        override fun surfaceChanged(holder: SurfaceHolder, format: Int, w: Int, h: Int) {
            val resized = w != width || h != height
            width = w
            height = h
            attached = true
            vm.gui.attach(this@GuiWindowActivity, view.surfaceControl, w, h)
            // Docked moves and resizes come as two sizes in a row (the caption inset lands second).
            handler.removeCallbacks(pushState)
            if (resized) handler.postDelayed(pushState, RESIZE_DEBOUNCE_MS) else pushState()
        }

        override fun surfaceDestroyed(holder: SurfaceHolder) {
            attached = false
            vm.gui.detach(this@GuiWindowActivity)
            handler.removeCallbacks(pushState)
            pushState()
        }
    }

    override fun onStart() {
        super.onStart()
        started = true
        if (bound) pushState()
    }

    override fun onStop() {
        started = false
        if (bound) pushState()
        super.onStop()
    }

    override fun onWindowFocusChanged(hasFocus: Boolean) {
        super.onWindowFocusChanged(hasFocus)
        if (!bound) return
        focused = hasFocus
        pushState()
        if (hasFocus) input.focused()
    }

    override fun onMultiWindowModeChanged(isInMultiWindowMode: Boolean, newConfig: android.content.res.Configuration) {
        super.onMultiWindowModeChanged(isInMultiWindowMode, newConfig)
        if (bound) pushState()
    }

    override fun dispatchKeyEvent(event: KeyEvent): Boolean = (bound && input.key(event)) || super.dispatchKeyEvent(event)

    override fun onDestroy() {
        if (bound) {
            handler.removeCallbacks(pushState)
            input.release()
            // Finishing without the guest asking: the user closed it (caption X, a Recents swipe).
            vm.gui.unbind(this, userClosed = isFinishing && !closedByGuest && !isChangingConfigurations)
        }
        super.onDestroy()
    }

    private fun pushState() {
        if (!bound) return
        vm.gui.windowState(
            this,
            WindowState(
                id = windowId,
                // Hidden, the window keeps its last size: the client needn't redraw at another for nobody.
                width = width,
                height = height,
                scale = resources.displayMetrics.densityDpi / 160f,
                focused = focused && attached,
                visible = attached && started,
                maximized = !isInMultiWindowMode,
            ),
        )
    }

    /** The guest unmapped the window. */
    internal fun closeByGuest() {
        closedByGuest = true
        finishAndRemoveTask()
    }

    /** The toplevel's title or app id changed. */
    internal fun toplevelChanged() {
        val t = vm.gui.title(windowId)
        title = t
        setTaskDescription(ActivityManager.TaskDescription.Builder().setLabel(t).build())
    }

    internal fun cursor(name: String) {
        if (bound) view.pointerIcon = PointerIcon.getSystemIcon(this, pointerType(name))
    }

    companion object {
        internal const val SCHEME = "pocketnix-gui"
        private const val RESIZE_DEBOUNCE_MS = 150L

        /** Opens (or brings back) the task of toplevel [id]: one document per window, deduplicated by its URI. */
        fun intent(context: Context, instance: String, id: Int): Intent = Intent(context, GuiWindowActivity::class.java)
            .setData(windowUri(instance, id))
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_NEW_DOCUMENT)

        /** A window URI's compositor instance and toplevel id. */
        fun parse(uri: Uri): Pair<String, Int>? {
            if (uri.scheme != SCHEME || uri.host != "window") return null
            val seg = uri.pathSegments
            if (seg.size != 2) return null
            val id = seg[1].toLongOrNull()?.takeIf { it in 1..0xffffffffL } ?: return null
            return seg[0] to id.toInt()
        }

        /** Removes the window tasks [which] picks. */
        fun removeTasks(context: Context, which: (ActivityManager.AppTask) -> Boolean) {
            val am = context.getSystemService(ActivityManager::class.java)
            for (t in am.appTasks) runCatching { if (t.windowId != 0 && which(t)) t.finishAndRemoveTask() }
        }

        private fun pointerType(name: String): Int = when (name) {
            "hidden" -> PointerIcon.TYPE_NULL
            "text" -> PointerIcon.TYPE_TEXT
            "vertical-text" -> PointerIcon.TYPE_VERTICAL_TEXT
            "pointer" -> PointerIcon.TYPE_HAND
            "grab" -> PointerIcon.TYPE_GRAB
            "grabbing" -> PointerIcon.TYPE_GRABBING
            "crosshair" -> PointerIcon.TYPE_CROSSHAIR
            "wait", "progress" -> PointerIcon.TYPE_WAIT
            "help" -> PointerIcon.TYPE_HELP
            "context-menu" -> PointerIcon.TYPE_CONTEXT_MENU
            "cell" -> PointerIcon.TYPE_CELL
            "alias" -> PointerIcon.TYPE_ALIAS
            "copy" -> PointerIcon.TYPE_COPY
            "no-drop", "not-allowed" -> PointerIcon.TYPE_NO_DROP
            "move", "all-scroll" -> PointerIcon.TYPE_ALL_SCROLL
            "ew-resize", "e-resize", "w-resize", "col-resize" -> PointerIcon.TYPE_HORIZONTAL_DOUBLE_ARROW
            "ns-resize", "n-resize", "s-resize", "row-resize" -> PointerIcon.TYPE_VERTICAL_DOUBLE_ARROW
            "nesw-resize", "ne-resize", "sw-resize" -> PointerIcon.TYPE_TOP_RIGHT_DIAGONAL_DOUBLE_ARROW
            "nwse-resize", "nw-resize", "se-resize" -> PointerIcon.TYPE_TOP_LEFT_DIAGONAL_DOUBLE_ARROW
            "zoom-in" -> PointerIcon.TYPE_ZOOM_IN
            "zoom-out" -> PointerIcon.TYPE_ZOOM_OUT
            else -> PointerIcon.TYPE_ARROW
        }
    }
}
