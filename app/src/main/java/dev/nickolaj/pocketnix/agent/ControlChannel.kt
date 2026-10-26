package dev.nickolaj.pocketnix.agent

import android.util.Log
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withTimeoutOrNull
import org.json.JSONObject
import java.io.IOException

/**
 * The control stream, kept connected while started: the agent isn't up until
 * the guest has booted, and comes back after a restart. [config] keeps the
 * last config across a reconnect.
 */
class ControlChannel(private val scope: CoroutineScope, private val appVersion: String) {
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

    private var job: Job? = null
    @Volatile private var stream: AgentStream? = null
    private val requests = Mutex()
    @Volatile private var pending: CompletableDeferred<Result<JSONObject>>? = null

    fun start(dialer: AgentDialer) {
        stop()
        job = scope.launch(Dispatchers.IO) {
            var backoff = 250L
            while (isActive) {
                try {
                    val (s, hello) = dialer.open(Protocol.hello("control", appVersion))
                    stream = s
                    _link.value = Link.Up(hello.optString("agent"))
                    Log.i(TAG, "control stream up (agent ${hello.optString("agent")})")
                    backoff = 250L
                    readLoop(s)
                } catch (e: AgentException) {
                    Log.w(TAG, "agent refused the control stream: ${e.message}")
                    _link.value = Link.Refused(e.message ?: "refused")
                    return@launch
                } catch (e: IOException) {
                    // Expected while the guest boots; the agent listens late.
                } catch (e: Exception) {
                    Log.w(TAG, "control stream failed", e)
                } finally {
                    stream?.let { runCatching { it.close() } }
                    stream = null
                    pending?.complete(Result.failure(IOException("control stream closed")))
                    if (_link.value is Link.Up) _link.value = Link.Down
                }
                delay(backoff)
                backoff = (backoff * 2).coerceAtMost(2_000L)
            }
        }
    }

    private fun readLoop(s: AgentStream) {
        while (true) {
            val msg = JSONObject(Protocol.readLine(s.input))
            when (msg.optString("type")) {
                "config" -> _config.value = GuestConfig.parse(msg)
                // Pushed, and the answer to [refreshUpdates]; never a pending request's reply.
                "updates" -> _updates.value = Updates.parse(msg)
                "error" -> pending?.complete(Result.failure(AgentException(msg.optString("message"))))
                else -> pending?.complete(Result.success(msg))
            }
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
        val answer = CompletableDeferred<Result<JSONObject>>()
        pending = answer
        try {
            scope.launch(Dispatchers.IO) {
                runCatching { synchronized(s) { Protocol.writeJson(s.output, req) } }
                    .onFailure { answer.complete(Result.failure(it)) }
            }
            val reply = withTimeoutOrNull(timeoutMs) { answer.await() }?.getOrThrow()
                ?: throw IOException("$type: no answer in ${timeoutMs / 1000}s")
            val want = if (type == "ping") "pong" else type
            if (reply.optString("type") != want) throw IOException("$type: the agent answered ${reply.optString("type")}")
            reply
        } finally {
            pending = null
        }
    }

    /** [call] for a request with no fields; false when it failed. */
    suspend fun request(type: String, timeoutMs: Long = 5_000): Boolean =
        runCatching { call(JSONObject().put("type", type), timeoutMs) }
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
        _link.value = Link.Down
    }

    /** Forgets the last config, for a VM that stopped. */
    fun clear() {
        _config.value = null
        _updates.value = null
    }

    private companion object {
        const val TAG = "ControlChannel"
    }
}
