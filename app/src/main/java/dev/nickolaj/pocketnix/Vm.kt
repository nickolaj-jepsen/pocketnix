package dev.nickolaj.pocketnix

import android.content.Context
import android.content.Intent
import android.graphics.Typeface
import android.util.Log
import dev.nickolaj.pocketnix.agent.ControlChannel
import dev.nickolaj.pocketnix.agent.FontCache
import dev.nickolaj.pocketnix.agent.GuestConfig
import dev.nickolaj.pocketnix.agent.PtyBackend
import dev.nickolaj.pocketnix.engine.AvfEngine
import dev.nickolaj.pocketnix.engine.ConsoleLog
import dev.nickolaj.pocketnix.engine.TcpEngine
import dev.nickolaj.pocketnix.engine.VmEngine
import dev.nickolaj.pocketnix.engine.VmState
import dev.nickolaj.pocketnix.forward.PortForwards
import dev.nickolaj.pocketnix.service.VmService
import dev.nickolaj.pocketnix.terminal.Tabs
import dev.nickolaj.pocketnix.terminal.TerminalTheme
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.flatMapLatest
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull
import java.io.File

/**
 * The app's one VM and everything that lives with it: the engine running
 * it, the agent's control channel, port forwards, the module's theme, and
 * the terminal tabs. Owned by [PocketnixApp]; the foreground service keeps
 * the process (and so this) alive while the VM runs.
 */
@OptIn(ExperimentalCoroutinesApi::class)
class Vm(private val app: Context) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
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

    init {
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
                .collect { (fw, running) ->
                    val dialer = if (running) _engine.value.dialer else null
                    _forwardErrors.value = withContext(Dispatchers.IO) { forwards.apply(fw, dialer) }
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

    fun start() {
        reloadEngine()
        if (state.value == VmState.Running || state.value == VmState.Starting) return
        // Refused when the app isn't in the foreground (e.g. started over adb on a locked
        // phone); the VM still runs, just without the service keeping the process alive.
        runCatching { app.startForegroundService(Intent(app, VmService::class.java)) }
            .onFailure { Log.w(TAG, "foreground service refused: ${it.message}") }
        _engine.value.start(settings.vm(), console)
    }

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
            if (input != null) tabs.current().session.write(input.replace("\\r", "\r"))
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
    }
}
