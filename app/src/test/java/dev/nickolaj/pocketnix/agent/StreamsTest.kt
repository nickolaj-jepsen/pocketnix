package dev.nickolaj.pocketnix.agent

import com.termux.terminal.TerminalBackend
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.Closeable
import java.io.IOException
import java.io.OutputStream
import java.net.ServerSocket
import java.nio.ByteBuffer
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/** PTY and file streams against stand-in agents: what reaches the guest, and what the app reads. */
class StreamsTest {
    private val servers = mutableListOf<Closeable>()

    @After
    fun tearDown() = servers.forEach { runCatching { it.close() } }

    @Test
    fun aTabClosedWhileDialingHangsUpItsStream() {
        val hungUp = CountDownLatch(1)
        val agent = FakeAgent { c -> if (c.input.read() < 0) hungUp.countDown() }.also { servers += it }
        val dialing = CountDownLatch(1)
        val release = CountDownLatch(1)
        val slow = AgentDialer {
            dialing.countDown()
            release.await()
            agent.dialer.dial()
        }
        val sink = RecordingSink()
        val pty = PtyBackend(slow, null, "test")
        pty.start(80, 24, 1, 1, sink)
        assertTrue(dialing.await(5, TimeUnit.SECONDS))
        pty.close()
        release.countDown()
        assertTrue("the guest's session stayed open", hungUp.await(5, TimeUnit.SECONDS))
        assertTrue(sink.exited.await(5, TimeUnit.SECONDS))
    }

    @Test
    fun ptyFramesArriveWholeAndInOrder() {
        val frames = (1..50).map { i -> ByteArray(i * 997) { i.toByte() } }
        val dialer = rawAgent("""{"type":"hello","version":1}""") { out ->
            for (f in frames) out.write(Frames.encode(Frames.DATA, f))
            out.write(Frames.encode(Frames.EXIT, ByteBuffer.allocate(4).putInt(3).array()))
        }
        val sink = RecordingSink()
        PtyBackend(dialer, null, "test").start(80, 24, 1, 1, sink)
        assertTrue(sink.exited.await(5, TimeUnit.SECONDS))
        assertEquals(3, sink.status)
        assertArrayEquals(frames.reduce(ByteArray::plus), sink.output())
    }

    @Test
    fun aFileStreamThatEndsEarlyIsAnError() {
        val files = GuestFiles(rawAgent("""{"type":"hello","version":1,"size":10}""") { it.write("abcd".toByteArray()) }, "test")
        val read = runCatching { files.read("f").use { it.readBytes() } }
        assertTrue(read.exceptionOrNull() is IOException)
    }

    @Test
    fun aFileStreamReadsExactlyItsSize() {
        val body = ByteArray(200_000) { (it % 251).toByte() }
        val dialer = rawAgent("""{"type":"hello","version":1,"size":${body.size}}""") { it.write(body + "trailing".toByteArray()) }
        assertArrayEquals(body, GuestFiles(dialer, "test").read("f").use { it.readBytes() })
    }

    private class RecordingSink : TerminalBackend.Sink {
        private val buf = java.io.ByteArrayOutputStream()
        val exited = CountDownLatch(1)
        @Volatile var status: Int? = null

        @Synchronized
        override fun onOutput(data: ByteArray, offset: Int, count: Int): Boolean {
            buf.write(data, offset, count)
            return true
        }

        override fun onExit(status: Int?, reason: String?) {
            this.status = status
            exited.countDown()
        }

        @Synchronized
        fun output(): ByteArray = buf.toByteArray()
    }

    /** An agent that answers each Hello with [hello], then writes [body] and ends the stream. */
    private fun rawAgent(hello: String, body: (OutputStream) -> Unit): AgentDialer {
        val server = ServerSocket(0).also { servers += it }
        Thread {
            while (true) {
                val s = runCatching { server.accept() }.getOrNull() ?: break
                Thread {
                    s.use {
                        Protocol.readLine(s.getInputStream())
                        val out = s.getOutputStream()
                        out.write((hello + "\n").toByteArray())
                        body(out)
                        out.flush()
                    }
                }.apply { isDaemon = true }.start()
            }
        }.apply { isDaemon = true }.start()
        return TcpDialer("127.0.0.1", server.localPort)
    }
}
