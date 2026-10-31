package dev.nickolaj.pocketnix.agent

import dev.nickolaj.pocketnix.engine.VmState
import java.io.IOException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.json.JSONObject
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ControlChannelTest {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val agents = mutableListOf<FakeAgent>()

    @After
    fun tearDown() {
        scope.cancel()
        agents.forEach { it.close() }
    }

    private fun agent(serve: (FakeConn) -> Unit) = FakeAgent(serve).also { agents += it }

    private fun ControlChannel.awaitUp() = runBlocking { withTimeout(5_000) { link.first { it is ControlChannel.Link.Up } } }

    @Test
    fun aLateAnswerDoesNotSettleTheNextRequest() {
        val a = agent { c ->
            var unanswered: Int? = null
            while (true) {
                val req = c.readJson() ?: break
                if (req.optString("type") != "history") continue
                val first = unanswered
                if (first == null) {
                    unanswered = req.getInt("id")
                    continue
                }
                c.send(JSONObject().put("type", "history").put("id", first).put("entries", listOf("late")))
                c.send(JSONObject().put("type", "history").put("id", req.getInt("id")).put("entries", listOf("right")))
            }
        }
        val ch = ControlChannel(scope, "test")
        ch.start(a.dialer)
        ch.awaitUp()
        runBlocking {
            val timedOut = runCatching { ch.call(JSONObject().put("type", "history"), timeoutMs = 300) }
            assertTrue(timedOut.exceptionOrNull() is IOException)
            val answer = ch.call(JSONObject().put("type", "history"))
            assertEquals("right", answer.getJSONArray("entries").getString(0))
        }
    }

    @Test
    fun aSilentAgentIsDroppedAndRedialed() {
        // Reads pings and never answers, like a guest that hung.
        val a = agent { c -> while (c.readJson() != null) Unit }
        val ch = ControlChannel(scope, "test", keepaliveMs = 100)
        ch.start(a.dialer)
        ch.awaitUp()
        runBlocking {
            withTimeout(5_000) {
                ch.link.first { it == ControlChannel.Link.Down }
                while (a.connections.get() < 2) delay(20)
            }
        }
    }

    @Test
    fun anAgentAnsweringPingsStaysConnected() {
        val a = agent { c ->
            while (true) {
                val req = c.readJson() ?: break
                if (req.optString("type") == "ping") c.send(JSONObject().put("type", "pong"))
            }
        }
        val ch = ControlChannel(scope, "test", keepaliveMs = 100)
        ch.start(a.dialer)
        ch.awaitUp()
        Thread.sleep(1_000)
        assertTrue(ch.link.value is ControlChannel.Link.Up)
        assertEquals(1, a.connections.get())
    }

    @Test
    fun anUnreadablePushKeepsTheLink() {
        val a = agent { c ->
            c.send("not json")
            // A profile without its name: GuestConfig.parse throws.
            c.send("""{"type":"config","terminal":{"profiles":[{}]}}""")
            c.send(JSONObject().put("type", "config").put("hostname", "phone"))
            while (c.readJson() != null) Unit
        }
        val ch = ControlChannel(scope, "test")
        ch.start(a.dialer)
        val cfg = runBlocking { withTimeout(5_000) { ch.config.first { it != null } } }
        assertEquals("phone", cfg!!.hostname)
        assertEquals(1, a.connections.get())
        assertTrue(ch.link.value is ControlChannel.Link.Up)
    }

    @Test
    fun aRebootForgetsTheLastBootsListeners() {
        val a = agent { c ->
            c.send(JSONObject().put("type", "config").put("hostname", "phone"))
            c.send("""{"type":"port-listeners","lan":false,"ports":[8080]}""")
            while (c.readJson() != null) Unit
        }
        val ch = ControlChannel(scope, "test")
        ch.follow(VmState.Running, a.dialer)
        runBlocking { withTimeout(5_000) { ch.portListeners.first { it != null } } }
        ch.follow(VmState.Starting, a.dialer)
        assertNull(ch.portListeners.value)
        assertNull(ch.config.value)
        assertEquals(ControlChannel.Link.Down, ch.link.value)
    }

    @Test
    fun stopEndsTheStreamAndFailsTheWaitingCall() {
        val a = agent { c -> while (c.readJson() != null) Unit }
        val ch = ControlChannel(scope, "test")
        ch.start(a.dialer)
        ch.awaitUp()
        runBlocking {
            val call = scope.async { runCatching { ch.call(JSONObject().put("type", "history"), timeoutMs = 10_000) } }
            delay(100)
            ch.stop()
            val result = withTimeout(2_000) { call.await() }
            assertTrue(result.exceptionOrNull() is IOException)
        }
        assertEquals(ControlChannel.Link.Down, ch.link.value)
    }
}
