package dev.nickolaj.pocketnix.hostfs

import dev.nickolaj.pocketnix.agent.Frames
import dev.nickolaj.pocketnix.agent.SocketStream
import java.io.File
import java.net.ServerSocket
import java.net.Socket
import java.nio.ByteBuffer
import java.util.concurrent.Executors
import org.json.JSONObject
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

/** The app's side of hostfs streams over a plain directory, driven the way the agent drives it. */
class HostFsServerTest {
    @get:Rule val tmp = TemporaryFolder()
    private val workers = Executors.newFixedThreadPool(4)
    private val sockets = mutableListOf<Socket>()

    @After
    fun tearDown() {
        sockets.forEach { runCatching { it.close() } }
        workers.shutdownNow()
    }

    private class Reply(val id: Int, val header: JSONObject, val data: ByteArray) {
        val errno: String get() = header.optString("errno")
    }

    /** The agent's end of one stream. */
    private inner class Agent(private val sock: Socket) {
        private var nextId = 0

        fun send(req: JSONObject, data: ByteArray = ByteArray(0)): Int {
            val id = ++nextId
            Frames.write(sock.getOutputStream(), HostFsServer.REQUEST, HostFsServer.encode(id, req, data))
            return id
        }

        fun reply(): Reply {
            val f = Frames.read(sock.getInputStream())!!
            assertEquals(HostFsServer.REPLY, f.type)
            val b = ByteBuffer.wrap(f.payload)
            val id = b.int
            val n = b.int
            return Reply(id, JSONObject(String(f.payload, 8, n)), f.payload.copyOfRange(8 + n, f.payload.size))
        }

        fun call(op: String, data: ByteArray = ByteArray(0), fields: JSONObject.() -> Unit = {}): Reply {
            val id = send(JSONObject().put("op", op).apply(fields), data)
            return reply().also { assertEquals(id, it.id) }
        }
    }

    /** Opens a stream to [server], served on its own thread like the app's. */
    private fun connect(server: HostFsServer): Agent {
        ServerSocket(0).use { l ->
            val app = Socket("127.0.0.1", l.localPort)
            val agent = l.accept()
            sockets += app
            sockets += agent
            Thread { runCatching { server.serve(SocketStream(app)) } }.apply { isDaemon = true }.start()
            return Agent(agent)
        }
    }

    private fun folder(writable: Boolean = true): Pair<File, HostFsServer> {
        val dir = tmp.newFolder()
        return dir to HostFsServer(DirectoryFolder(dir), writable, workers)
    }

    @Test
    fun filesRoundTrip() {
        val (dir, server) = folder()
        val a = connect(server)
        assertEquals(
            "",
            a.call("create") {
                put("name", "docs")
                put("dir", true)
            }.errno
        )
        val created = a.call("create") {
            put("path", "docs")
            put("name", "a.txt")
        }
        assertEquals("a.txt", created.header.getJSONObject("file").getString("name"))

        val h = a.call("open") {
            put("path", "docs/a.txt")
            put("mode", "rw")
        }.header.getLong("handle")
        assertEquals(11, a.call("write", "hello world".toByteArray()) { put("handle", h) }.header.getInt("size"))
        assertEquals(
            "world",
            String(
                a.call("read") {
                    put("handle", h)
                    put("offset", 6)
                    put("size", 100)
                }.data
            )
        )
        assertEquals(
            "",
            a.call("truncate") {
                put("handle", h)
                put("size", 5)
            }.errno
        )
        assertEquals("", a.call("sync") { put("handle", h) }.errno)
        assertEquals("", a.call("close") { put("handle", h) }.errno)
        assertEquals("hello", File(dir, "docs/a.txt").readText())
        assertEquals(
            "EBADF",
            a.call("read") {
                put("handle", h)
                put("size", 1)
            }.errno
        )

        assertEquals(
            "",
            a.call("truncate") {
                put("path", "docs/a.txt")
                put("size", 2)
            }.errno
        )
        val listed = a.call("list") { put("path", "docs") }.header.getJSONArray("files")
        assertEquals(1, listed.length())
        assertEquals(2, listed.getJSONObject(0).getLong("size"))

        assertEquals(
            "",
            a.call("rename") {
                put("path", "docs/a.txt")
                put("to", "b.txt")
            }.errno
        )
        assertTrue(File(dir, "b.txt").isFile)
        assertEquals("", a.call("delete") { put("path", "docs") }.errno)
        assertFalse(File(dir, "docs").exists())
        assertTrue(a.call("stat").header.getJSONObject("file").getBoolean("dir"))
    }

    @Test
    fun failuresCarryTheirErrno() {
        val (dir, server) = folder()
        File(dir, "a").writeText("x")
        val a = connect(server)
        assertEquals("ENOENT", a.call("stat") { put("path", "missing") }.errno)
        assertEquals("EEXIST", a.call("create") { put("name", "a") }.errno)
        assertEquals("EINVAL", a.call("stat") { put("path", "../etc") }.errno)
        assertEquals("EINVAL", a.call("create") { put("name", "x/y") }.errno)
        assertEquals("EBUSY", a.call("delete") { put("path", "") }.errno)
        assertEquals("ENOSYS", a.call("link") { put("path", "a") }.errno)
        assertTrue(File(dir, "a").exists())
    }

    @Test
    fun aReadOnlyFolderOnlyReads() {
        val (dir, server) = folder(writable = false)
        File(dir, "a").writeText("keep")
        val a = connect(server)
        assertEquals(
            "EROFS",
            a.call("open") {
                put("path", "a")
                put("mode", "rw")
            }.errno
        )
        assertEquals("EROFS", a.call("delete") { put("path", "a") }.errno)
        assertEquals(
            "EROFS",
            a.call("truncate") {
                put("path", "a")
                put("size", 0)
            }.errno
        )
        val h = a.call("open") {
            put("path", "a")
            put("mode", "r")
        }.header.getLong("handle")
        assertEquals(
            "keep",
            String(
                a.call("read") {
                    put("handle", h)
                    put("size", 10)
                }.data
            )
        )
        assertEquals("EROFS", a.call("write", "x".toByteArray()) { put("handle", h) }.errno)
        assertEquals("keep", File(dir, "a").readText())
    }

    @Test
    fun streamsShareOpenFilesAndAnswerByID() {
        val (dir, server) = folder()
        val body = ByteArray(3 * HostFsServer.MAX_DATA) { (it % 251).toByte() }
        File(dir, "big").writeBytes(body)
        val one = connect(server)
        val two = connect(server)
        val h = one.call("open") {
            put("path", "big")
            put("mode", "r")
        }.header.getLong("handle")
        // Several at once on the other stream; the answers may come in any order.
        val ids = (0 until 4).associateBy { i ->
            two.send(JSONObject().put("op", "read").put("handle", h).put("offset", i.toLong() * HostFsServer.MAX_DATA).put("size", HostFsServer.MAX_DATA * 2))
        }
        val got = arrayOfNulls<ByteArray>(4)
        repeat(4) {
            val r = two.reply()
            got[ids.getValue(r.id)] = r.data
        }
        assertArrayEquals(body, got.filterNotNull().reduce(ByteArray::plus))
        assertEquals(0, got[3]!!.size)
    }
}
