package dev.nickolaj.pocketnix.gui

import android.app.ActivityManager
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.hardware.display.DisplayManager
import android.net.Uri
import android.os.Handler
import android.os.Looper
import android.util.Log
import android.view.Display
import android.view.SurfaceControl
import dev.nickolaj.pocketnix.R
import dev.nickolaj.pocketnix.agent.AgentDialer
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.Executors
import kotlin.math.roundToInt

/**
 * The guest's GUI windows: the one GUI stream while the guest has GUI on,
 * the windows and surfaces it describes, and the Android window
 * ([GuiWindowActivity]) showing each toplevel. Owned by the app's Vm, so
 * windows outlive their activities and a reconnect.
 */
class GuiSession(private val app: Context, appVersion: String) : GuiLink.Listener {
    // Guards the state and the renderers; the link's thread, the render thread and the main thread take it.
    private val lock = Any()
    private val state = GuiState(GuiNative)
    private val link = GuiLink(appVersion, this)
    private val main = Handler(Looper.getMainLooper())
    // Draws outside the link's thread: a window's first frame, and frames that waited for a buffer.
    private val renderThread = Executors.newSingleThreadExecutor { r -> Thread(r, "gui-render").apply { isDaemon = true } }

    private class View(val id: Int) {
        @Volatile var activity: GuiWindowActivity? = null

        /** Under [lock]. */
        var renderer: WindowRenderer? = null

        /** The last `window` message, re-sent on every connect; written on the main thread only. */
        @Volatile var sent: WindowState? = null

