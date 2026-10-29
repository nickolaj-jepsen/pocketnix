package dev.nickolaj.pocketnix.agent

import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.launch
import org.json.JSONObject
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Test

class HostStateSyncTest {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val agents = mutableListOf<FakeAgent>()

    @After
    fun tearDown() {
        scope.cancel()
        agents.forEach { it.close() }
    }

    private val home = HostState("Europe/Copenhagen", online = true, metered = false, charging = true, batteryLow = false, powerSave = false)

    /** An agent that answers host-state (or refuses it, as an older one does) and hands each request's host to [got]. */
    private fun agent(refuse: Boolean, got: LinkedBlockingQueue<JSONObject>) = FakeAgent { c ->
        while (true) {
            val req = c.readJson() ?: break
            if (req.optString("type") != "host-state") continue
            got.put(req.getJSONObject("host"))
            val answer = if (refuse) JSONObject().put("type", "error").put("message", "unknown request \"host-state\"") else JSONObject().put("type", "host-state")
            c.send(answer.put("id", req.getInt("id")))
        }
    }.also { agents += it }

    private fun sync(a: FakeAgent, state: MutableStateFlow<HostState?>) {
        val ch = ControlChannel(scope, "test")
        ch.start(a.dialer)
        scope.launch { HostStateSync(ch, state, debounceMs = 50).run() }
    }

    @Test
    fun sendsOnConnectAndOnChange() {
        val got = LinkedBlockingQueue<JSONObject>()
        val state = MutableStateFlow<HostState?>(home)
        sync(agent(refuse = false, got), state)
        val first = got.poll(5, TimeUnit.SECONDS)!!
        assertEquals("Europe/Copenhagen", first.getString("timeZone"))
        assertEquals(true, first.getBoolean("charging"))

        // A burst settles into one request with its last state.
        state.value = home.copy(online = false)
        state.value = home.copy(online = true, metered = true)
        val second = got.poll(5, TimeUnit.SECONDS)!!
        assertEquals(true, second.getBoolean("metered"))
        assertEquals(null, got.poll(300, TimeUnit.MILLISECONDS))
    }

    @Test
    fun anAgentThatRefusesIsNotAskedAgain() {
        val got = LinkedBlockingQueue<JSONObject>()
        val state = MutableStateFlow<HostState?>(home)
        sync(agent(refuse = true, got), state)
        got.poll(5, TimeUnit.SECONDS)!!
        state.value = home.copy(powerSave = true)
        assertEquals(null, got.poll(500, TimeUnit.MILLISECONDS))
    }
}
