package dev.nickolaj.pocketnix.gui

import android.app.Activity
import android.app.ActivityManager
import android.content.Context
import android.content.Intent
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.graphics.Color
import android.net.Uri
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.text.TextUtils
import android.view.Gravity
import android.view.KeyEvent
import android.view.PointerIcon
import android.view.SurfaceHolder
import android.view.SurfaceView
import android.view.View
import android.view.WindowInsets
import android.widget.LinearLayout
import android.widget.PopupMenu
import android.widget.TextView
import android.window.OnBackInvokedDispatcher
import android.util.Log
import dev.nickolaj.pocketnix.R
import dev.nickolaj.pocketnix.engine.VmState
import dev.nickolaj.pocketnix.ui.missingRequirements
import dev.nickolaj.pocketnix.vm
import kotlinx.coroutines.CoroutineExceptionHandler
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.launch
import java.util.UUID

/**
 * One guest toplevel as an Android window: its own document task, so it has
 * its own Recents card and, docked, its own freeform window. The session
 * owns the window; this only shows it while Android keeps the activity.
 *
 * Opened for an app-list entry instead (a launcher tap, a shortcut), it
 * first shows a splash: it boots the VM if needed, launches the app and
 * becomes the window the launch opens.
 */
class GuiWindowActivity : Activity(), WindowHost {
    override var windowId = 0
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

