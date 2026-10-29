package dev.nickolaj.pocketnix.gui

import android.Manifest
import android.app.ActivityManager
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.hardware.display.DisplayManager
import android.net.Uri
import android.os.Handler
import android.os.Looper
import android.util.Log
import android.view.Display
import android.view.SurfaceControl
import dev.nickolaj.pocketnix.MainActivity
import dev.nickolaj.pocketnix.R
import dev.nickolaj.pocketnix.agent.AgentDialer
import dev.nickolaj.pocketnix.agent.GuiConfig
import java.io.File
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicInteger
import kotlin.math.roundToInt
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * The guest's GUI windows: the one GUI stream while the guest has GUI on,
 * the windows and surfaces it describes, what shows each window (its own
 * task's [GuiWindowActivity], or a terminal tab's [TabWindowView]), the app
 * list and the launches. Owned by the app's Vm, so windows outlive their
 * activities and a reconnect.
 */
class GuiSession(private val app: Context, appVersion: String) : GuiLink.Listener {
    // Guards the state and the renderers; the link's thread, the render thread and the main thread take it.
    private val lock = Any()
    private val state = GuiState(GuiNative)
    private val link = GuiLink(appVersion, this)

    /** The guest's app list and icons, kept for when the VM is off. */
    val catalog = AppCatalog(File(app.filesDir, "gui"))
    val launches = Launches(link::send)
    val shortcuts = AppShortcuts(app, catalog) { iconSize(large = true) }

    /** Each app's size preset and input mode: the module's, under the user's changes. */
    val appSettings = AppSettings(PrefsOverrideStore(app))

    // The factors the compositor last got on this connection.
    @Volatile private var sentFactors: Pair<Float, Map<String, Float>>? = null

    /** The guest's keymap follows the physical keyboard's layout. */
    private val keyboardLayouts = KeyboardLayouts(app) { l -> if (link.connected) link.send(Gui.keyboardLayout(l.layout, l.variant)) }

    // The focused text input, and the `ime` serials of this connection.
    @Volatile private var textInput = TextInputState.NONE
    private val imeSerial = AtomicInteger()

    // The splash waiting on each launch, by launch id.
    private val owners = ConcurrentHashMap<String, GuiWindowActivity>()

    // Icons asked for on this connection, under [lock]: each is answered once.
    private val askedIcons = HashSet<Pair<String, Int>>()

    private val _tabWindows = MutableStateFlow<List<WindowTab>>(emptyList())

    /** The windows shown as terminal tabs, in the order they mapped. */
    val tabWindows: StateFlow<List<WindowTab>> = _tabWindows.asStateFlow()

    /** Selects a window's tab in the terminal screen; called on the main thread. */
    @Volatile var selectTab: (Int) -> Unit = {}

    // Windows the user moved, whose `toplevel` echo hasn't come yet: the move shows them where they went.
    private val moving = ConcurrentHashMap.newKeySet<Int>()

    private val _ready = MutableStateFlow(false)

    /** The GUI stream is connected and its replay done. */
    val ready: StateFlow<Boolean> = _ready.asStateFlow()
    private val main = Handler(Looper.getMainLooper())

    // Draws outside the link's thread: a window's first frame, and frames that waited for a buffer.
    private val renderThread = Executors.newSingleThreadExecutor { r -> Thread(r, "gui-render").apply { isDaemon = true } }

    // The link thread's commit work, against the display's frame interval as `output` last read it.
    private val hints = FrameHints(app)

    @Volatile private var frameNs = 0L

    private class View(val id: Int) {
        @Volatile var host: WindowHost? = null

        /** Under [lock]. */
        var renderer: WindowRenderer? = null

        /** The last `window` message, re-sent on every connect; written on the main thread only. */
        @Volatile var sent: WindowState? = null

        @Volatile var title = ""

        /** The user closed its activity and the client was asked to close; written on the main thread only. */
        var closeAsked = false

        /** A launch's splash is about to show it, so it gets no activity of its own. */
        @Volatile var claimed = false

        /** The task of its last activity, to bring it forward; -1 for none. */
        @Volatile var taskId = -1

        /** Full screen as the client last asked or the user last set it, for a host that binds later. Main thread only. */
        var fullscreen = false
    }

    // Concurrent so the main thread can read a window's fields without waiting for a commit to apply.
    private val views = ConcurrentHashMap<Int, View>()

    @Volatile private var started = false
    private var hashesOk = 0L
    private var hashesBad = 0L

