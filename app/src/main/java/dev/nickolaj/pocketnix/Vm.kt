package dev.nickolaj.pocketnix

import android.content.Context
import android.content.Intent
import android.graphics.Typeface
import android.util.Log
import dev.nickolaj.pocketnix.agent.ControlChannel
import dev.nickolaj.pocketnix.agent.FontCache
import dev.nickolaj.pocketnix.agent.GuestConfig
import dev.nickolaj.pocketnix.agent.GuestFiles
import dev.nickolaj.pocketnix.agent.PtyBackend
import dev.nickolaj.pocketnix.files.GuestDocumentsProvider
import dev.nickolaj.pocketnix.files.PendingUploads
import dev.nickolaj.pocketnix.agent.Updates
import dev.nickolaj.pocketnix.engine.AvfEngine
import dev.nickolaj.pocketnix.engine.BootMenu
import dev.nickolaj.pocketnix.engine.ConsoleLog
import dev.nickolaj.pocketnix.engine.TcpEngine
import dev.nickolaj.pocketnix.engine.VmEngine
import dev.nickolaj.pocketnix.engine.VmState
import dev.nickolaj.pocketnix.forward.PortForwards
import dev.nickolaj.pocketnix.service.VmService
import dev.nickolaj.pocketnix.terminal.TabAlerts
import dev.nickolaj.pocketnix.terminal.Tabs
import dev.nickolaj.pocketnix.terminal.TerminalTheme
import kotlinx.coroutines.CoroutineExceptionHandler
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.delay
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
import org.json.JSONObject
import java.io.File

/**
 * The app's one VM and everything that lives with it: the engine running
 * it, the agent's control channel, port forwards, the module's theme, and
 * the terminal tabs. Owned by [PocketnixApp]; the foreground service keeps
 * the process (and so this) alive while the VM runs.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class Vm(private val app: Context) {
    // A failed coroutine logs instead of crashing the app, and with it the VM.
    private val scope = CoroutineScope(
        SupervisorJob() + Dispatchers.Main.immediate + CoroutineExceptionHandler { _, e -> Log.e(TAG, "coroutine failed", e) },
    )
    val settings = Settings(app)
    val console = ConsoleLog()
    private val appVersion = "pocketnix ${BuildConfig.VERSION_NAME}"
    val control = ControlChannel(scope, appVersion)
    private val forwards = PortForwards(appVersion)
    private val fonts = FontCache(File(app.cacheDir, "fonts"), appVersion)
    val tabs = Tabs(app, console.backend(), appVersion)

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

    val updates: StateFlow<Updates?> get() = control.updates

    private val bridge = GuestBridge(app) { control.config.value?.hostname }
    private var lastClipboardSent: String? = null

    init {
        tabs.alerts = TabAlerts(app) { control.config.value?.hostname }
        scope.launch {
            control.events.collect { ev ->
                runCatching { bridge.onEvent(ev, tabs.visible) }.onFailure { Log.e(TAG, "agent push ${ev.optString("type")}", it) }
            }
        }
        // The Files app lists the root either way, but says when it can't be opened.
        scope.launch {
            control.link.map { it is ControlChannel.Link.Up }.distinctUntilChanged().collect { up ->
                GuestDocumentsProvider.rootsChanged(app)
                // Saves that missed the guest go in now.
                val f = if (up) files() else null
                if (f != null) withContext(Dispatchers.IO) { PendingUploads.retry(app, f) }
            }
        }
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
            combine(control.config, state) { cfg, s -> cfg?.forwards.orEmpty() to (s == VmState.Running) }
                .distinctUntilChanged()
                .collectLatest { (fw, running) ->
                    val dialer = if (running) _engine.value.dialer else null
                    _forwardErrors.value = withContext(Dispatchers.IO) { forwards.apply(fw, dialer) }
                    // A port another app holds may come free (that app stopped): keep trying.
                    while (_forwardErrors.value.isNotEmpty() && dialer != null) {
                        delay(FORWARD_RETRY_MS)
                        _forwardErrors.value = withContext(Dispatchers.IO) { forwards.apply(fw, dialer) }
                    }
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

    private fun pickEngine(): VmEngine =
        if (settings.tcpBackend) {
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
     * Runs an update request (update-check, update-apply, or
     * switch-generation with [generation]); progress arrives in [updates].
     * Returns why the agent refused, or null.
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
     * Hands Android's clipboard to the agent for a guest paste; called when
     * the app gains focus, the only time Android lets it read the clipboard.
     */
    fun syncClipboard(text: String?) {
        if (text == null || text == lastClipboardSent || text == bridge.lastGuestCopy) return
        if (control.link.value !is ControlChannel.Link.Up) return
        scope.launch {
            if (runCatching { control.call(JSONObject().put("type", "set-clipboard").put("text", text)) }.isSuccess) lastClipboardSent = text
        }
    }

    /** Shares text as the share sheet does; returns where it landed, or throws. */
    suspend fun shareText(text: String): String =
        control.call(JSONObject().put("type", "share-text").put("text", text)).optString("path")

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
    fun openTab(profile: String? = null) {
        val dialer = _engine.value.dialer ?: return
        val cfg = control.config.value
        val name = profile ?: cfg?.defaultProfile?.takeIf { it.isNotEmpty() } ?: "shell"
        tabs.open(name, PtyBackend(dialer, name, appVersion))
    }

    val config: StateFlow<GuestConfig?> get() = control.config

    /** Debug builds, over adb: opens a tab once the agent is up, and types [input] into it. */
    fun debugOpenTab(profile: String?, input: String?) {
        scope.launch {
            control.link.first { it is ControlChannel.Link.Up }
            openTab(profile?.takeIf { it.isNotEmpty() })
            if (!input.isNullOrEmpty()) tabs.current().session.write(input.replace("\\r", "\r"))
        }
    }

    /** Debug builds, over adb: logs every tab's transcript, for checks on a locked phone. */
    fun debugDumpTabs() {
        Log.i(TAG, "dump: state=${state.value} link=${control.link.value} host=${control.config.value?.hostname}")
        for (tab in tabs.all()) {
            val text = tab.session.emulator?.screen?.transcriptText ?: "(no emulator yet)"
            Log.i(TAG, "dump: tab ${tab.id} ${tab.kind} '${tab.title}' running=${tab.session.isRunning}")
            for (line in text.lines().takeLast(40)) Log.i(TAG, "dump|${tab.id}| $line")
        }
    }

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
        const val HISTORY_LIMIT = 1000
    }
}
