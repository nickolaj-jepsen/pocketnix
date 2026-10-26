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

    private var job: Job? = null
    @Volatile private var stream: AgentStream? = null
    private val requests = Mutex()
    @Volatile private var pending: CompletableDeferred<Result<String>>? = null

    fun start(dialer: AgentDialer) {
        stop()
        job = scope.launch(Dispatchers.IO) {
            var backoff = 250L
            while (isActive) {
                try {
                    val (s, hello) = dialer.open(Protocol.hello("control", appVersion))
                    stream = s
                    _link.value = Link.Up(hello.optString("agent"))
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
            when (val type = msg.optString("type")) {
                "config" -> _config.value = GuestConfig.parse(msg)
                "error" -> pending?.complete(Result.failure(AgentException(msg.optString("message"))))
                else -> pending?.complete(Result.success(type))
            }
        }
    }

    /**
     * Sends a request (poweroff, reboot, ping) and waits for the agent's
     * answer. Returns false when the stream is down or the agent refused.
     */
    suspend fun request(type: String, timeoutMs: Long = 5_000): Boolean = requests.withLock {
        val s = stream ?: return false
        val answer = CompletableDeferred<Result<String>>()
        pending = answer
        try {
            scope.launch(Dispatchers.IO) {
                runCatching { synchronized(s) { Protocol.writeJson(s.output, JSONObject().put("type", type)) } }
                    .onFailure { answer.complete(Result.failure(it)) }
            }
            val r = withTimeoutOrNull(timeoutMs) { answer.await() } ?: return false
            r.onFailure { Log.w(TAG, "$type: ${it.message}") }
            val want = if (type == "ping") "pong" else type
            r.getOrNull() == want
        } finally {
            pending = null
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
    }

    private companion object {
        const val TAG = "ControlChannel"
    }
}