    // A launch's splash, until it's bound to the window.
    private val scope = CoroutineScope(
        SupervisorJob() + Dispatchers.Main.immediate + CoroutineExceptionHandler { _, e -> Log.e(TAG, "splash failed", e) },
    )
    private var target: AppTarget? = null
    private var launch: Launch? = null
    private var watch: Job? = null
    private var splash: SplashView? = null
    private var stale: Runnable? = null
    private var barTitle: TextView? = null

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        onBackInvokedDispatcher.registerOnBackInvokedCallback(OnBackInvokedDispatcher.PRIORITY_DEFAULT) { moveTaskToBack(true) }
        target = intent.data?.let(::parseApp)
        if (target != null) {
            guard("splash: create") { createLaunch(savedInstanceState) }
            return
        }
        val window = intent.data?.let(::parse)
        // A card from an earlier session, or a window that closed while its notification waited.
        if (window == null || !vm.gui.bind(this, window.first, window.second)) {
            finishAndRemoveTask()
            return
        }
        showWindow(window.second)
    }

    /** Shows bound window [id] in place of anything this showed before. */
    private fun showWindow(id: Int) {
        windowId = id
        bound = true
        splash = null
        watch?.cancel()
        launch?.let {
            vm.gui.disown(it.id, this)
            vm.gui.launches.forget(it.id)
        }
        launch = null
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
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setBackgroundColor(Color.BLACK)
            addView(view, LinearLayout.LayoutParams(LinearLayout.LayoutParams.MATCH_PARENT, 0, 1f))
            addView(windowBar())
            // Edge to edge is enforced: keep the guest's window out from under the bars, cutout and caption.
            setOnApplyWindowInsetsListener { v, insets ->
                val i = insets.getInsets(WindowInsets.Type.systemBars() or WindowInsets.Type.displayCutout())
                v.setPadding(i.left, i.top, i.right, i.bottom)
                WindowInsets.CONSUMED
            }
        }
        setContentView(root)
        toplevelChanged()
        // The activity started before its window existed.
        if (started) pushState()
        if (focused) input.focused()
    }

    /** The slim bar under the window: its title and its menu. */
    private fun windowBar(): View {
        val pad = (8 * resources.displayMetrics.density).toInt()
        val title = TextView(this).apply {
            setTextColor(BAR_TEXT)
            textSize = 13f
            maxLines = 1
            ellipsize = TextUtils.TruncateAt.END
            setPadding(pad * 2, 0, pad, 0)
        }
        barTitle = title
        val menu = TextView(this).apply {
            text = "⋮"
            setTextColor(BAR_TEXT)
            textSize = 18f
            gravity = Gravity.CENTER
            setPadding(pad * 2, 0, pad * 2, 0)
            contentDescription = getString(R.string.gui_window_menu)
            setOnClickListener { v -> guard("window menu") { showMenu(v) } }
        }
        return LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
            setBackgroundColor(BAR_BACKGROUND)
            addView(title, LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, 1f))
            addView(menu, LinearLayout.LayoutParams(LinearLayout.LayoutParams.WRAP_CONTENT, LinearLayout.LayoutParams.MATCH_PARENT))
            layoutParams = LinearLayout.LayoutParams(LinearLayout.LayoutParams.MATCH_PARENT, (BAR_DP * resources.displayMetrics.density).toInt())
        }
    }

    private fun showMenu(anchor: View) {
        val m = PopupMenu(this, anchor)
        val targets = vm.tabs.windowTargets()
        if (targets.isEmpty()) {
            // Only a running shell tab has an id the guest knows.
            m.menu.add(R.string.gui_move_to_tab).isEnabled = false
        } else {
            val sub = m.menu.addSubMenu(R.string.gui_move_to_tab)
            for (t in targets) {
                val tab = t.agentId
                sub.add(t.title).setOnMenuItemClickListener { guard("window ${windowId.u}: move") { vm.gui.move(windowId, tab) }; true }
            }
        }
        m.menu.add(R.string.gui_close).setOnMenuItemClickListener { guard("window ${windowId.u}: close") { vm.gui.requestClose(windowId) }; true }
        m.show()
    }

    // --- A launch's splash ---

    private fun createLaunch(saved: Bundle?) {
        val savedWindow = saved?.getInt(KEY_WINDOW) ?: 0
        val savedLaunch = saved?.getString(KEY_LAUNCH)?.let { vm.gui.launches[it] }
        showSplash()
        when {
            savedWindow != 0 -> if (vm.gui.bindLaunched(this, savedWindow)) showWindow(savedWindow) else finishAndRemoveTask()
            savedLaunch != null -> follow(savedLaunch)
            // Recents restoring a card whose VM died with the app; a tap on its pin follows as a new intent.
            saved != null || intent.flags and Intent.FLAG_ACTIVITY_LAUNCHED_FROM_HISTORY != 0 -> {
                Log.i(TAG, "a stale launch card for ${target?.app}; removing it")
                stale = Runnable { finishAndRemoveTask() }.also { handler.post(it) }
            }
            else -> startLaunch()
        }
    }

    private fun showSplash() {
        val t = target ?: return
        val app = vm.gui.catalog.app(t.app)
        val label = app?.label ?: t.app
        val icon = app?.let { iconBitmap(it) }
        val v = SplashView(this, onCancel = ::cancelLaunch, onRetry = ::startLaunch, onClose = ::finishAndRemoveTask)
        v.app(label, icon)
        splash = v
        setContentView(v)
        title = label
        @Suppress("DEPRECATION")
        setTaskDescription(ActivityManager.TaskDescription(label, icon))
    }

    /** Boots the VM if it's off, and launches the app; again after a failure. */
    private fun startLaunch() {
        val t = target ?: return
        launch?.let {
            vm.gui.launches.cancel(it.id)
            vm.gui.disown(it.id, this)
            vm.gui.launches.forget(it.id)
        }
        val s = vm.state.value
        if (s == VmState.Stopped || s is VmState.Failed) {
            val missing = missingRequirements(this, vm)
            if (missing.isNotEmpty()) {
                splash?.show(SplashStatus(getString(dev.nickolaj.pocketnix.R.string.gui_vm_cant_start, missing.joinToString { it.name }), failed = true))
                return
            }
            // Through the same foreground service the tile uses.
            vm.start()
        }
        follow(vm.gui.launch(t.app, t.action))
    }

    private fun follow(l: Launch) {
        launch = l
        vm.gui.own(l.id, this)
        val name = vm.gui.catalog.app(l.app)?.label ?: l.app
        watch?.cancel()
        watch = scope.launch {
            val vmFlows = combine(vm.state, vm.control.link, vm.agentLate, vm.config, vm.gui.ready) { s, link, late, cfg, ready ->
                { ls: LaunchState -> splashStatus(name, s, link, late, cfg?.gui?.enable, ready, ls) }
            }
            combine(vmFlows, l.state) { status, ls -> status(ls) to ls }.collect { (status, ls) ->
                when (ls) {
                    is LaunchState.Opened -> bindTo(ls.window)
                    is LaunchState.Activated -> bindTo(ls.window)
                    else -> splash?.show(status)
                }
            }
        }
    }

    /** The launch ended with [window]: show it here, or bring forward the task already showing it. */
    private fun bindTo(window: Int) {
        if (bound) return
        if (vm.gui.bindLaunched(this, window)) {
            showWindow(window)
        } else {
            vm.gui.raise(window)
            finishAndRemoveTask()
        }
    }

    private fun cancelLaunch() {
        launch?.let { vm.gui.launches.cancel(it.id) }
        finishAndRemoveTask()
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        guard("splash: new intent") {
            if (bound || target == null) return@guard
            val pending = stale
            if (pending != null) {
                // A tap on the pin of a stale card: launch after all.
                handler.removeCallbacks(pending)
                stale = null
                startLaunch()
            } else if (launch?.state?.value is LaunchState.Failed) {
                startLaunch()
            }
        }
    }

    override fun onSaveInstanceState(outState: Bundle) {
        super.onSaveInstanceState(outState)
        if (bound && target != null) outState.putInt(KEY_WINDOW, windowId)
        launch?.let { outState.putString(KEY_LAUNCH, it.id) }
    }

    private fun iconBitmap(app: AppEntry): Bitmap? =
        vm.gui.catalog.icon(app.icon, vm.gui.iconSize(large = true))?.let { BitmapFactory.decodeFile(it.path) }

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
        // Kept on a splash too: the window it becomes has the focus it had.
        focused = hasFocus
        if (!bound) return
        pushState()
        if (hasFocus) input.focused()
    }

    override fun onMultiWindowModeChanged(isInMultiWindowMode: Boolean, newConfig: android.content.res.Configuration) {
        super.onMultiWindowModeChanged(isInMultiWindowMode, newConfig)
        if (bound) pushState()
    }

    override fun dispatchKeyEvent(event: KeyEvent): Boolean = (bound && input.key(event)) || super.dispatchKeyEvent(event)

    override fun onDestroy() {
        scope.cancel()
        launch?.let {
            // Closed while waiting: the user doesn't want it any more.
            if (isFinishing) {
                vm.gui.launches.cancel(it.id)
                vm.gui.launches.forget(it.id)
            }
            vm.gui.disown(it.id, this)
        }
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

    /** The guest unmapped the window, or it moved to a tab: the task goes without the client hearing of it. */
    override fun dismiss() {
        closedByGuest = true
        finishAndRemoveTask()
    }

    override fun minimize() {
        moveTaskToBack(true)
    }

    override fun toplevelChanged() {
        if (!bound) return
        val t = vm.gui.title(windowId)
        title = t
        barTitle?.text = t
        val icon = vm.gui.appOf(windowId)?.let(::iconBitmap)
        @Suppress("DEPRECATION") // The Builder takes an icon only as a resource.
        setTaskDescription(ActivityManager.TaskDescription(t, icon))
    }

    override fun cursor(name: String) {
        if (bound) view.pointerIcon = PointerIcon.getSystemIcon(this, pointerType(name))
    }

    /** An app-list entry to launch, and the action ("" for none). */
    internal data class AppTarget(val app: String, val action: String)

    companion object {
        internal const val SCHEME = "pocketnix-gui"
        private const val RESIZE_DEBOUNCE_MS = 150L
        private const val TAG = "GuiWindowActivity"
        private const val KEY_WINDOW = "window"
        private const val KEY_LAUNCH = "launch"
        private const val BAR_DP = 36
        private const val BAR_BACKGROUND = 0xff1e1e1e.toInt()
        private const val BAR_TEXT = 0xffdddddd.toInt()

        /**
         * Launches entry [app] ([action] "" for none) on a splash that becomes
         * its window. The same entry and action share a task, so a second tap
         * brings the first forward the way an Android app's icon does;
         * [newWindow] always starts another.
         */
        fun appIntent(context: Context, app: String, action: String = "", newWindow: Boolean = false): Intent {
            val uri = Uri.Builder().scheme(SCHEME).authority("app").appendPath(app)
                .apply { if (action.isNotEmpty()) appendQueryParameter("action", action) }
                .apply { if (newWindow) appendQueryParameter("new", UUID.randomUUID().toString()) }
                .build()
            return Intent(Intent.ACTION_VIEW, uri, context, GuiWindowActivity::class.java)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_NEW_DOCUMENT)
        }

        /** An app URI's entry and action. */
        internal fun parseApp(uri: Uri): AppTarget? {
            if (uri.scheme != SCHEME || uri.host != "app") return null
            val app = uri.pathSegments.singleOrNull()?.takeIf(::validAppId) ?: return null
            return AppTarget(app, uri.getQueryParameter("action").orEmpty())
        }

        /** Whether a task's base intent is a launch's, which shows its window under the app's URI. */
        internal fun isLaunchTask(t: ActivityManager.AppTask): Boolean =
            runCatching { t.taskInfo.baseIntent.data?.let(::parseApp) != null }.getOrDefault(false)

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

        internal fun pointerType(name: String): Int = when (name) {
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