        @Volatile var title = ""
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
            main,
        )
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
        if (closeWindows) {
            val ids = synchronized(lock) { state.windows.keys.toList() }
            for (id in ids) closeWindow(id)
            synchronized(lock) {
                state.windows.clear()
                state.surfaces.clear()
            }
        }
    }

    // --- The link's thread ---

    override fun connected(hello: GuiMessage.Hello) {
        val dropped = synchronized(lock) { state.hello(hello) }
        for (w in dropped) closeWindow(w.id)
        link.send(output())
        for (v in views.values) v.sent?.let { link.send(Gui.window(it)) }
    }

    override fun message(msg: GuiMessage) {
        when (msg) {
            is GuiMessage.Group -> {
                // Decompressing is the slow part, and needs no state: keep it out of the lock.
                val g = GuiMessage.Group(msg.commits.map { it.unpacked(GuiNative) })
                synchronized(lock) {
                    state.apply(g)
                    render(g)
                }
            }
            is GuiMessage.Toplevel -> {
                val new = synchronized(lock) {
                    state.toplevel(msg).also { if (it) views[msg.id] = View(msg.id) }
                }
                views[msg.id]?.title = title(msg)
                if (new) launch(msg.id) else onMain("window ${msg.id.u}: title") { views[msg.id]?.activity?.toplevelChanged() }
            }
            is GuiMessage.ToplevelRemoved -> {
                synchronized(lock) {
                    state.toplevelRemoved(msg.id)
                }
                closeWindow(msg.id)
            }
            is GuiMessage.ToplevelRequest -> if (msg.request == "minimize") {
                onMain("window ${msg.id.u}: minimize") { views[msg.id]?.activity?.moveTaskToBack(true) }
            }
            is GuiMessage.Surface -> synchronized(lock) {
                val moved = state.surface(msg) ?: return@synchronized
                views[moved]?.renderer?.let { r -> SurfaceControl.Transaction().also { r.remove(it, msg.id) }.apply() }
            }
            is GuiMessage.SurfaceRemoved -> synchronized(lock) {
                val s = state.surfaceRemoved(msg.id) ?: return@synchronized
                views[s.window]?.renderer?.let { r -> SurfaceControl.Transaction().also { r.remove(it, s.id) }.apply() }
            }
            is GuiMessage.Cursor -> onMain("window ${msg.window.u}: cursor") { views[msg.window]?.activity?.cursor(msg.name) }
            GuiMessage.Ready -> {
                val windows = synchronized(lock) {
                    val (windows, surfaces) = state.ready()
                    val t = SurfaceControl.Transaction()
                    for (s in surfaces) views[s.window]?.renderer?.remove(t, s.id)
                    t.apply()
                    windows
                }
                for (w in windows) closeWindow(w.id)
            }
            is GuiMessage.Error -> Log.w(TAG, "compositor error: ${msg.message}")
            is GuiMessage.Hello -> throw GuiProtocolException("a second hello")
        }
    }

    override fun disconnected(error: Throwable?) {
        Log.i(TAG, "disconnected${error?.let { ": ${it.message}" } ?: ""}")
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
                if (!first && r.deferred == 0) return@guard
                val t = SurfaceControl.Transaction()
                r.render(t, state.surfaces.values.filter { it.window == v.id }, if (first) state.windows[v.id]?.lastSeq ?: 0 else 0)
                t.apply()
            }
        }
    }

    // --- Windows ---

    /** Opens the toplevel's Android window: directly while a pocketnix window is visible, else through a notification. */
    private fun launch(id: Int) = onMain("window ${id.u}: launch") {
        val instance = synchronized(lock) {
            if (state.windows[id] == null || views[id]?.activity != null) return@onMain
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

    private fun notifyWindow(id: Int, title: String, intent: Intent) {
        val nm = app.getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(NotificationChannel(CHANNEL, app.getString(R.string.gui_channel), NotificationManager.IMPORTANCE_HIGH))
        val pi = PendingIntent.getActivity(app, id, intent, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        val n = Notification.Builder(app, CHANNEL)
            .setSmallIcon(R.drawable.ic_pocketnix)
            .setContentTitle(title)
            .setContentText(app.getString(R.string.gui_window_opened))
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
            val a = v?.activity
            if (a != null) a.closeByGuest() else GuiWindowActivity.removeTasks(app) { it.windowId == id }
        }
    }

    private fun onMain(what: String, block: () -> Unit) {
        main.post { guard(what, block) }
    }

    // --- From GuiWindowActivity, on the main thread ---

    /** Binds an activity to toplevel [id] of [instance]; false when that window is gone. */
    internal fun bind(a: GuiWindowActivity, instance: String, id: Int): Boolean = guard("window ${id.u}: bind", false) {
        val previous = synchronized(lock) {
            if (instance != state.instance || state.windows[id] == null) return@guard false
            val v = views[id] ?: return@guard false
            v.activity.also { v.activity = a }
        }
        previous?.takeIf { it !== a }?.closeByGuest()
        app.getSystemService(NotificationManager::class.java).cancel(NOTIFY_TAG, id)
        true
    }

    /** The activity is gone; [userClosed] (not unmapped by the guest) asks the client to close, and relaunches the window if it stays. */
    internal fun unbind(a: GuiWindowActivity, userClosed: Boolean) = guard("window ${a.windowId.u}: unbind") {
        val id = a.windowId
        val v = views[id] ?: return@guard
        synchronized(lock) {
            if (v.activity !== a) return@guard
            v.activity = null
            v.renderer?.destroy()
            v.renderer = null
        }
        v.sent = v.sent?.copy(focused = false, visible = false)?.also { link.send(Gui.window(it)) }
        if (!userClosed) return@guard
        link.send(Gui.close(id))
        // The client may keep it (an unsaved document): bring it back.
        main.postDelayed({
            guard("window ${id.u}: relaunch") {
                val stays = synchronized(lock) { state.windows.containsKey(id) } && views[id]?.let { it.activity == null } == true
                if (stays) launch(id)
            }
        }, RELAUNCH_MS)
    }

    /** The window's surface is [width] × [height] under [parent]: show the window's layers there. */
    internal fun attach(id: Int, parent: SurfaceControl, width: Int, height: Int) = guard("window ${id.u}: attach") {
        val r = synchronized(lock) {
            val v = views[id] ?: return@guard
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

    /** The window's surface is going away. */
    internal fun detach(id: Int) = guard("window ${id.u}: detach") {
        synchronized(lock) {
            val v = views[id] ?: return@guard
            v.renderer?.destroy()
            v.renderer = null
        }
    }

    internal fun windowState(s: WindowState) {
        val v = views[s.id] ?: return
        if (v.sent == s) return
        v.sent = s
        link.send(Gui.window(s))
    }

    internal fun send(frame: ByteArray) = link.send(frame)

    internal fun title(id: Int): String = views[id]?.title?.ifEmpty { null } ?: app.getString(R.string.app_name)

    private fun title(t: GuiMessage.Toplevel): String = t.title.ifEmpty { t.appId }

    /** The display a new window opens on. */
    private fun output(): ByteArray {
        val dm = app.resources.displayMetrics
        val display = app.getSystemService(DisplayManager::class.java).getDisplay(Display.DEFAULT_DISPLAY)
        val refresh = ((display?.refreshRate ?: 60f) * 1000).roundToInt()
        return Gui.output(dm.widthPixels, dm.heightPixels, dm.densityDpi / 160f, refresh)
    }

    /** Debug dumps: the connection, every window and the hash checks. */
    fun dump(): List<String> = synchronized(lock) {
        listOf("connected=${link.connected} instance=${state.instance} hashes ok=$hashesOk bad=$hashesBad") +
            state.windows.values.map { w ->
                val v = views[w.id]
                val surfaces = state.surfaces.values.filter { it.window == w.id }
                    .joinToString(" ") { "${it.id.u}:${it.role}:${it.content.width}x${it.content.height}" }
                "window ${w.id.u} '${title(w.id)}' activity=${v?.activity != null} shown=${v?.renderer != null} " +
                    "seq=${w.lastSeq.u} sent=${v?.sent} surfaces=[$surfaces]"
            }
    }

    companion object {
        private const val TAG = "GuiSession"
        private const val CHANNEL = "gui-windows"
        private const val NOTIFY_TAG = "gui-window"
        private const val RELAUNCH_MS = 2_000L

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

/** Whether any of the app's activities is started: Android only lets a visible app launch activities. */
object Foreground : android.app.Application.ActivityLifecycleCallbacks {
    @Volatile private var started = 0
    val visible: Boolean get() = started > 0

    override fun onActivityStarted(activity: android.app.Activity) {
        started++
    }

    override fun onActivityStopped(activity: android.app.Activity) {
        started = (started - 1).coerceAtLeast(0)
    }

    override fun onActivityCreated(activity: android.app.Activity, savedInstanceState: android.os.Bundle?) {}
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