    init {
        state.onHash = { surface, seq, expected, actual ->
            if (expected == actual) {
                hashesOk++
            } else {
                hashesBad++
                Log.e(TAG, "hash mismatch surface=${surface.u} seq=${seq.u}: compositor %016x, app %016x".format(expected, actual))
            }
        }
        // Rotation, display size and refresh rate changes all come as display changes.
        app.getSystemService(DisplayManager::class.java).registerDisplayListener(
            object : DisplayManager.DisplayListener {
                override fun onDisplayChanged(displayId: Int) = guard("display change") {
                    if (displayId == Display.DEFAULT_DISPLAY && link.connected) link.send(output())
                }

                override fun onDisplayAdded(displayId: Int) {}
                override fun onDisplayRemoved(displayId: Int) {}
            },
            main
        )
        Foreground.onVisible { launchPending() }
        appSettings.onChange = {
            sendSettings(force = false)
            onMain("app settings: windows") { for (v in views.values) v.host?.settingsChanged() }
        }
    }

    /** The module's GUI config; null (no config yet, or the VM is off) keeps the last one. */
    fun config(c: GuiConfig?) = appSettings.config(c)

    /** Sends `settings` when the factors differ from what this connection has, or [force]d at its start. */
    private fun sendSettings(force: Boolean) {
        val f = appSettings.factors()
        if (!force && f == sentFactors) return
        sentFactors = f
        link.send(Gui.settings(f.first, f.second))
    }

    /** Keeps the GUI stream connected through [dialer]; [hashes] has the compositor send content hashes to check. */
    fun start(dialer: AgentDialer, hashes: Boolean) {
        started = true
        link.start(dialer, hashes)
    }

    /** Drops the stream; with [closeWindows] (the guest stopped, or turned GUI off) every window goes too. */
    fun stop(closeWindows: Boolean) {
        if (!started && !closeWindows) return
        started = false
        link.stop()
        _ready.value = false
        launches.disconnected()
        synchronized(lock) { forgetAcks() }
        if (closeWindows) {
            val ids = synchronized(lock) { state.windows.keys.toList() }
            for (id in ids) closeWindow(id)
            synchronized(lock) {
                state.windows.clear()
                state.surfaces.clear()
                publishTabs()
            }
        }
    }

    // --- The link's thread ---

    override fun connected(hello: GuiMessage.Hello) {
        _ready.value = false
        launches.connected(hello.instance)
        val dropped = synchronized(lock) {
            askedIcons.clear()
            state.hello(hello).also {
                forgetAcks()
                publishTabs()
            }
        }
        for (w in dropped) closeWindow(w.id)
        imeSerial.set(0)
        textInput(TextInputState.NONE)
        link.send(output())
        sendSettings(force = true)
        keyboardLayouts.current.let { link.send(Gui.keyboardLayout(it.layout, it.variant)) }
        for (v in views.values) v.sent?.let { link.send(Gui.window(it)) }
    }

