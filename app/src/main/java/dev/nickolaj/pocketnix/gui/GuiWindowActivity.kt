package dev.nickolaj.pocketnix.gui

import android.annotation.SuppressLint
import android.app.ActivityManager
import android.content.Context
import android.content.Intent
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.net.Uri
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.util.Log
import android.view.KeyEvent
import android.view.PointerIcon
import android.view.WindowInsetsController
import android.view.WindowManager
import androidx.activity.ComponentActivity
import androidx.activity.addCallback
import androidx.activity.compose.setContent
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.displayCutout
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.ime
import androidx.compose.foundation.layout.systemBars
import androidx.compose.foundation.layout.union
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.SideEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.layout.onSizeChanged
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import dev.nickolaj.pocketnix.engine.VmState
import dev.nickolaj.pocketnix.ui.ExtraKeysBar
import dev.nickolaj.pocketnix.ui.PocketnixTheme
import dev.nickolaj.pocketnix.ui.extraKeyRows
import dev.nickolaj.pocketnix.ui.missingRequirements
import dev.nickolaj.pocketnix.vm
import java.util.UUID
import kotlin.math.max
import kotlinx.coroutines.CoroutineExceptionHandler
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.launch

/**
 * One guest toplevel as an Android window: its own document task, so it has
 * its own Recents card and, docked, its own freeform window. The session
 * owns the window; this only shows it while Android keeps the activity.
 *
 * Opened for an app-list entry instead (a launcher tap, a shortcut), it
 * first shows a splash: it boots the VM if needed, launches the app and
 * becomes the window the launch opens.
 */
