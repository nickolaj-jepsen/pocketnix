package dev.nickolaj.pocketnix.hostfs

import dev.nickolaj.pocketnix.agent.ControlChannel
import dev.nickolaj.pocketnix.agent.FakeAgent
import dev.nickolaj.pocketnix.agent.Frames
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.json.JSONObject
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder
import java.io.File
import java.nio.ByteBuffer
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit

/** Announcing shared folders and keeping their streams, against a stand-in agent. */
class HostFolderLinksTest {
    @get:Rule val tmp = TemporaryFolder()
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val agents = mutableListOf<FakeAgent>()

    @After
    fun tearDown() {
        scope.cancel()
        agents.forEach { it.close() }
    }

    private fun up(agent: FakeAgent): Pair<ControlChannel, ControlChannel.Link.Up> {
        val ch = ControlChannel(scope, "test")
        ch.start(agent.dialer)
        val link = runBlocking { withTimeout(5_000) { ch.link.first { it is ControlChannel.Link.Up } } }
        return ch to link as ControlChannel.Link.Up
    }

    @Test
    fun foldersAreAnnouncedAndServedOnTheirStreams() {
        val announced = LinkedBlockingQueue<JSONObject>()
        val stats = LinkedBlockingQueue<Pair<String, JSONObject>>()
        val agent = FakeAgent { c ->
            when (c.hello.optString("stream")) {
                "control" -> while (true) {
                    val req = c.readJson() ?: break
                    if (req.optString("type") != "host-folders") continue
                    announced += req
                    c.send(JSONObject().put("type", "host-folders").put("id", req.getInt("id")))
                }
                "hostfs" -> {
                    Frames.write(c.output, HostFsServer.REQUEST, HostFsServer.encode(1, JSONObject().put("op", "stat").put("path", "a.txt"), null))
                    val f = Frames.read(c.input) ?: return@FakeAgent
                    val n = ByteBuffer.wrap(f.payload, 4, 4).int
                    stats += c.hello.optString("name") to JSONObject(String(f.payload, 8, n))
                }
            }
        }.also { agents += it }
        val dir = tmp.newFolder()
        File(dir, "a.txt").writeText("hello")
        val (ch, link) = up(agent)
        val links = HostFolderLinks(ch, "test") { DirectoryFolder(dir) }
        scope.launch { links.share(link, agent.dialer, listOf(SharedFolder("Pictures", "content://tree", writable = false))) }

        val req = announced.poll(5, TimeUnit.SECONDS)!!
        val folder = req.getJSONArray("folders").getJSONObject(0)
        assertEquals("Pictures", folder.getString("name"))
        assertEquals(false, folder.optBoolean("writable"))
        repeat(2) {
            val (name, reply) = stats.poll(5, TimeUnit.SECONDS)!!
            assertEquals("Pictures", name)
            assertEquals(5, reply.getJSONObject("file").getLong("size"))
        }
    }

    @Test
    fun anAgentWithoutHostFoldersIsAskedOncePerConnection() {
        val requests = CopyOnWriteArrayList<String>()
        val streams = CopyOnWriteArrayList<String>()
        val agent = FakeAgent { c ->
            when (c.hello.optString("stream")) {
                "control" -> while (true) {
                    val req = c.readJson() ?: break
                    requests += req.optString("type")
                    c.send(JSONObject().put("type", "error").put("id", req.getInt("id")).put("message", "unknown request \"${req.optString("type")}\""))
                }
                else -> streams += c.hello.optString("stream")
            }
        }.also { agents += it }
        val (ch, link) = up(agent)
        val links = HostFolderLinks(ch, "test") { DirectoryFolder(tmp.root) }
        runBlocking {
            withTimeout(5_000) {
                links.share(link, agent.dialer, listOf(SharedFolder("a", "content://a", writable = true)))
                links.share(link, agent.dialer, listOf(SharedFolder("b", "content://b", writable = true)))
            }
        }
        assertEquals(listOf("host-folders"), requests.filter { it == "host-folders" })
        assertTrue("dialed $streams", streams.isEmpty())
        assertTrue(links.error.value!!.isNotEmpty())
    }
}
