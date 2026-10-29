package dev.nickolaj.pocketnix.hostfs

import android.util.Log
import dev.nickolaj.pocketnix.agent.AgentDialer
import dev.nickolaj.pocketnix.agent.AgentException
import dev.nickolaj.pocketnix.agent.AgentStream
import dev.nickolaj.pocketnix.agent.ControlChannel
import dev.nickolaj.pocketnix.agent.Protocol
import dev.nickolaj.pocketnix.agent.open
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.awaitCancellation
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject
import java.io.IOException
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicInteger

/**
 * Shares folders with the agent: announces them with a host-folders request
 * on every control connection and whenever they change, then keeps [STREAMS]
 * hostfs streams open per folder, on which the agent asks for its files. An
 * agent that refuses the request (an older one) isn't asked again on that
 * connection.
 */
class HostFolderLinks(
    private val control: ControlChannel,
    private val appVersion: String,
    private val open: (SharedFolder) -> HostFolderFs,
) {
    private val counter = AtomicInteger()
    // Shared by every folder's streams; a handful of SAF calls at a time is plenty.
    private val workers = Executors.newFixedThreadPool(WORKERS) { r ->
        Thread(r, "hostfs-${counter.incrementAndGet()}").apply { isDaemon = true }
    }

    // A folder's server outlives a change to the others, so its open files survive.
    private val servers = mutableMapOf<SharedFolder, HostFsServer>()
    private var refusedBy: ControlChannel.Link? = null

    private val _error = MutableStateFlow<String?>(null)
    /** Why the agent couldn't mount some folders, from the last announcement. */
    val error: StateFlow<String?> = _error.asStateFlow()

    /**
     * Shares [folders] over [link], the control link that is up now, until
     * cancelled: call it again for a new link or a new list.
     */
    suspend fun share(link: ControlChannel.Link.Up, dialer: AgentDialer, folders: List<SharedFolder>) = coroutineScope {
        val current = synchronized(servers) {
            for (gone in servers.keys - folders.toSet()) servers.remove(gone)?.close()
            folders.associateWith { f ->
                servers.getOrPut(f) { HostFsServer(runCatching { open(f) }.getOrElse { Unavailable(it) }, f.writable, workers) }
            }
        }
        if (refusedBy === link) return@coroutineScope
        val req = JSONObject().put("type", "host-folders").put(
            "folders",
            JSONArray(folders.map { JSONObject().put("name", it.name).put("writable", it.writable) }),
        )
        try {
            control.call(req, ANNOUNCE_TIMEOUT_MS)
            _error.value = null
        } catch (e: AgentException) {
            if (e.message.orEmpty().startsWith("unknown request")) {
                refusedBy = link
                _error.value = "The guest's agent can't mount folders; update the guest."
                return@coroutineScope
            }
            // Some folders failed; the others are mounted.
            _error.value = e.message
        } catch (e: IOException) {
            Log.w(TAG, "host-folders: ${e.message}")
            return@coroutineScope
        }
        for ((folder, server) in current) repeat(STREAMS) {
            launch(Dispatchers.IO) { keep(dialer, folder, server) }
        }
        awaitCancellation()
    }

    /** Keeps one stream of [folder] open, dialing again after it ends. */
    private suspend fun keep(dialer: AgentDialer, folder: SharedFolder, server: HostFsServer) {
        var backoff = MIN_BACKOFF_MS
        while (currentCoroutineContext().isActive) {
            try {
                val (s, _) = dialer.open(Protocol.hello("hostfs", appVersion) { put("name", folder.name) })
                backoff = MIN_BACKOFF_MS
                serve(s, server)
            } catch (e: AgentException) {
                // Not mounted (the announcement said why); maybe after the next one.
                Log.w(TAG, "hostfs ${folder.name}: ${e.message}")
                backoff = MAX_BACKOFF_MS
            } catch (e: Exception) {
                Log.w(TAG, "hostfs ${folder.name}: ${e.message}")
            }
            delay(backoff)
            backoff = (backoff * 2).coerceAtMost(MAX_BACKOFF_MS)
        }
    }

    /** Serves [s] until it ends or the caller is cancelled, which closes it. */
    private suspend fun serve(s: AgentStream, server: HostFsServer) = coroutineScope {
        val closer = launch {
            try {
                awaitCancellation()
            } finally {
                runCatching { s.close() }
            }
        }
        try {
            withContext(Dispatchers.IO) { server.serve(s) }
        } finally {
            runCatching { s.close() }
            closer.cancel()
        }
    }

    /** Stands in for a folder whose tree can't be opened (its permission is gone): every operation fails. */
    private class Unavailable(private val cause: Throwable) : HostFolderFs {
        private fun fail(): Nothing = throw HostFsException("EACCES", "the folder can't be opened: ${cause.message}")
        override fun stat(path: String) = fail()
        override fun list(path: String) = fail()
        override fun create(dir: String, name: String, isDir: Boolean) = fail()
        override fun delete(path: String) = fail()
        override fun rename(path: String, to: String) = fail()
        override fun open(path: String, mode: String) = fail()
    }

    private companion object {
        const val TAG = "HostFolderLinks"
        const val STREAMS = 2
        const val WORKERS = 4
        // Mounting is quick, but the agent may first wait out a stale mount.
        const val ANNOUNCE_TIMEOUT_MS = 15_000L
        const val MIN_BACKOFF_MS = 250L
        const val MAX_BACKOFF_MS = 30_000L
    }
}