class GuiWindowActivity :
    ComponentActivity(),
    WindowHost {
    override var windowId = 0
        private set
    private var bound = false
    private var closedByGuest = false
    override var surface: WindowSurface? = null
        private set
    private val handler = Handler(Looper.getMainLooper())
    private var focused = false
    private var started = false

    private var barTitle by mutableStateOf("")
    private var fullscreen by mutableStateOf(false)

    // A full-screen window's bar, shown over it for a while after a swipe up from the bottom edge.
    private var barRevealed by mutableStateOf(false)
    private var menuOpen = false
    private val hideBar = Runnable { if (!menuOpen) barRevealed = false }

    /** The bar's keyboard button does this; without it the bar has none. */
    internal var keyboardAction by mutableStateOf<(() -> Unit)?>(null)

    // Redraws the extra keys row when its modifiers change.
    private var modifierRevision by mutableIntStateOf(0)

    // A launch's splash, until it's bound to the window.
    private val scope = CoroutineScope(
        SupervisorJob() + Dispatchers.Main.immediate + CoroutineExceptionHandler { _, e -> Log.e(TAG, "splash failed", e) }
    )
    private var target: AppTarget? = null
    private var launch: Launch? = null
    private var watch: Job? = null
    private var splash: SplashView? = null
    private var stale: Runnable? = null

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        onBackPressedDispatcher.addCallback(this) { moveTaskToBack(true) }
        // The window's view is a text editor to Android, which would otherwise show the keyboard on opening it.
        window.setSoftInputMode(WindowManager.LayoutParams.SOFT_INPUT_STATE_HIDDEN or WindowManager.LayoutParams.SOFT_INPUT_ADJUST_RESIZE)
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
        val s = WindowSurface(this, vm.gui, this, { bound }) { attached ->
            WindowSurface.Flags(
                focused = focused && attached,
                visible = attached && started,
                maximized = !isInMultiWindowMode,
                fullscreen = fullscreen
            )
        }
        surface = s
        keyboardAction = s.ime::toggleKeyboard
        s.ime.onModifiers = { modifierRevision++ }
        setContent {
            PocketnixTheme { WindowContent(s) }
        }
        toplevelChanged()
        // The activity started before its window existed.
        if (started) s.pushState()
        if (focused) s.input.focused()
        if (vm.gui.fullscreen(id)) fullscreen(true)
    }

    /**
     * The window, and the bar under it; a full-screen window's bar shows over
     * it only while revealed. While the phone keyboard is up, the extra keys
     * row sits above it in the bar's place.
     */
    @Composable
    private fun WindowContent(s: WindowSurface) {
        val density = LocalDensity.current
        val config by vm.config.collectAsState()
        val imeBottom = WindowInsets.ime.getBottom(density)
        val insets = WindowInsets.systemBars.union(WindowInsets.displayCutout)
        val barsBottom = insets.getBottom(density)
        val typing = imeBottom > 0
        var typingRowHeight by remember { mutableIntStateOf(0) }
        val typingRow: (@Composable () -> Unit)? = if (typing) {
            {
                modifierRevision.let {}
                ExtraKeysBar(
                    rows = extraKeyRows(config),
                    modifiers = s.ime.modifiers,
                    revision = modifierRevision,
                    foreground = BAR_TEXT,
                    onKey = s.ime::extraKey
                )
            }
        } else {
            null
        }
        // The fit measures the window as if the keyboard and the row above it weren't there.
        val barPx = if (fullscreen) 0 else with(density) { BAR_HEIGHT.dp.roundToPx() }
        val keyboard = if (typing) max(0, imeBottom - barsBottom) + max(0, typingRowHeight - barPx) else 0
        SideEffect { s.keyboardHeight = keyboard }
        LaunchedEffect(typing) { if (!typing) s.ime.keyboardHidden() }
        val bar: @Composable () -> Unit = {
            WindowBar(barTitle, keyboardAction, typingRow, menu = ::menu, onMenu = ::menuOpened)
        }
        // Edge to edge is enforced: keep the guest's window out from under the bars, cutout, caption and keyboard.
        Column(
            Modifier
                .fillMaxSize()
                .background(Color.Black)
                .windowInsetsPadding(insets.union(WindowInsets.ime))
        ) {
            Box(Modifier.weight(1f).fillMaxWidth()) {
                AndroidView(factory = { s.root }, modifier = Modifier.fillMaxSize())
                if (fullscreen && barRevealed && !typing) Box(Modifier.align(Alignment.BottomCenter)) { bar() }
            }
            if (!fullscreen || typing) {
                Box(Modifier.onSizeChanged { if (typing) typingRowHeight = it.height }) { bar() }
            }
        }
    }

    private fun menu(): List<MenuAction> {
        val id = windowId
        val targets = vm.tabs.windowTargets()
        val extra = buildList {
            if (fullscreen) add(MenuAction("Leave full screen") { fullscreen(false) })
            // Only a running shell tab has an id the guest knows.
            add(
                MenuAction(
                    MOVE_TO_TAB,
                    enabled = targets.isNotEmpty(),
                    children = targets.map { t ->
                        val tab = t.agentId
                        MenuAction(t.title) { vm.gui.move(id, tab) }
                    }
                )
            )
        }
        return windowMenu(vm.gui, id, extra)
    }

    private fun menuOpened(open: Boolean) {
        menuOpen = open
        handler.removeCallbacks(hideBar)
        if (!open) handler.postDelayed(hideBar, BAR_REVEAL_MS)
    }

    private fun revealBar() {
        barRevealed = true
        handler.removeCallbacks(hideBar)
        handler.postDelayed(hideBar, BAR_REVEAL_MS)
    }

    // --- A launch's splash ---

    private fun createLaunch(saved: Bundle?) {
        val savedWindow = saved?.getInt(KEY_WINDOW) ?: 0
        val savedLaunch = saved?.getString(KEY_LAUNCH)?.let { vm.gui.launches[it] }
        showSplash()
        when {
            savedWindow != 0 && vm.gui.bindLaunched(this, savedWindow) -> showWindow(savedWindow)

            savedLaunch != null -> follow(savedLaunch)

            // A card whose VM (and so its window or launch) died with the app: Recents restoring it, or a tap
            // on its pin reusing it, which follows as a new intent.
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
                splash?.show(SplashStatus("The VM can't start yet: ${missing.joinToString { it.name }}. pocketnix shows what's missing.", failed = true))
                return
            }
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

    private fun iconBitmap(app: AppEntry): Bitmap? = vm.gui.catalog.icon(app.icon, vm.gui.iconSize(large = true))?.let { BitmapFactory.decodeFile(it.path) }

    override fun onStart() {
        super.onStart()
        started = true
        surface?.pushState()
    }

    override fun onStop() {
        started = false
        surface?.pushState()
        super.onStop()
    }

    override fun onWindowFocusChanged(hasFocus: Boolean) {
        super.onWindowFocusChanged(hasFocus)
        // Kept on a splash too: the window it becomes has the focus it had.
        focused = hasFocus
        val s = surface ?: return
        s.ime.windowFocus(hasFocus)
        s.pushState()
        if (hasFocus) s.input.focused()
    }

    override fun onMultiWindowModeChanged(isInMultiWindowMode: Boolean, newConfig: android.content.res.Configuration) {
        super.onMultiWindowModeChanged(isInMultiWindowMode, newConfig)
        surface?.pushState()
    }

    @SuppressLint("RestrictedApi") // Activity's dispatchKeyEvent is public; lint sees ComponentActivity's @RestrictTo override
    override fun dispatchKeyEvent(event: KeyEvent): Boolean = guard("window ${windowId.u}: key", false) { bound && surface?.key(event) == true } || super.dispatchKeyEvent(event)

    override fun onDestroy() {
        scope.cancel()
        handler.removeCallbacks(hideBar)
        launch?.let {
            // Closed while waiting: the user doesn't want it any more.
            if (isFinishing) {
                vm.gui.launches.cancel(it.id)
                vm.gui.launches.forget(it.id)
            }
            vm.gui.disown(it.id, this)
        }
        if (bound) {
            surface?.release()
            // Finishing without the guest asking: the user closed it (caption X, a Recents swipe).
            vm.gui.unbind(this, userClosed = isFinishing && !closedByGuest && !isChangingConfigurations)
        }
        super.onDestroy()
    }

    /** The guest unmapped the window, or it moved to a tab: the task goes without the client hearing of it. */
    override fun dismiss() {
        closedByGuest = true
        finishAndRemoveTask()
    }

    override fun minimize() {
        moveTaskToBack(true)
    }

    /** Full screen hides the system bars and the window's own, until a swipe up from the bottom edge. */
    override fun fullscreen(on: Boolean) {
        if (!bound || fullscreen == on) return
        fullscreen = on
        vm.gui.fullscreenChanged(windowId, on)
        barRevealed = false
        window.insetsController?.let {
            val bars = android.view.WindowInsets.Type.systemBars()
            if (on) {
                it.systemBarsBehavior = WindowInsetsController.BEHAVIOR_SHOW_TRANSIENT_BARS_BY_SWIPE
                it.hide(bars)
            } else {
                it.show(bars)
            }
        }
        surface?.onEdgeSwipe = if (on) ::revealBar else null
        surface?.pushState()
    }

    override fun toplevelChanged() {
        if (!bound) return
        val t = vm.gui.title(windowId)
        title = t
        barTitle = t
        val icon = vm.gui.appOf(windowId)?.let(::iconBitmap)
        @Suppress("DEPRECATION") // The Builder takes an icon only as a resource.
        setTaskDescription(ActivityManager.TaskDescription(t, icon))
        surface?.refresh()
    }

    override fun settingsChanged() {
        surface?.refresh()
    }

    override fun cursor(name: String) {
        surface?.view?.pointerIcon = PointerIcon.getSystemIcon(this, pointerType(name))
        surface?.cursor(name)
    }

    /** An app-list entry to launch, and the action ("" for none). */
    internal data class AppTarget(val app: String, val action: String)

    companion object {
        internal const val SCHEME = "pocketnix-gui"
        private const val TAG = "GuiWindowActivity"
        private const val KEY_WINDOW = "window"
        private const val KEY_LAUNCH = "launch"
        private const val BAR_REVEAL_MS = 4_000L

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
        internal fun isLaunchTask(t: ActivityManager.AppTask): Boolean = runCatching { t.taskInfo.baseIntent.data?.let(::parseApp) != null }.getOrDefault(false)

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
