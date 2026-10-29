package dev.nickolaj.pocketnix.agent

import android.util.Log
import java.io.IOException
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.filterNotNull
import kotlinx.coroutines.withContext

/**
 * Sends the phone's [state] to the agent: on every connect, then each change
 * once it has held for [debounceMs] (a network handover is a burst). The
 * agent forgets it with the stream. An agent that refuses host-state (an
 * older one) isn't asked again on that connection.
 */
class HostStateSync(
    private val control: ControlChannel,
    private val state: StateFlow<HostState?>,
    private val debounceMs: Long = 2_000
) {
    suspend fun run() {
        control.link.collectLatest { link ->
            if (link !is ControlChannel.Link.Up) return@collectLatest
            var sent: HostState? = null
            var refused = false
            state.filterNotNull().collectLatest { st ->
                if (refused || st == sent) return@collectLatest
                if (sent != null) delay(debounceMs)
                // A newer state waits for this answer rather than cancelling it: a refusal must count.
                withContext(NonCancellable) {
                    try {
                        control.call(st.request())
                        sent = st
                    } catch (e: AgentException) {
                        Log.i(TAG, "the agent takes no host state: ${e.message}")
                        refused = true
                    } catch (e: IOException) {
                        // The stream is down; its reconnect sends again.
                        Log.w(TAG, "host-state: ${e.message}")
                    }
                }
            }
        }
    }

    private companion object {
        const val TAG = "HostStateSync"
    }
}
