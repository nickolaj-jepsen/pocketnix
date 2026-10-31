package dev.nickolaj.pocketnix.managed

import android.content.Context
import android.content.Intent
import android.util.Log
import dev.nickolaj.pocketnix.Settings
import dev.nickolaj.pocketnix.agent.ControlChannel
import dev.nickolaj.pocketnix.agent.Plan
import dev.nickolaj.pocketnix.agent.Updates
import dev.nickolaj.pocketnix.engine.DiskImage
import dev.nickolaj.pocketnix.service.SetupService
import java.io.File
import java.io.IOException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.filterNotNull
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.onStart
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject

/**
 * A guest the app manages (pocketnix.guest.managed): set up from the
 * release's base image, brought in line with [store] each time it connects,
 * and rebuilt, given packages and asked to run them from the app.
 */
class ManagedGuest(
    private val app: Context,
    private val scope: CoroutineScope,
    private val control: ControlChannel,
    private val settings: Settings
) {
    val store = ManagedStore(app)
    val index = PackageIndex(File(app.filesDir, "packages"), settings)

    /** Setting up a disk from the base image. */
    sealed interface Setup {
        data object Idle : Setup

        /** Bytes so far and in all (0 if unknown); [unpacking] once downloaded. */
        data class Working(val done: Long, val total: Long, val unpacking: Boolean) : Setup

        data class Failed(val message: String) : Setup
    }

    private val _setup = MutableStateFlow<Setup>(Setup.Idle)
    val setup: StateFlow<Setup> = _setup.asStateFlow()

    private val _syncError = MutableStateFlow<String?>(null)

    /** Why the guest isn't running the app's selections yet; the next connect tries again. */
    val syncError: StateFlow<String?> = _syncError.asStateFlow()

    // One operation at a time from here; the agent refuses a second anyway.
    private val ops = Mutex()

    init {
        scope.launch {
            combine(control.link, control.config) { link, cfg -> link is ControlChannel.Link.Up && cfg?.managedFlake != null }
                .distinctUntilChanged()
                .collectLatest { up -> if (up) runCatching { sync() }.onFailure { Log.w(TAG, "sync: ${it.message}") } }
        }
    }

    /** Runs the sync a connect runs again, after [syncError]. */
    fun retrySync() {
        scope.launch { runCatching { sync() }.onFailure { Log.w(TAG, "sync: ${it.message}") } }
    }

    /**
     * Makes the disk from the base image, keeps [selections] for the guest's
     * first rebuild, and calls [onReady]. Refuses while a disk exists.
     */
    fun install(selections: Selections, onReady: () -> Unit) {
        if (_setup.value is Setup.Working) return
        _setup.value = Setup.Working(0, 0, false)
        runCatching { app.startForegroundService(Intent(app, SetupService::class.java)) }
            .onFailure { Log.w(TAG, "setup service refused: ${it.message}") }
        scope.launch {
            try {
                withContext(Dispatchers.IO) { installImage() }
                store.selections = selections
                store.rebuildPending = true
                _setup.value = Setup.Idle
                onReady()
            } catch (e: Exception) {
                Log.w(TAG, "setup", e)
                _setup.value = Setup.Failed(e.message ?: e.toString())
            }
        }
    }

    private fun installImage() {
        val disk = DiskImage.file(app)
        if (disk.exists()) throw IOException("a disk image exists already")
        val name = Release.baseImage()
        val gz = File(app.filesDir, "$name.part")
        Release.download(Release.url(settings, name), gz) { done, total -> _setup.value = Setup.Working(done, total, false) }
        Release.verify(Release.url(settings, "$name.sha256"), gz)
        val part = File(disk.path + ".part")
        Release.unpack(gz, part) { done, total -> _setup.value = Setup.Working(done, total, true) }
        DiskImage.grow(part, settings.diskGb)
        if (!part.renameTo(disk)) throw IOException("couldn't move the image into place")
        gz.delete()
    }

    /**
     * Brings a guest that just connected in line with [store]: a disk that
     * didn't have these selections, or whose rebuild to them didn't finish,
     * rebuilds (without building on the phone: the agent's blocked plan asks
     * for that), then gets the packages it lacks.
     */
    private suspend fun sync() {
        val sel = store.selections ?: return
        val answer = writeSelections(sel)
        if (answer.optBoolean("changed") || store.rebuildPending) {
            store.rebuildPending = true
            // An agent that just booted may be pruning: wait rather than fail as busy.
            control.updates.filterNotNull().first { !it.busy }
            val err = rebuild(sel, null)
            _syncError.value = err
            if (err != null) {
                Log.w(TAG, "rebuild to the app's selections: $err")
                return
            }
        }
        val have = control.updates.filterNotNull().first().packages.toSet()
        val missing = store.packages - have
        if (missing.isNotEmpty()) runOp(JSONObject().put("type", "package-add").put("packages", JSONArray(missing.sorted())))
    }

    private suspend fun writeSelections(sel: Selections): JSONObject = control.call(JSONObject().put("type", "selections").put("selections", sel.toJson()))

    /**
     * Writes [sel] and rebuilds the guest from it, pinned to this app's
     * release: checks, then asks [confirm] about anything it would compile on
     * the phone, then switches. Returns why it failed, or null.
     */
    suspend fun rebuild(sel: Selections, confirm: (suspend (Plan) -> Boolean)?): String? {
        store.selections = sel
        val err = try {
            writeSelections(sel)
            update(confirm)
        } catch (e: IOException) {
            e.message ?: e.toString()
        }
        if (err == null && control.updates.value?.blocked == null) {
            store.rebuildPending = false
            _syncError.value = null
        }
        return err
    }

    /**
     * Checks for and applies an update, as [rebuild] does without new
     * selections. A null [confirm] applies without building on the phone,
     * so the agent records what it stopped short of. Returns why it failed,
     * or null (also when [confirm] said no).
     */
    suspend fun update(confirm: (suspend (Plan) -> Boolean)?): String? {
        var u = runOp(withPin(JSONObject().put("type", "update-check")))
        u.error?.let { return it }
        if (u.available == null) return null
        val plan = u.plan ?: Plan(0, emptyList())
        val builds = plan.builds.isNotEmpty()
        if (builds && confirm != null && !confirm(plan)) return null
        u = runOp(withPin(JSONObject().put("type", "update-apply").put("allowBuilds", builds && confirm != null)))
        return u.error
    }

    /** Installs a package into the user's profile and keeps it. The answer's blocked plan says what it would build. */
    suspend fun addPackage(attr: String, allowBuilds: Boolean): Updates {
        val u = runOp(JSONObject().put("type", "package-add").put("packages", JSONArray(listOf(attr))).put("allowBuilds", allowBuilds))
        if (attr in u.packages) store.packages = store.packages + attr
        return u
    }

    suspend fun removePackage(attr: String): Updates {
        store.packages = store.packages - attr
        return runOp(JSONObject().put("type", "package-remove").put("packages", JSONArray(listOf(attr))))
    }

    /** Fetches a package to run once; the answer's run says how to start it, or its blocked plan what it would build. */
    suspend fun run(attr: String, allowBuilds: Boolean): Updates = runOp(JSONObject().put("type", "run").put("package", attr).put("allowBuilds", allowBuilds))

    private fun withPin(req: JSONObject): JSONObject = req.apply { Release.pin?.let { put("pin", it) } }

    /**
     * Starts an agent operation and waits for it to end. An agent that
     * refused it, or went away before it ended, answers with its last state
     * and why in [Updates.error].
     */
    private suspend fun runOp(req: JSONObject): Updates = ops.withLock {
        try {
            val before = upState().first().second
            control.call(req)
            // A restarted agent counts its operations from 0 again: a dropped link ends the wait.
            val (up, u) = upState().first { (up, u) -> !up || u.op < before.op || (u.op > before.op && !u.busy) }
            if (!up || u.op < before.op) throw IOException("the agent went away")
            u
        } catch (e: IOException) {
            val why = e.message ?: e.toString()
            control.updates.value?.copy(error = why, blocked = null, run = null) ?: noState(why)
        }
    }

    private fun noState(why: String) = Updates(emptyList(), "idle", 0, "", "", null, null, 0, why, emptyList(), emptyList())

    /** Whether the link is up, with the latest update state; throws while the agent isn't connected. */
    private fun upState() = combine(control.link, control.updates.filterNotNull()) { link, u -> (link is ControlChannel.Link.Up) to u }
        .onStart { if (control.link.value !is ControlChannel.Link.Up) throw IOException("the agent isn't connected") }

    private companion object {
        const val TAG = "ManagedGuest"
    }
}