    override fun message(msg: GuiMessage) {
        when (msg) {
            is GuiMessage.Group -> {
                val start = System.nanoTime()
                // Decompressing is the slow part, and needs no state: keep it out of the lock.
                val g = GuiMessage.Group(msg.commits.map { it.unpacked(GuiNative) })
                synchronized(lock) {
                    state.apply(g)
                    render(g)
                }
                hints.report(System.nanoTime() - start, frameNs)
            }

            is GuiMessage.Toplevel -> {
                var oldTab: String? = null
                val new = synchronized(lock) {
                    if (!msg.isDialog) oldTab = state.windows[msg.id]?.toplevel?.tab
                    val new = state.toplevel(msg).also { if (it) views[msg.id] = View(msg.id) }
                    val ended = launches.toplevel(msg)
                    // Its splash binds it once it sees the launch end; see disown for a splash that's gone.
                    if (new && ended != null && owners.containsKey(ended.id)) views[msg.id]?.claimed = true
                    if (!msg.isDialog) publishTabs()
                    new
                }
                if (msg.isDialog) return
                views[msg.id]?.title = title(msg)
                val retabbed = new || oldTab != msg.tab
                // Only the echo that moves it ends a move; a title change may come first.
                val moved = retabbed && moving.remove(msg.id)
                when {
                    msg.tab.isNotEmpty() && retabbed -> intoTab(msg.id, new = new, moved = moved)
                    msg.tab.isEmpty() && !oldTab.isNullOrEmpty() -> outOfTab(msg.id)
                    !new -> onMain("window ${msg.id.u}: title") { views[msg.id]?.host?.toplevelChanged() }
                    views[msg.id]?.claimed != true -> openWindow(msg.id)
                }
            }

            is GuiMessage.ToplevelRemoved -> {
                val window = synchronized(lock) {
                    val window = msg.id in state.windows
                    val gone = state.toplevelRemoved(msg.id)
                    if (!window && gone.isNotEmpty()) {
                        // A dialog: its layers leave its window, which stays.
                        val t = SurfaceControl.Transaction()
                        for (s in gone) views[s.window]?.renderer?.remove(t, s.id)
                        t.apply()
                    }
                    if (window) publishTabs()
                    window
                }
                if (window) closeWindow(msg.id)
            }

            is GuiMessage.ToplevelRequest -> when (msg.request) {
                "minimize" -> onMain("window ${msg.id.u}: minimize") { views[msg.id]?.host?.minimize() }

                "fullscreen", "unfullscreen" -> onMain("window ${msg.id.u}: ${msg.request}") {
                    val v = views[msg.id] ?: return@onMain
                    v.fullscreen = msg.request == "fullscreen"
                    v.host?.fullscreen(v.fullscreen)
                }
            }

            is GuiMessage.Surface -> synchronized(lock) {
                val moved = state.surface(msg) ?: return@synchronized
                views[moved]?.renderer?.let { r -> SurfaceControl.Transaction().also { r.remove(it, msg.id) }.apply() }
            }

            is GuiMessage.SurfaceRemoved -> synchronized(lock) {
                val s = state.surfaceRemoved(msg.id) ?: return@synchronized
                views[s.window]?.renderer?.let { r -> SurfaceControl.Transaction().also { r.remove(it, s.id) }.apply() }
            }

            is GuiMessage.Cursor -> onMain("window ${msg.window.u}: cursor") { views[msg.window]?.host?.cursor(msg.name) }

            GuiMessage.Ready -> {
                val windows = synchronized(lock) {
                    val (windows, surfaces) = state.ready()
                    val t = SurfaceControl.Transaction()
                    for (s in surfaces) views[s.window]?.renderer?.remove(t, s.id)
                    // The replay sends no commit for a surface without a buffer: hide what it showed before.
                    for (v in views.values) {
                        val r = v.renderer ?: continue
                        guard("window ${v.id.u}: render") { r.render(t, state.surfaces.values.filter { it.window == v.id }, 0) }
                    }
                    t.apply()
                    publishTabs()
                    windows
                }
                for (w in windows) closeWindow(w.id)
                _ready.value = true
                launches.ready()
                launchPending()
            }

            is GuiMessage.Apps -> {
                val change = catalog.update(msg.apps)
                if (change.changed) Log.i(TAG, "app list: ${msg.apps.size} entries, ${change.added.size} new, ${change.gone.size} gone")
                shortcuts.update()
                requestIcons()
                // A toplevel's icon comes from its entry.
                onMain("app list: windows") { for (v in views.values) v.host?.toplevelChanged() }
            }

            is GuiMessage.Icon -> {
                catalog.storeIcon(msg.key, msg.size, msg.png)
                shortcuts.update()
            }

            is GuiMessage.LaunchStarted -> launches.started(msg.id)

            is GuiMessage.LaunchFailed -> {
                val l = launches.failed(msg) ?: return
                Log.i(TAG, "launch ${l.id} of ${l.app} failed: ${msg.reason} ${msg.message}")
                if (msg.reason == LaunchState.UNKNOWN) shortcuts.disable(l.app)
            }

            is GuiMessage.Activate -> {
                val l = launches.activate(msg)
                // A launch's splash handles its own activation.
                if (l == null || !owners.containsKey(l.id)) onMain("window ${msg.window.u}: activate") { raise(msg.window) }
            }

            is GuiMessage.TextInput -> textInput(msg.state)

            is GuiMessage.Error -> Log.w(TAG, "compositor error: ${msg.message}")

            is GuiMessage.Hello -> throw GuiProtocolException("a second hello")
        }
    }

    override fun disconnected(error: Throwable?) {
        Log.i(TAG, "disconnected${error?.let { ": ${it.message}" } ?: ""}")
        _ready.value = false
        // Sessions are per connection: the IME's edits would go nowhere.
        textInput(TextInputState.NONE)
        launches.disconnected()
        hints.close()
        // Null is a stop, which forgot them already, and may come after the next link's hello.
        if (error != null) synchronized(lock) { forgetAcks() }
    }

    /** The focused text input changed: every shown window hears of it, on the main thread. */
    private fun textInput(s: TextInputState) {
        if (s == textInput) return
        textInput = s
        onMain("text input") { for (v in views.values) v.host?.surface?.textInput(s) }
    }

    /**
     * Under [lock]: a commit seq means something on its own connection only,
     * so no ack, held back or committed but not yet sent, may reach the next.
     */
    private fun forgetAcks() {
        for (w in state.windows.values) w.lastSeq = 0
        for (v in views.values) v.renderer?.forgetAcks()
    }

