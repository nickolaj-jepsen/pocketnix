package dev.nickolaj.pocketnix

import android.content.ClipData
import android.content.Context
import android.content.Intent
import android.graphics.Typeface
import android.util.Log
import dev.nickolaj.pocketnix.agent.AgentException
import dev.nickolaj.pocketnix.agent.ControlChannel
import dev.nickolaj.pocketnix.agent.FontCache
import dev.nickolaj.pocketnix.agent.GuestConfig
import dev.nickolaj.pocketnix.agent.GuestFiles
import dev.nickolaj.pocketnix.agent.HostStateSync
import dev.nickolaj.pocketnix.agent.PtyBackend
import dev.nickolaj.pocketnix.agent.Updates
import dev.nickolaj.pocketnix.agent.WakeHolder
import dev.nickolaj.pocketnix.engine.AvfEngine
import dev.nickolaj.pocketnix.engine.BootMenu
import dev.nickolaj.pocketnix.engine.ConsoleLog
import dev.nickolaj.pocketnix.engine.TcpEngine
import dev.nickolaj.pocketnix.engine.VmEngine
import dev.nickolaj.pocketnix.engine.VmState
import dev.nickolaj.pocketnix.files.GuestDocumentsProvider
import dev.nickolaj.pocketnix.files.PendingUploads
import dev.nickolaj.pocketnix.files.Transfers
import dev.nickolaj.pocketnix.forward.PortForwards
import dev.nickolaj.pocketnix.gui.GuiSession
import dev.nickolaj.pocketnix.host.HostMonitor
import dev.nickolaj.pocketnix.hostfs.HostFolderLinks
import dev.nickolaj.pocketnix.hostfs.SafFolder
import dev.nickolaj.pocketnix.hostfs.SharedFolders
import dev.nickolaj.pocketnix.service.VmService
import dev.nickolaj.pocketnix.service.wakeHoldersOf
import dev.nickolaj.pocketnix.terminal.Tab
import dev.nickolaj.pocketnix.terminal.TabAlerts
import dev.nickolaj.pocketnix.terminal.Tabs
import dev.nickolaj.pocketnix.terminal.TerminalTheme
import java.io.File
import kotlinx.coroutines.CoroutineExceptionHandler
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.flatMapLatest
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull
import org.json.JSONArray
import org.json.JSONObject

