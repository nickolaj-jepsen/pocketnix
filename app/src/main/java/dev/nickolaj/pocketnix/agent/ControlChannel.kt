package dev.nickolaj.pocketnix.agent

import android.util.Log
import java.io.IOException
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.channels.BufferOverflow
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withTimeoutOrNull
import org.json.JSONObject

/**
 * The control stream, kept connected while started: the agent isn't up until
 * the guest has booted, and comes back after a restart. [config] keeps the
 * last config across a reconnect.
 */
class ControlChannel(
    private val scope: CoroutineScope,
    private val appVersion: String,
    /** How long the agent may be quiet before a ping; three times this without a word drops the stream. */
    private val keepaliveMs: Long = 15_000
) {
    sealed interface Link {
        data object Down : Link
        data class Up(val agentVersion: String) : Link

        /** The agent refused the Hello (e.g. a protocol mismatch); retrying won't help. */
        data class Refused(val reason: String) : Link
    }

    private val _link = MutableStateFlow<Link>(Link.Down)
    val link: StateFlow<Link> = _link.asStateFlow()

    private val _config = MutableStateFlow<GuestConfig?>(null)
    val config: StateFlow<GuestConfig?> = _config.asStateFlow()

    private val _updates = MutableStateFlow<Updates?>(null)

    /** The agent's update state; pushed on connect and on every change. */
    val updates: StateFlow<Updates?> = _updates.asStateFlow()

    private val _wake = MutableStateFlow<Wake?>(null)

    /** The agent's last wake push; it comes on connect, so a stale one only outlives a dropped stream. */
    val wake: StateFlow<Wake?> = _wake.asStateFlow()

    private val _events = MutableSharedFlow<JSONObject>(extraBufferCapacity = 64, onBufferOverflow = BufferOverflow.DROP_OLDEST)

    /** The agent's other pushes (go/internal/protocol/integrations.go). */
    val events: SharedFlow<JSONObject> = _events.asSharedFlow()

    private var job: Job? = null

    @Volatile private var stream: AgentStream? = null
    private val requests = Mutex()

    /**
     * The request waiting for its answer, which has [type]'s type (pong for
     * ping) or is an error, and carries [id] from agents that echo it: a late
     * answer to a request that timed out mustn't settle the next one.
     */
    private class Pending(val type: String, val id: Int, val answer: CompletableDeferred<Result<JSONObject>>)

    @Volatile private var pending: Pending? = null
    private var nextId = 0

    /** Request types this stream's agent answered as unknown (it's older): not sent to it again. */
    private val unsupported: MutableSet<String> = java.util.concurrent.ConcurrentHashMap.newKeySet()

    private fun Pending.answers(msg: JSONObject): Boolean = !msg.has("id") || msg.optInt("id") == id

    fun start(dialer: AgentDialer) {
        stop()
        job = scope.launch(Dispatchers.IO) {
            var backoff = MIN_BACKOFF_MS
            var downSince = System.currentTimeMillis()
            while (isActive) {
                var s: AgentStream? = null
                try {
                    val (opened, hello) = dialer.open(Protocol.hello("control", appVersion))
                    s = opened
                    // stop() ran while this dialed: the stream is no one's.
                    if (!isActive) break
                    stream = s
                    unsupported.clear()
                    _link.value = Link.Up(hello.optString("agent"))
                    Log.i(TAG, "control stream up (agent ${hello.optString("agent")})")
                    backoff = MIN_BACKOFF_MS
                    val keepalive = launch { keepalive(s) }
                    try {
                        readLoop(s)
                    } finally {
                        keepalive.cancel()
                        downSince = System.currentTimeMillis()
                    }
                } catch (e: AgentException) {
                    Log.w(TAG, "agent refused the control stream: ${e.message}")
                    _link.value = Link.Refused(e.message ?: "refused")
                    return@launch
                } catch (e: IOException) {
                    // Expected while the guest boots; the agent listens late.
                } catch (e: Exception) {
                    Log.w(TAG, "control stream failed", e)
                } finally {
                    s?.let { runCatching { it.close() } }
                    if (stream === s) stream = null
                    // A cancelled job's cleanup mustn't touch the link its successor owns.
                    if (isActive) {
                        pending?.answer?.complete(Result.failure(IOException("control stream closed")))
                        if (_link.value is Link.Up) _link.value = Link.Down
                    }
                }
                delay(backoff)
                // An agent that hasn't come in [SLOW_AFTER_MS] isn't booting: stop asking virtmgr so often.
                val cap = if (System.currentTimeMillis() - downSince > SLOW_AFTER_MS) MAX_SLOW_BACKOFF_MS else MAX_BACKOFF_MS
                backoff = (backoff * 2).coerceAtMost(cap)
            }
        }
    }

    @Volatile private var lastHeard = 0L

    /**
     * Pings a quiet agent and drops a stream that stays silent: vsock reads
     * have no timeout, so a hung guest would otherwise look connected forever.
     */
    private suspend fun keepalive(s: AgentStream) {
        lastHeard = System.currentTimeMillis()
        while (true) {
            delay(keepaliveMs)
            val quiet = System.currentTimeMillis() - lastHeard
            if (quiet >= keepaliveMs * KEEPALIVE_MISSES) {
                Log.w(TAG, "agent silent for ${quiet / 1000}s; reconnecting")
                runCatching { s.close() }
                return
            }
            // No id: whichever pong comes back only shows the agent is alive.
            if (quiet >= keepaliveMs) {
                scope.launch(Dispatchers.IO) {
                    runCatching { synchronized(s) { Protocol.writeJson(s.output, JSONObject().put("type", "ping")) } }
                }
            }
        }
    }

    private fun readLoop(s: AgentStream) {
        while (true) {
            val line = Protocol.readLine(s.input)
            lastHeard = System.currentTimeMillis()
            // One message the app can't read mustn't drop the link (and reconnect in a loop).
            try {
                handle(JSONObject(line))
            } catch (e: Exception) {
                Log.w(TAG, "unreadable message from the agent: ${e.message}")
            }
        }
    }

    private fun handle(msg: JSONObject) {
        val type = msg.optString("type")
        val p = pending
        when {
            type == "config" -> _config.value = GuestConfig.parse(msg)

            // Pushed, and the answer to [refreshUpdates]; never a pending request's reply.
            type == "updates" -> _updates.value = Updates.parse(msg)

            type == "wake" -> _wake.value = Wake.parse(msg)

            type == "error" -> p?.takeIf { it.answers(msg) }?.answer?.complete(Result.failure(AgentException(msg.optString("message"))))
                ?: Log.w(TAG, "agent error: ${msg.optString("message")}")

            p != null && type == p.type && p.answers(msg) -> p.answer.complete(Result.success(msg))

            // The late answer to a request that already timed out.
            msg.has("id") -> Log.w(TAG, "late answer: $type")

            // The keepalive's.
            type == "pong" -> {}

            else -> _events.tryEmit(msg)
        }
    }

    /**
     * Sends [req] and returns the agent's answer: a message of the request's
     * own type (pong for ping). Throws [AgentException] when the agent refused,
     * IOException when the stream is down or the answer doesn't come in time.
     */
    suspend fun call(req: JSONObject, timeoutMs: Long = 5_000): JSONObject = requests.withLock {
        val s = stream ?: throw IOException("the agent isn't connected")
        val type = req.getString("type")
        if (type in unsupported) throw AgentException("unknown request \"$type\" (an older agent)")
        val answer = CompletableDeferred<Result<JSONObject>>()
        val id = ++nextId
        req.put("id", id)
        pending = Pending(if (type == "ping") "pong" else type, id, answer)
        try {
            scope.launch(Dispatchers.IO) {
                runCatching { synchronized(s) { Protocol.writeJson(s.output, req) } }
                    .onFailure { answer.complete(Result.failure(it)) }
            }
            val result = withTimeoutOrNull(timeoutMs) { answer.await() } ?: throw IOException("$type: no answer in ${timeoutMs / 1000}s")
            if (result.exceptionOrNull()?.message.orEmpty().startsWith("unknown request")) unsupported += type
            result.getOrThrow()
        } finally {
            pending = null
        }
    }

    /** [call] for a request with no fields; false when it failed. */
    suspend fun request(type: String, timeoutMs: Long = 5_000): Boolean = runCatching { call(JSONObject().put("type", type), timeoutMs) }
        .onFailure { Log.w(TAG, "$type: ${it.message}") }
        .isSuccess

    /** Asks for a fresh update state (the generations may have changed under it); it arrives in [updates]. */
    fun refreshUpdates() {
        val s = stream ?: return
        scope.launch(Dispatchers.IO) {
            runCatching { synchronized(s) { Protocol.writeJson(s.output, JSONObject().put("type", "updates")) } }
        }
    }

    fun stop() {
        job?.cancel()
        job = null
        stream?.let { runCatching { it.close() } }
        stream = null
        pending?.answer?.complete(Result.failure(IOException("control stream closed")))
        _link.value = Link.Down
    }

    /** Forgets the last config, for a VM that stopped. */
    fun clear() {
        _config.value = null
        _updates.value = null
        _wake.value = null
    }

    private companion object {
        const val TAG = "ControlChannel"
        const val MIN_BACKOFF_MS = 250L
        const val MAX_BACKOFF_MS = 2_000L
        const val MAX_SLOW_BACKOFF_MS = 30_000L
        const val SLOW_AFTER_MS = 120_000L
        const val KEEPALIVE_MISSES = 3
    }
}
