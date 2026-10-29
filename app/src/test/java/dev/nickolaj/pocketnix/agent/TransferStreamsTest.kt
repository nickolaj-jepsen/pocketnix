package dev.nickolaj.pocketnix.agent

import dev.nickolaj.pocketnix.files.Transfers
import java.io.ByteArrayOutputStream
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.net.ServerSocket
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.json.JSONObject
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** The app's halves of guest transfers, against stand-in agents. */
class TransferStreamsTest {
    private val closers = mutableListOf<AutoCloseable>()
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    @After
    fun tearDown() {
        scope.cancel()
        closers.forEach { runCatching { it.close() } }
    }

    /** An agent that hands each connection's Hello, input and output to [serve], answering nothing itself. */
    private fun agent(serve: (hello: JSONObject, input: InputStream, output: OutputStream) -> Unit): AgentDialer {
        val server = ServerSocket(0).also { closers += it }
        Thread {
            while (true) {
                val s = runCatching { server.accept() }.getOrNull() ?: break
                Thread { s.use { runCatching { serve(JSONObject(Protocol.readLine(s.getInputStream())), s.getInputStream(), s.getOutputStream()) } } }
                    .apply { isDaemon = true }.start()
            }
        }.apply { isDaemon = true }.start()
        return TcpDialer("127.0.0.1", server.localPort)
    }

    private fun OutputStream.line(o: JSONObject) = Protocol.writeJson(this, o)

    @Test
    fun aFetchStoresTheBytesAndConfirms() {
        val answers = LinkedBlockingQueue<JSONObject>()
        val body = ByteArray(100_000) { (it % 253).toByte() }
        val dialer = agent { hello, input, out ->
            assertEquals("save", hello.getString("stream"))
            out.line(JSONObject().put("type", "hello").put("version", 1).put("name", "a.png").put("size", body.size))
            out.write(body)
            out.flush()
            answers += JSONObject(Protocol.readLine(input)).put("ref", hello.getString("ref"))
        }
        val got = ByteArrayOutputStream()
        GuestFiles(dialer, "test").fetch("r1") { name, size, input ->
            assertEquals("a.png", name)
            assertEquals(body.size.toLong(), size)
            input.copyTo(got)
        }
        assertArrayEquals(body, got.toByteArray())
        val answer = answers.poll(5, TimeUnit.SECONDS)!!
        assertEquals("done", answer.getString("type"))
        assertEquals("r1", answer.getString("ref"))
    }

    @Test
    fun aFailedStoreTellsTheAgent() {
        val answers = LinkedBlockingQueue<JSONObject>()
        val dialer = agent { _, input, out ->
            out.line(JSONObject().put("type", "hello").put("version", 1).put("size", 3))
            out.write("abc".toByteArray())
            out.flush()
            answers += JSONObject(Protocol.readLine(input))
        }
        val failed = runCatching { GuestFiles(dialer, "test").fetch("r") { _, _, _ -> throw IOException("disk full") } }
        assertTrue(failed.isFailure)
        val answer = answers.poll(5, TimeUnit.SECONDS)!!
        assertEquals("error", answer.getString("type"))
        assertEquals("disk full", answer.getString("message"))
    }

    @Test
    fun aShareForARequestNamesIt() {
        val hellos = LinkedBlockingQueue<JSONObject>()
        val dialer = agent { hello, input, out ->
            hellos += hello
            out.line(JSONObject().put("type", "hello").put("version", 1))
            input.readNBytes(hello.getInt("size"))
            out.line(JSONObject().put("type", "done").put("path", "/home/u/Inbox/a.png"))
        }
        val p = GuestFiles(dialer, "test").share("a.png", 4, "data".byteInputStream(), ref = "r2")
        assertEquals("/home/u/Inbox/a.png", p)
        assertEquals("r2", hellos.poll(5, TimeUnit.SECONDS)!!.getString("ref"))
    }

    @Test
    fun aRequestAnOlderAgentRefusedIsNotSentAgain() {
        val asked = AtomicInteger()
        val dialer = agent { _, input, out ->
            out.line(JSONObject().put("type", "hello").put("version", 1))
            while (true) {
                val req = JSONObject(Protocol.readLine(input))
                if (req.getString("type") == "new-thing") asked.incrementAndGet()
                out.line(JSONObject().put("type", "error").put("id", req.getInt("id")).put("message", "unknown request \"${req.getString("type")}\""))
            }
        }
        val ch = ControlChannel(scope, "test")
        ch.start(dialer)
        runBlocking {
            withTimeout(5_000) { ch.link.first { it is ControlChannel.Link.Up } }
            repeat(2) { assertTrue(runCatching { ch.call(JSONObject().put("type", "new-thing")) }.exceptionOrNull() is AgentException) }
        }
        assertEquals(1, asked.get())
    }

    @Test
    fun mimePatternsMatch() {
        assertTrue(Transfers.mimeMatches("image/*", "image/png"))
        assertTrue(Transfers.mimeMatches("text/html", "text/html; charset=utf-8"))
        assertTrue(Transfers.mimeMatches("*/*", "application/pdf"))
        assertFalse(Transfers.mimeMatches("image/*", "imagery/png"))
        assertFalse(Transfers.mimeMatches("image/png", "image/jpeg"))
    }
}