/**
 * The app's one VM and everything that lives with it. Owned by [PocketnixApp];
 * the foreground service keeps the process (and so this) alive while the VM runs.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class Vm(private val app: Context) {
    // A failed coroutine logs instead of crashing the app, and with it the VM.
    private val scope = CoroutineScope(
        SupervisorJob() + Dispatchers.Main.immediate + CoroutineExceptionHandler { _, e -> Log.e(TAG, "coroutine failed", e) }
    )
    val settings = Settings(app)
    val console = ConsoleLog()
    private val appVersion = "pocketnix ${BuildConfig.VERSION_NAME}"
    val control = ControlChannel(scope, appVersion)
    private val forwards = PortForwards(appVersion)
    private val fonts = FontCache(File(app.cacheDir, "fonts"), appVersion)
    val tabs = Tabs(app, console.backend(), appVersion)

    /** The guest's GUI windows, while its config turns GUI on. */
    val gui = GuiSession(app, appVersion)

    private val avf by lazy { AvfEngine(app) }
    private val _engine = MutableStateFlow(pickEngine())
    val engine: StateFlow<VmEngine> = _engine.asStateFlow()
    val state: StateFlow<VmState> = _engine.flatMapLatest { it.state }.stateIn(scope, SharingStarted.Eagerly, VmState.Stopped)

    private val _stopping = MutableStateFlow(false)
    val stopping: StateFlow<Boolean> = _stopping.asStateFlow()

    private val _forwardErrors = MutableStateFlow<List<String>>(emptyList())

    /** Forwards that couldn't listen, for the UI. */
    val forwardErrors: StateFlow<List<String>> = _forwardErrors.asStateFlow()

    private val _typeface = MutableStateFlow<Typeface?>(null)

    /** The module's font, or null for the default monospace. */
    val typeface: StateFlow<Typeface?> = _typeface.asStateFlow()

    /** Bumped when the module's colors change, for views to redraw. */
    private val _themeRevision = MutableStateFlow(0)
    val themeRevision: StateFlow<Int> = _themeRevision.asStateFlow()

    private val _agentLate = MutableStateFlow(false)

    /** The guest runs but its agent hasn't answered in [AGENT_LATE_MS]: maybe a generation that won't boot. */
    val agentLate: StateFlow<Boolean> = _agentLate.asStateFlow()

    /** What every status surface shows (see [vmPhase]). */
    val phase: StateFlow<VmPhase> =
        combine(state, control.link, stopping, agentLate, control.config) { s, link, stop, late, cfg -> vmPhase(s, link, stop, late, cfg?.hostname) }
            .stateIn(scope, SharingStarted.Eagerly, vmPhase(state.value, control.link.value, stopping.value, agentLate.value, control.config.value?.hostname))

    val updates: StateFlow<Updates?> get() = control.updates

    private val bridge = GuestBridge(app) { control.config.value?.hostname }
    val transfers = Transfers(app, scope, control, bridge, ::files) { control.config.value?.hostname }
    private var lastClipboardSent: Pair<String, List<String>>? = null

    /** The phone's network, power and timezone, watched while the agent is connected. */
    val host = HostMonitor(app)

    private val wakeLocksAllowed = MutableStateFlow(settings.wakeLocks)

    /** Who the phone stays awake for (see [wakeHoldersOf]), or null. */
    val wakeHolders: StateFlow<List<WakeHolder>?> =
        combine(control.wake, control.link, state, wakeLocksAllowed) { wake, link, s, allowed ->
            wakeHoldersOf(wake, link is ControlChannel.Link.Up, s == VmState.Running, allowed)
        }.stateIn(scope, SharingStarted.Eagerly, null)

    fun setWakeLocks(allowed: Boolean) {
        settings.wakeLocks = allowed
        wakeLocksAllowed.value = allowed
    }

    /** Android folders mounted in the guest. */
    val sharedFolders = SharedFolders(app)
    val hostFolders = HostFolderLinks(control, appVersion) { SafFolder(app.contentResolver, android.net.Uri.parse(it.uri)) }

    init {
        tabs.alerts = TabAlerts(app) { control.config.value?.hostname }
        gui.selectTab = tabs::selectWindow
        scope.launch { gui.tabWindows.collect { ws -> tabs.windowsChanged(ws.map { it.id }) } }
        scope.launch {
            control.events.collect { ev ->
                runCatching { if (!transfers.onEvent(ev, tabs.visible)) bridge.onEvent(ev, tabs.visible) }
                    .onFailure { Log.e(TAG, "agent push ${ev.optString("type")}", it) }
            }
        }
        // The Files app lists the root either way, but says when it can't be opened.
        scope.launch {
            control.link.map { it is ControlChannel.Link.Up }.distinctUntilChanged().collect { up ->
                if (up) notifyAnswersRefused = false
                GuestDocumentsProvider.rootsChanged(app)
                // A boot lands in a shell, not the console; a reconnect keeps the tabs it has.
                if (up && tabs.all().none { it.kind is Tab.Kind.Pty && it.session.isRunning }) openTab()
                // Saves that missed the guest go in now.
                val f = if (up) files() else null
                if (f != null) withContext(Dispatchers.IO) { PendingUploads.retry(app, f) }
            }
        }
        scope.launch {
            control.link.map { it is ControlChannel.Link.Up }.distinctUntilChanged().collect { up ->
                if (up) host.start() else host.stop()
            }
        }
        scope.launch { HostStateSync(control, host.state).run() }
        scope.launch {
            // Down, not Refused: an agent that answers (a version mismatch) booted fine.
            combine(state, control.link) { s, link -> s == VmState.Running && link is ControlChannel.Link.Down }
                .distinctUntilChanged()
                .collectLatest { waiting ->
                    _agentLate.value = false
                    if (waiting) {
                        delay(AGENT_LATE_MS)
                        _agentLate.value = true
                    }
                }
        }
        scope.launch {
            state.collect { s ->
                when (s) {
                    VmState.Running -> _engine.value.dialer?.let { control.start(it) }

                    VmState.Starting -> {}

                    else -> {
                        control.stop()
                        control.clear()
                    }
                }
            }
        }
        scope.launch {
            // An agent restart keeps the windows; the reconnect's replay restores them. A stopped guest takes them along.
            combine(control.config, control.link, state) { cfg, link, s ->
                Triple(cfg?.gui?.enable == true, link is ControlChannel.Link.Up, s == VmState.Running)
            }.distinctUntilChanged().collect { (on, up, running) ->
                val dialer = _engine.value.dialer
                when {
                    !on || !running -> gui.stop(closeWindows = true)
                    up && dialer != null -> gui.start(dialer, settings.guiHashes)
                    else -> gui.stop(closeWindows = false)
                }
            }
        }
        scope.launch {
            // Only a config the agent sent says anything: none arrives while the VM is off.
            control.config.map { cfg -> cfg?.let { it.gui?.enable == true } }.distinctUntilChanged().collect { on ->
                if (on != null) gui.shortcuts.setLauncherEnabled(on)
            }
        }
        scope.launch { control.config.map { it?.gui }.distinctUntilChanged().collect { gui.config(it) } }
        scope.launch {
            combine(control.config, state) { cfg, s -> cfg?.forwards.orEmpty() to (s == VmState.Running) }
                .distinctUntilChanged()
                .collectLatest { (fw, running) ->
                    val dialer = if (running) _engine.value.dialer else null
                    _forwardErrors.value = withContext(Dispatchers.IO) { forwards.apply(fw, dialer) }
                    // A port another app holds may come free (that app stopped): keep trying, less and less often.
                    var wait = FORWARD_RETRY_MS
                    while (_forwardErrors.value.isNotEmpty() && dialer != null) {
                        delay(wait)
                        wait = (wait * 2).coerceAtMost(FORWARD_RETRY_MAX_MS)
                        _forwardErrors.value = withContext(Dispatchers.IO) { forwards.apply(fw, dialer) }
                    }
                }
        }
        scope.launch {
            combine(control.link, sharedFolders.folders) { link, folders -> link to folders }.collectLatest { (link, folders) ->
                val dialer = _engine.value.dialer
                if (link is ControlChannel.Link.Up && dialer != null) hostFolders.share(link, dialer, folders)
            }
        }
        scope.launch {
            control.config.map { it?.theme }.distinctUntilChanged().collect { theme ->
                if (theme == null) return@collect
                applyColors(theme.colors)
                loadFont(theme.font, theme.fontSha256)
            }
        }
    }

    private fun pickEngine(): VmEngine = if (settings.tcpBackend) {
        val (host, port) = settings.tcpAddress.split(":").let { it[0] to (it.getOrNull(1)?.toIntOrNull() ?: 9100) }
        TcpEngine(host, port)
    } else {
        avf
    }

    /** Re-reads the backend setting; only while stopped. */
    fun reloadEngine() {
        if (state.value == VmState.Running || state.value == VmState.Starting) return
        _engine.value = pickEngine()
    }

    /**
     * Boots the VM. With [bootMenu], systemd-boot shows its menu in the
     * console tab (see [BootMenu]), the way back from a generation that
     * won't boot.
     */
    fun start(bootMenu: Boolean = false) {
        // The guest still runs while it powers off: start once it's down.
        if (_stopping.value) {
            scope.launch {
                _stopping.first { !it }
                state.first { it != VmState.Running && it != VmState.Starting }
                start(bootMenu)
            }
            return
        }
        reloadEngine()
        if (state.value == VmState.Running || state.value == VmState.Starting) return
        val e = _engine.value
        if (bootMenu && e.ownsGuest) {
            tabs.select(tabs.console.id)
            BootMenu(console, scope).tap()
        }
        // The engine claims Starting first, so the service never starts into a Stopped state and quits.
        e.start(settings::vm, console)
        startService()
    }

    private val _serviceRefused = MutableStateFlow(false)

    /** The VM runs without its foreground service, so Android may kill it with the app at any time. */
    val serviceRefused: StateFlow<Boolean> = _serviceRefused.asStateFlow()

    /** (Re)starts the foreground service while the VM runs; Android refuses it from the background. */
    fun startService() {
        if (state.value != VmState.Running && state.value != VmState.Starting && _engine.value.state.value != VmState.Starting) return
        _serviceRefused.value = runCatching { app.startForegroundService(Intent(app, VmService::class.java)) }
            .onFailure { Log.w(TAG, "foreground service refused: ${it.message}") }
            .isFailure
    }

    /** The service reports that it runs in the foreground. */
    fun serviceStarted() {
        _serviceRefused.value = false
    }

    /** Stops the VM at once (its agent may be what's broken) and boots it into the boot menu. */
    fun restartWithBootMenu() {
        scope.launch {
            if (state.value == VmState.Running || state.value == VmState.Starting) {
                _engine.value.forceStop()
                state.first { it != VmState.Running && it != VmState.Starting }
            }
            start(bootMenu = true)
        }
    }

    /** Asks the guest to reboot; the engine boots it again (into the default generation). */
    fun reboot() {
        scope.launch { control.request("reboot") }
    }

    /**
     * Runs an update request ([generation] for switch-generation); progress
     * arrives in [updates]. Returns why the agent refused, or null.
     */
    suspend fun updateRequest(type: String, generation: Int? = null): String? {
        val req = JSONObject().put("type", type)
        if (generation != null) req.put("generation", generation)
        return runCatching { control.call(req) }.exceptionOrNull()?.message
    }

    /** The guest's files (the file provider and shares), or null while the agent can't be reached. */
    fun files(): GuestFiles? {
        if (control.link.value !is ControlChannel.Link.Up) return null
        return _engine.value.dialer?.let { GuestFiles(it, appVersion) }
    }

    /**
     * Hands Android's clipboard to the agent for a guest paste: its text and
     * MIME types, the data only when a paste asks ([Transfers]). Called when
     * the app gains focus, the only time Android lets it read the clipboard.
     */
    fun syncClipboard(clip: ClipData) {
        // A guest's own copy, handed straight back.
        if (clip.getItemAt(0).uri?.authority == Transfers.clipAuthority(app)) return
        transfers.clip = clip
        val desc = clip.description
        val types = (0 until desc.mimeTypeCount).map { desc.getMimeType(it) }.distinct()
        // An image's "text" is its URI.
        val text = if (types.any { it.startsWith("text/") }) clip.getItemAt(0).coerceToText(app)?.toString().orEmpty() else ""
        if (text == bridge.lastGuestCopy || text to types == lastClipboardSent) return
        if (control.link.value !is ControlChannel.Link.Up) return
        scope.launch {
            val req = JSONObject().put("type", "set-clipboard").put("text", text).put("types", JSONArray(types))
            if (runCatching { control.call(req) }.isSuccess) lastClipboardSent = text to types
        }
    }

    /** An agent on this connection answered notify-action with "unknown request": an older one. */
    @Volatile private var notifyAnswersRefused = false

    /**
     * Tells the guest what became of its notification [id]: the user chose
     * action [key], or (null) dismissed it. Dropped while the agent can't be
     * reached.
     */
    fun answerNotification(id: Long, key: String?) {
        if (control.link.value !is ControlChannel.Link.Up || notifyAnswersRefused) return
        val req = if (key != null) {
            JSONObject().put("type", "notify-action").put("notification", id).put("key", key)
        } else {
            JSONObject().put("type", "notify-closed").put("notification", id).put("reason", CLOSED_DISMISSED)
        }
        scope.launch {
            runCatching { control.call(req) }.onFailure {
                if (it is AgentException && it.message.orEmpty().startsWith("unknown request")) notifyAnswersRefused = true
                Log.w(TAG, "${req.optString("type")}: ${it.message}")
            }
        }
    }

    /** Shares text as the share sheet does; returns where it landed, or throws. */
    suspend fun shareText(text: String): String = control.call(JSONObject().put("type", "share-text").put("text", text)).optString("path")

    /** The user's shell history for the command palette, newest first; empty while the agent can't be reached. */
    suspend fun history(): List<String> = runCatching {
        val h = control.call(JSONObject().put("type", "history").put("limit", HISTORY_LIMIT))
        val a = h.optJSONArray("entries") ?: return@runCatching emptyList()
        (0 until a.length()).map { a.getString(it) }
    }.onFailure { Log.w(TAG, "history: ${it.message}") }.getOrDefault(emptyList())

    /** Graceful: the agent powers the guest off; a hard stop follows if it can't or doesn't in time. */
    fun stop() {
        if (_stopping.value) return
        _stopping.value = true
        scope.launch {
            try {
                val e = _engine.value
                val asked = e.ownsGuest && control.link.value is ControlChannel.Link.Up && control.request("poweroff")
                if (asked) {
                    val down = withTimeoutOrNull(GRACEFUL_STOP_MS) { state.first { it != VmState.Running && it != VmState.Starting } }
                    if (down == null) {
                        Log.w(TAG, "guest still running ${GRACEFUL_STOP_MS / 1000}s after poweroff; stopping it hard")
                        e.forceStop()
                    }
                } else {
                    e.forceStop()
                }
            } finally {
                _stopping.value = false
            }
        }
    }

    /** A new terminal tab running [profile], or null while the agent can't be reached. */
    fun openTab(profile: String? = null): Tab? {
        val dialer = _engine.value.dialer ?: return null
        val cfg = control.config.value
        val name = profile ?: cfg?.defaultProfile?.takeIf { it.isNotEmpty() } ?: "shell"
        val backend = PtyBackend(dialer, name, appVersion)
        return tabs.open(name, backend) { backend.agentTab }
    }

    val config: StateFlow<GuestConfig?> get() = control.config

    /** Debug builds, over adb: opens a tab once the agent is up and types [input] into it; returns its id. */
    suspend fun debugOpenTab(profile: String?, input: String, enter: Boolean): Int = withContext(Dispatchers.Main) {
        control.link.first { it is ControlChannel.Link.Up }
        val tab = openTab(profile?.takeIf { it.isNotEmpty() }) ?: error("the VM stopped")
        debugType(tab.id, input, enter)
        tab.id
    }

    /** Debug builds, over adb, on the main thread: types [input] into tab [id], then Enter if [enter]. */
    fun debugType(id: Int, input: String, enter: Boolean) {
        val tab = tabs.all().firstOrNull { it.id == id } ?: error("no tab $id")
        val text = if (enter) "$input\r" else input
        if (text.isNotEmpty()) tab.session.write(text)
    }

    /**
     * Debug builds, over adb, on the main thread: the VM's state, the last
     * [lines] of every tab's transcript (only tab [only]'s if given) and,
     * for all tabs, the GUI windows.
     */
    fun debugDump(only: Int?, lines: Int): List<String> = buildList {
        add("state=${state.value} link=${control.link.value} host=${control.config.value?.hostname}")
        val shown = tabs.all().filter { only == null || it.id == only }
        if (shown.isEmpty()) error("no tab $only")
        for (tab in shown) {
            add("tab ${tab.id} ${tab.kind} '${tab.title}' running=${tab.session.isRunning}")
            val text = tab.session.emulator?.screen?.transcriptText ?: "(no emulator yet)"
            for (line in text.lines().takeLast(lines)) add("|${tab.id}| $line")
        }
        if (only == null) for (line in gui.dump()) add("gui $line")
    }

    /** Debug builds, over adb: the GUI link's [GuiSession.stats] as JSON. */
    fun debugStats(): String = gui.stats().toString()

    private fun applyColors(colors: Map<String, String>) {
        TerminalTheme.apply(colors)
        for (tab in tabs.all()) tab.session.emulator?.mColors?.reset()
        _themeRevision.value++
    }

    private suspend fun loadFont(path: String?, sha: String?) {
        if (path == null || sha == null) {
            _typeface.value = null
            return
        }
        val dialer = _engine.value.dialer ?: return
        val file = withContext(Dispatchers.IO) {
            runCatching { fonts.get(dialer, sha) }.onFailure { Log.w(TAG, "font: ${it.message}") }.getOrNull()
        } ?: return
        _typeface.value = runCatching { Typeface.createFromFile(file) }.onFailure { Log.w(TAG, "font: ${it.message}") }.getOrNull()
    }

    private companion object {
        const val TAG = "Vm"
        const val GRACEFUL_STOP_MS = 30_000L
        const val AGENT_LATE_MS = 120_000L
        const val FORWARD_RETRY_MS = 30_000L
        const val FORWARD_RETRY_MAX_MS = 600_000L
        const val HISTORY_LIMIT = 1000

        // org.freedesktop.Notifications' "dismissed by the user".
        const val CLOSED_DISMISSED = 2
    }
}