    /** Under [lock]: one group into one transaction, acknowledged per window once shown. */
    private fun render(group: GuiMessage.Group) {
        var t: SurfaceControl.Transaction? = null
        for (window in group.commits.map { it.window }.distinct()) {
            val r = views[window]?.renderer ?: continue
            val tx = t ?: SurfaceControl.Transaction().also { t = it }
            val surfaces = group.commits.filter { it.window == window }.mapNotNull { state.surfaces[it.surface] }
            // A renderer failing is no reason to drop the connection, and so every window's content.
            guard("window ${window.u}: render") { r.render(tx, surfaces, state.windows[window]?.lastSeq ?: 0) }
        }
        t?.apply()
    }

    private fun presented(window: Int, seq: Int) = link.send(Gui.presented(window, seq))

    /**
     * Draws every surface of the window [r] shows, on the render thread:
     * its [first] frame, acknowledging the window's commits so far, or else
     * what it deferred for want of a buffer.
     */
    private fun redraw(r: WindowRenderer, first: Boolean) = renderThread.execute {
        guard("redraw") {
            synchronized(lock) {
                val v = views.values.firstOrNull { it.renderer === r } ?: return@guard
                if (!first && !r.behind) return@guard
                val t = SurfaceControl.Transaction()
                r.render(t, state.surfaces.values.filter { it.window == v.id }, if (first) state.windows[v.id]?.lastSeq ?: 0 else 0)
                t.apply()
            }
        }
    }

    // --- Windows ---

    /**
     * Opens the toplevel's Android window, unless it shows in a tab:
     * directly while a pocketnix window is visible, else through a
     * notification.
     */
    private fun openWindow(id: Int) = onMain("window ${id.u}: open") {
        val instance = synchronized(lock) {
            val w = state.windows[id] ?: return@onMain
            if (w.toplevel.tab.isNotEmpty() || views[id]?.let { it.host != null || it.claimed } != false) return@onMain
            state.instance
        } ?: return@onMain
        val intent = GuiWindowActivity.intent(app, instance, id)
        if (Foreground.visible) {
            Log.i(TAG, "window ${id.u}: opening")
            app.startActivity(intent)
        } else {
            Log.i(TAG, "window ${id.u}: no pocketnix window is visible; posting a notification")
            notifyWindow(id, title(id), intent)
        }
    }

    /**
     * Opens every window that has neither an activity nor a task, as when it
     * mapped while nothing of pocketnix showed and its notification was
     * denied or dismissed. A window whose activity Android destroyed keeps
     * its task, and Recents brings it back.
     */
    private fun launchPending() = onMain("launching waiting windows") {
        if (!Foreground.visible) return@onMain
        val ids = synchronized(lock) {
            state.windows.values.filter { it.toplevel.tab.isEmpty() }.map { it.id }
                .filter { id -> views[id]?.let { it.host == null && !it.closeAsked && !it.claimed } == true }
        }
        if (ids.isEmpty()) return@onMain
        val tasks = app.getSystemService(ActivityManager::class.java).appTasks
        val windows = tasks.mapNotNull { runCatching { it.windowId }.getOrNull() }
        // A launch's task shows its window under the launch's URI.
        val taskIds = tasks.mapNotNull { runCatching { it.taskInfo.taskId }.getOrNull() }
        for (id in ids) if (id !in windows && views[id]?.taskId !in taskIds) openWindow(id)
    }

