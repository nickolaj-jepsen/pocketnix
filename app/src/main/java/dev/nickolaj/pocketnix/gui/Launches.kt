package dev.nickolaj.pocketnix.gui

import dev.nickolaj.pocketnix.agent.ControlChannel
import dev.nickolaj.pocketnix.engine.VmState
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import java.util.UUID

/** What became of a launch (gui-protocol.md, Launching). */
sealed interface LaunchState {
    /** Queued until a connection is ready, or sent and not answered. */
    data object Pending : LaunchState

    /** The compositor started its process. */
    data object Started : LaunchState

    /** Ended with a new window. */
    data class Opened(val window: Int) : LaunchState

    /** Ended by activating a window that already existed (a single-instance app's). */
    data class Activated(val window: Int) : LaunchState

    data class Failed(val reason: String, val message: String) : LaunchState

    data object Cancelled : LaunchState

    val ended: Boolean get() = this !is Pending && this !is Started

    companion object {
        const val UNKNOWN = "unknown"
        const val EXEC = "exec"
        const val TIMEOUT = "timeout"
        const val CLOSED = "closed"
    }
}

/** One start of app-list entry [app], with [action] ("" for none), its windows going to [tab]. */
class Launch internal constructor(val id: String, val app: String, val action: String, val tab: String) {
    private val _state = MutableStateFlow<LaunchState>(LaunchState.Pending)
    val state: StateFlow<LaunchState> = _state.asStateFlow()

    /** The compositor instance it was last sent to, or null while it never was. */
    internal var sentTo: String? = null

    internal fun set(s: LaunchState): Boolean {
        if (_state.value.ended) return false
        _state.value = s
        return true
    }
}

/**
 * The app's launches, by the protocol's rules: queued while no connection
 * has finished its replay, sent again after every replay until they end, so
 * a tap that boots the VM or outlives a reconnect still opens its window.
 * [send] queues a frame on the current connection. Thread-safe.
 */
class Launches(private val send: (ByteArray) -> Unit, private val newId: () -> String = { UUID.randomUUID().toString() }) {
    private val launches = LinkedHashMap<String, Launch>()
    private var ready = false
    private var instance: String? = null

    // Cancelled while disconnected after reaching [instance]: told after the next replay of that instance.
    private val cancelLater = LinkedHashSet<String>()

    @Synchronized fun start(app: String, action: String = "", tab: String = ""): Launch {
        val l = Launch(newId(), app, action, tab)
        launches[l.id] = l
        if (ready) sendLaunch(l)
        return l
    }

    @Synchronized operator fun get(id: String): Launch? = launches[id]

    /** The user stopped waiting: a launch not yet ended is cancelled, its process stopped if it has no window. */
    @Synchronized fun cancel(id: String) {
        val l = launches[id] ?: return
        if (!l.set(LaunchState.Cancelled)) return
        val to = l.sentTo ?: return
        if (to != instance) return
        if (ready) send(Gui.launchCancel(id)) else cancelLater += id
    }

    /** Drops an ended launch nobody waits on any more. */
    @Synchronized fun forget(id: String) {
        if (launches[id]?.state?.value?.ended == true) launches.remove(id)
    }

    /** A connection said hello: its replay follows. */
    @Synchronized fun connected(instance: String) {
        ready = false
        // A new compositor knows no earlier launch: they start afresh after its replay.
        if (instance != this.instance) cancelLater.clear()
        this.instance = instance
    }

    @Synchronized fun disconnected() {
        ready = false
    }

    /** The replay ended: tells of cancels it missed and sends every launch still waiting. */
    @Synchronized fun ready() {
        ready = true
        for (id in cancelLater) send(Gui.launchCancel(id))
        cancelLater.clear()
        for (l in launches.values) if (!l.state.value.ended) sendLaunch(l)
    }

    /** A toplevel arrived; returns the launch it ended, if this was a window naming a launch that waited. */
    @Synchronized fun toplevel(t: GuiMessage.Toplevel): Launch? {
        if (t.isDialog || t.launch.isEmpty()) return null
        return launches[t.launch]?.takeIf { it.set(LaunchState.Opened(t.id)) }
    }

    /** Returns the launch [m] ended, if any. */
    @Synchronized fun activate(m: GuiMessage.Activate): Launch? {
        if (m.launch.isEmpty()) return null
        return launches[m.launch]?.takeIf { it.set(LaunchState.Activated(m.window)) }
    }

    @Synchronized fun started(id: String) {
        val l = launches[id] ?: return
        if (l.state.value == LaunchState.Pending) l.set(LaunchState.Started)
    }

    /** Returns the launch [m] ended, if any. */
    @Synchronized fun failed(m: GuiMessage.LaunchFailed): Launch? =
        launches[m.id]?.takeIf { it.set(LaunchState.Failed(m.reason, m.message)) }

    private fun sendLaunch(l: Launch) {
        l.sentTo = instance
        send(Gui.launch(l.id, l.app, l.action, l.tab))
    }
}

/** What a launch's splash says, and whether it ended without a window. */
data class SplashStatus(val text: String, val failed: Boolean = false, val canRetry: Boolean = false)

/** Why app-list entry [name] can't launch any more, on its splash and its disabled pins. */
fun appGone(name: String): String = "$name is no longer installed in the guest."

/**
 * The splash's line for launching [name]: the VM booting, the GUI stream
 * connecting, the app starting, or why none of it worked. [guiEnabled] is
 * the guest config's `gui.enable`, null before the agent sent one;
 * [guiReady] that the GUI stream finished a replay.
 */
fun splashStatus(
    name: String,
    vm: VmState,
    link: ControlChannel.Link,
    agentLate: Boolean,
    guiEnabled: Boolean?,
    guiReady: Boolean,
    launch: LaunchState,
): SplashStatus = when {
    launch is LaunchState.Failed -> when (launch.reason) {
        LaunchState.UNKNOWN -> SplashStatus(appGone(name), failed = true)
        LaunchState.CLOSED -> SplashStatus("$name opened and closed its window while pocketnix was away.", failed = true, canRetry = true)
        LaunchState.TIMEOUT -> SplashStatus("$name opened no window. ${launch.message}".trim(), failed = true, canRetry = true)
        else -> SplashStatus("$name couldn't start. ${launch.message}".trim(), failed = true, canRetry = true)
    }
    launch.ended -> SplashStatus("Opening $name…")
    vm is VmState.Failed -> SplashStatus("The VM failed: ${vm.message}", failed = true, canRetry = true)
    vm == VmState.Stopped -> SplashStatus("The VM is stopped.", failed = true, canRetry = true)
    link is ControlChannel.Link.Refused -> SplashStatus("The guest's agent refused pocketnix: ${link.reason}", failed = true)
    link !is ControlChannel.Link.Up && agentLate ->
        SplashStatus("The guest's agent hasn't answered in two minutes. pocketnix's console tab shows the boot.")
    link !is ControlChannel.Link.Up -> SplashStatus("Starting the VM…")
    guiEnabled == false -> SplashStatus("GUI apps are off in this guest (pocketnix.guest.gui.enable).", failed = true)
    !guiReady -> SplashStatus("Connecting to the guest's display…")
    else -> SplashStatus("Starting $name…")
}