    private fun notifyWindow(id: Int, title: String, intent: Intent) {
        // Without it the window still opens with pocketnix (launchPending).
        if (app.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) return
        val nm = app.getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(NotificationChannel(CHANNEL, app.getString(R.string.gui_channel), NotificationManager.IMPORTANCE_HIGH))
        val pi = PendingIntent.getActivity(app, id, intent, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        val n = Notification.Builder(app, CHANNEL)
            .setSmallIcon(R.drawable.ic_pocketnix)
            .setContentTitle(title)
            .setContentText("Opened a window. Tap to show it.")
            .setContentIntent(pi)
            .setAutoCancel(true)
            .build()
        nm.notify(NOTIFY_TAG, id, n)
    }

    /** Takes the window off screen and out of Recents, its activity or not. */
    private fun closeWindow(id: Int) {
        val v = synchronized(lock) {
            views.remove(id)?.also {
                it.renderer?.destroy()
                it.renderer = null
            }
        }
        onMain("window ${id.u}: close") {
            app.getSystemService(NotificationManager::class.java).cancel(NOTIFY_TAG, id)
            val h = v?.host
            if (h != null) h.dismiss() else removeTask(id, v?.taskId ?: -1)
        }
    }

    /** On the main thread: removes window [id]'s task, found by its URI or, for a launch's, by [taskId]. */
    private fun removeTask(id: Int, taskId: Int) {
        GuiWindowActivity.removeTasks(app) { it.windowId == id }
        // A launch's task names its app, not the window.
        if (taskId == -1) return
        for (t in app.getSystemService(ActivityManager::class.java).appTasks) {
            runCatching { if (t.taskInfo.taskId == taskId) t.finishAndRemoveTask() }
        }
    }

    /** Under [lock]: the windows in tabs changed, or their titles did. */
    private fun publishTabs() {
        _tabWindows.value = state.windows.values.filter { it.toplevel.tab.isNotEmpty() }
            .map { WindowTab(it.id, it.toplevel.tab, title(it.toplevel)) }
    }

    /**
     * Window [id] now shows in a terminal tab: its own task goes, without
     * the client hearing of it. A [new] window (a terminal just started it)
     * gets its tab selected; one the user [moved] also brings the terminal
     * screen forward.
     */
    private fun intoTab(id: Int, new: Boolean, moved: Boolean) = onMain("window ${id.u}: into a tab") {
        app.getSystemService(NotificationManager::class.java).cancel(NOTIFY_TAG, id)
        val v = views[id] ?: return@onMain
        when (val h = v.host) {
            is GuiWindowActivity -> h.dismiss()
            null -> removeTask(id, v.taskId)
            else -> {}
        }
        v.taskId = -1
        if (moved) {
            showTab(id)
        } else if (new) {
            selectTab(id)
        }
    }

    /** Window [id] left its tab for a task of its own. */
    private fun outOfTab(id: Int) = onMain("window ${id.u}: out of its tab") {
        (views[id]?.host as? TabWindowView)?.release()
        openWindow(id)
    }

    /** Selects window [id]'s tab and brings the terminal screen forward, while Android lets pocketnix. */
    private fun showTab(id: Int) {
        selectTab(id)
        if (Foreground.visible) {
            app.startActivity(Intent(app, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
        }
    }

    private fun onMain(what: String, block: () -> Unit) {
        main.post { guard(what, block) }
    }

    // --- From GuiWindowActivity, on the main thread ---

    /** Binds an activity to toplevel [id] of [instance]; false when that window is gone or shows in a tab. */
    internal fun bind(a: GuiWindowActivity, instance: String, id: Int): Boolean = guard("window ${id.u}: bind", false) {
        if (!bindHost(a, id) { instance == state.instance && it.toplevel.tab.isEmpty() }) return@guard false
        views[id]?.taskId = a.taskId
        app.getSystemService(NotificationManager::class.java).cancel(NOTIFY_TAG, id)
        true
    }

    /** Binds a tab's view to window [id]; false when the window is gone or has left its tab. */
    internal fun bindTab(t: TabWindowView, id: Int): Boolean = guard("window ${id.u}: bind a tab", false) {
        bindHost(t, id) { it.toplevel.tab.isNotEmpty() }
    }

    /** Makes [h] what shows window [id], if [ok] for the window; the host it had stops showing it. */
    private inline fun bindHost(h: WindowHost, id: Int, ok: (GuiState.Window) -> Boolean): Boolean {
        val previous = synchronized(lock) {
            val w = state.windows[id] ?: return false
            if (!ok(w)) return false
            val v = views[id] ?: return false
            v.host.also { v.host = h }
        }
        views[id]?.closeAsked = false
        previous?.takeIf { it !== h }?.dismiss()
        return true
    }

    /** The host is gone; [userClosed] (not unmapped by the guest) asks the client to close, and relaunches the window if it stays. */
    internal fun unbind(h: WindowHost, userClosed: Boolean) = guard("window ${h.windowId.u}: unbind") {
        val id = h.windowId
        val v = views[id] ?: return@guard
        synchronized(lock) {
            if (v.host !== h) return@guard
            v.host = null
            v.renderer?.destroy()
            v.renderer = null
        }
        v.sent = v.sent?.copy(focused = false, visible = false)?.also { link.send(Gui.window(it)) }
        if (!userClosed) return@guard
        v.closeAsked = true
        link.send(Gui.close(id))
        // The client may keep it (an unsaved document): bring it back.
        main.postDelayed({
            guard("window ${id.u}: relaunch") {
                v.closeAsked = false
                val stays = synchronized(lock) { state.windows.containsKey(id) } && views[id]?.let { it.host == null } == true
                if (stays) openWindow(id)
            }
        }, RELAUNCH_MS)
    }

    // A host finishing for an earlier window of the same id (a remap, a new
    // compositor instance) still gets callbacks: only the bound one speaks for the window.

    /** [h]'s surface is [width] × [height] under [parent]: show the window's layers there. */
    internal fun attach(h: WindowHost, parent: SurfaceControl, width: Int, height: Int) = guard("window ${h.windowId.u}: attach") {
        val id = h.windowId
        val r = synchronized(lock) {
            val v = views[id]?.takeIf { it.host === h } ?: return@guard
            val t = SurfaceControl.Transaction()
            val old = v.renderer
            if (old != null && old.parent === parent) {
                old.resize(t, width, height)
                t.apply()
                return@guard
            }
            old?.destroy()
            WindowRenderer(parent, id, lock, ::presented) { redraw(it, first = false) }.also {
                v.renderer = it
                it.resize(t, width, height)
                t.apply()
            }
        }
        // The first frame copies every surface whole: not on the main thread.
        redraw(r, first = true)
    }

    /** [h]'s surface is going away. */
    internal fun detach(h: WindowHost) = guard("window ${h.windowId.u}: detach") {
        synchronized(lock) {
            val v = views[h.windowId]?.takeIf { it.host === h } ?: return@guard
            v.renderer?.destroy()
            v.renderer = null
        }
    }

    internal fun windowState(h: WindowHost, s: WindowState) {
        val v = views[s.id]?.takeIf { it.host === h } ?: return
        if (v.sent == s) return
        v.sent = s
        link.send(Gui.window(s))
    }

    internal fun send(frame: ByteArray) = link.send(frame)

    /** The next `ime` serial on this connection. */
    internal fun nextImeSerial(): Int = imeSerial.incrementAndGet()

    /** The focused text input as last announced. */
    internal val currentTextInput: TextInputState get() = textInput

    /** A key event came from input device [deviceId]: the guest's keymap follows the keyboard last typed on. */
    internal fun typedOn(deviceId: Int) = keyboardLayouts.typedOn(deviceId)

    /** How touches reach window [id]: its app's input mode, Pointer where Touch can't reach the client. */
    fun inputMode(id: Int): InputMode {
        val m = appSettings.of(appKey(id)).input
        return if (m == InputMode.TOUCH && !hasTouch(id)) InputMode.POINTER else m
    }

    /**
     * Window [id]'s scale for a [width] × [height] area at the display's
     * [density]: its app's size preset, lowered by the fit while the
     * client's minimum doesn't fit.
     */
    internal fun fitScale(id: Int, width: Int, height: Int, density: Float): Float {
        val t = synchronized(lock) { state.windows[id]?.toplevel }
        val factor = appSettings.of(appKey(t)).size.factor
        // A compositor that sends no scale leaves the minimum's scale a guess that can be stale: no fit.
        val at = t?.scale ?: 0f
        return WindowFit.scale(
            density,
            factor,
            width,
            height,
            WindowFit.logical(t?.minWidth ?: 0, at),
            WindowFit.logical(t?.minHeight ?: 0, at)
        )
    }

    /** Window [id]'s geometry in window pixels, larger than its view when the client's minimum is. */
    internal fun contentSize(id: Int): Pair<Int, Int> = synchronized(lock) { state.windows[id]?.toplevel }?.let { it.width to it.height } ?: (0 to 0)

    /** The id window [id]'s settings go by: its app-list entry's, else its app id; "" for neither. */
    fun appKey(id: Int): String = appKey(synchronized(lock) { state.windows[id]?.toplevel })

    private fun appKey(t: GuiMessage.Toplevel?): String = t?.app?.ifEmpty { t.appId }.orEmpty()

    /** Whether window [id] is to be full screen; main thread only. */
    internal fun fullscreen(id: Int): Boolean = views[id]?.fullscreen == true

    /** The user took window [id] in or out of full screen; main thread only. */
    internal fun fullscreenChanged(id: Int, on: Boolean) {
        views[id]?.fullscreen = on
    }

    /** Whether window [id]'s client has a wl_touch, so Touch mode can reach it. */
    fun hasTouch(id: Int): Boolean = synchronized(lock) { state.windows[id]?.toplevel?.touch } == true

    /** What shows window [id], for its menu's zoom items; null while nothing does. */
    fun surfaceOf(id: Int): WindowSurface? = views[id]?.host?.surface

    /** Asks window [id]'s client to close, as its close button would; the client may ask first, or refuse. */
    fun requestClose(id: Int) = link.send(Gui.close(id))

    /**
     * Moves window [id] into the terminal tab with agent id [tab], or ""
     * to a task of its own. The window moves when the compositor's
     * `toplevel` says so, as after a replay.
     */
    fun move(id: Int, tab: String) {
        moving += id
        link.send(Gui.move(id, tab))
    }

    /** A view showing tab window [id], for the terminal screen while that tab is selected; [release] it after. */
    fun tabView(context: Context, id: Int): android.view.View = TabWindowView(context, this, id).let { t -> t.view.also { it.tag = t } }

    /** Ends what [tabView] made. */
    fun release(view: android.view.View) {
        (view.tag as? TabWindowView)?.release()
    }

    // --- Launches, from a splash on the main thread ---

    /** [a] shows launch [id]'s splash and binds the window it opens. */
    internal fun own(id: String, a: GuiWindowActivity) {
        owners[id] = a
    }

    /** [a] no longer waits on launch [id]; a window it claimed but never bound opens on its own. */
    internal fun disown(id: String, a: GuiWindowActivity) = guard("launch $id: disown") {
        if (!owners.remove(id, a)) return@guard
        val opened = launches[id]?.state?.value as? LaunchState.Opened ?: return@guard
        val v = views[opened.window] ?: return@guard
        if (v.claimed && v.host == null) {
            v.claimed = false
            openWindow(opened.window)
        }
    }

    /**
     * Binds [a], a launch's splash, to [window]: the window the launch
     * opened, or the one it activated when that has no activity yet. False
     * when the window is gone or another activity shows it.
     */
    internal fun bindLaunched(a: GuiWindowActivity, window: Int): Boolean = guard("window ${window.u}: bind a launch", false) {
        val instance = synchronized(lock) {
            val v = views[window] ?: return@guard false
            if (v.host != null && v.host !== a) return@guard false
            state.instance
        } ?: return@guard false
        bind(a, instance, window).also { if (it) views[window]?.claimed = false }
    }

    /** Brings [window]'s task or tab forward, opening it when it has none; only while Android lets pocketnix. */
    internal fun raise(window: Int) {
        if (!Foreground.visible) return
        val v = views[window] ?: return
        if (synchronized(lock) { state.windows[window]?.toplevel?.tab.orEmpty() }.isNotEmpty()) {
            v.claimed = false
            showTab(window)
            return
        }
        val task = app.getSystemService(ActivityManager::class.java).appTasks
            .firstOrNull { runCatching { it.taskInfo.taskId }.getOrNull() == v.taskId }
        if (task != null && v.host != null) {
            task.moveToFront()
        } else if (!v.claimed) {
            openWindow(window)
        }
    }

    /** Starts entry [app] for a splash; it becomes a recent app. */
    internal fun launch(app: String, action: String): Launch {
        catalog.launched(app)
        shortcuts.update()
        return launches.start(app, action)
    }

    /** The app-list entry toplevel [window] belongs to, if the list has it. */
    internal fun appOf(window: Int): AppEntry? = synchronized(lock) { state.windows[window]?.toplevel?.app }?.let(catalog::app)

    /** The titles of the open windows, for asking before the VM stops. */
    fun windowTitles(): List<String> = synchronized(lock) { state.windows.keys.toList() }.map(::title)

    /** An icon's size in pixels: a launcher grid's, or [large] for a splash or shortcut. */
    fun iconSize(large: Boolean): Int {
        val dp = if (large) LARGE_ICON_DP else SMALL_ICON_DP
        return (dp * app.resources.displayMetrics.density).roundToInt().coerceIn(Gui.MIN_ICON_SIZE, Gui.MAX_ICON_SIZE)
    }

    /** Asks for every icon the list names that isn't cached, once per connection. */
    private fun requestIcons() {
        val wanted = catalog.missingIcons(listOf(iconSize(false), iconSize(true)).distinct())
        val ask = synchronized(lock) { wanted.filter { askedIcons.add(it) } }
        for ((key, size) in ask) link.send(Gui.icon(key, size))
    }

    internal fun title(id: Int): String = views[id]?.title?.ifEmpty { null } ?: app.getString(R.string.app_name)

    private fun title(t: GuiMessage.Toplevel): String = t.title.ifEmpty { t.appId }

    /** The display a new window opens on. */
    private fun output(): ByteArray {
        val dm = app.resources.displayMetrics
        val display = app.getSystemService(DisplayManager::class.java).getDisplay(Display.DEFAULT_DISPLAY)
        val refresh = ((display?.refreshRate ?: 60f) * 1000).roundToInt()
        frameNs = 1_000_000_000_000L / refresh
        return Gui.output(dm.widthPixels, dm.heightPixels, dm.densityDpi / 160f, refresh)
    }

    /** Debug dumps: the connection, every window, the hash checks, the app list and the launches waited on. */
    fun dump(): List<String> = synchronized(lock) {
        listOf(
            "connected=${link.connected} ready=${_ready.value} instance=${state.instance} hashes ok=$hashesOk bad=$hashesBad",
            "apps=${catalog.apps.size} recent=${catalog.recent.map { it.id }} launcher=${shortcuts.launcherEnabled} " +
                "launches=${owners.keys.map { "$it:${launches[it]?.state?.value}" }}"
        ) +
            state.windows.values.map { w ->
                val v = views[w.id]
                val surfaces = state.surfaces.values.filter { it.window == w.id }
                    .joinToString(" ") { "${it.id.u}:${it.role}:${it.content.width}x${it.content.height}" }
                "window ${w.id.u} '${title(w.id)}' app=${w.toplevel.app} launch=${w.toplevel.launch} tab=${w.toplevel.tab} " +
                    "host=${v?.host?.javaClass?.simpleName} shown=${v?.renderer != null} " +
                    "settings=${appSettings.of(appKey(w.toplevel))} min=${w.toplevel.minWidth}x${w.toplevel.minHeight}@${w.toplevel.scale} " +
                    "geometry=${w.toplevel.width}x${w.toplevel.height} zoom=${v?.host?.surface?.zoom?.let { "${it.scale}@${it.x},${it.y}" }} " +
                    "seq=${w.lastSeq.u} sent=${v?.sent} surfaces=[$surfaces]"
            }
    }

    companion object {
        private const val TAG = "GuiSession"
        private const val CHANNEL = "gui-windows"
        private const val NOTIFY_TAG = "gui-window"
        private const val RELAUNCH_MS = 2_000L
        private const val SMALL_ICON_DP = 48
        private const val LARGE_ICON_DP = 96

        /** Removes every window task: at process start, those belong to a dead session. */
        fun sweep(context: Context) = guard("sweeping window tasks") {
            GuiWindowActivity.removeTasks(context) { true }
        }
    }
}

/**
 * Runs [block] and logs what it throws, returning [fallback] then. For the
 * main thread and callbacks Android runs, where an exception would kill the
 * process and so the VM.
 */
internal inline fun <T> guard(what: String, fallback: T, block: () -> T): T = try {
    block()
} catch (e: Exception) {
    Log.e("GuiSession", "$what failed", e)
    fallback
}

internal inline fun guard(what: String, block: () -> Unit) = guard(what, Unit, block)

/**
 * A translucent helper over another app (a dialog, a picker): it doesn't
 * make pocketnix visible to [Foreground], so a window waiting to open doesn't
 * cover it.
 */
interface Helper

/** Whether any of the app's activities but a [Helper] is started: Android only lets a visible app launch activities. */
object Foreground : android.app.Application.ActivityLifecycleCallbacks {
    @Volatile private var started = 0
    private var created = false
    private val listeners = CopyOnWriteArrayList<() -> Unit>()
    val visible: Boolean get() = started > 0

    /** Calls [listener] on the main thread whenever the app becomes visible. */
    fun onVisible(listener: () -> Unit) {
        listeners += listener
    }

    override fun onActivityStarted(activity: android.app.Activity) {
        if (activity is Helper) return
        if (started++ == 0) for (l in listeners) l()
    }

    override fun onActivityStopped(activity: android.app.Activity) {
        if (activity is Helper) return
        started = (started - 1).coerceAtLeast(0)
    }

    override fun onActivityCreated(activity: android.app.Activity, savedInstanceState: android.os.Bundle?) {
        if (created) return
        created = true
        // A launch's task can't be told from a fresh one while the process starts, but by its first
        // activity every other launch task is a card from a dead session.
        guard("sweeping launch tasks") {
            for (t in activity.getSystemService(ActivityManager::class.java).appTasks) {
                runCatching { if (GuiWindowActivity.isLaunchTask(t) && t.taskInfo.taskId != activity.taskId) t.finishAndRemoveTask() }
            }
        }
    }
    override fun onActivityResumed(activity: android.app.Activity) {}
    override fun onActivityPaused(activity: android.app.Activity) {}
    override fun onActivitySaveInstanceState(activity: android.app.Activity, outState: android.os.Bundle) {}
    override fun onActivityDestroyed(activity: android.app.Activity) {}
}

/** The window id in a task's base intent, or 0 for another kind of task. */
internal val ActivityManager.AppTask.windowId: Int
    get() = taskInfo.baseIntent.data?.let(GuiWindowActivity::parse)?.second ?: 0

internal fun windowUri(instance: String, id: Int): Uri = Uri.Builder().scheme(GuiWindowActivity.SCHEME).authority("window")
    .appendPath(instance).appendPath(id.u.toString()).build()
