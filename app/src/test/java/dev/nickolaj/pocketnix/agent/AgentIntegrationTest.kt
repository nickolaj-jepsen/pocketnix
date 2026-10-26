package dev.nickolaj.pocketnix.agent

import com.termux.terminal.TerminalBackend
import dev.nickolaj.pocketnix.forward.PortForwards
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.filterNotNull
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Before
import org.junit.Test
import java.io.ByteArrayOutputStream
import java.io.File
import java.net.ServerSocket
import java.net.Socket
import java.nio.file.Files
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/**
 * The app's agent code against the real Go agent over TCP. Opt-in:
 * POCKETNIX_AGENT=$(nix build --print-out-paths .#pocketnix)/bin/pocketnix-agent ./gradlew :app:testDebugUnitTest
 */
class AgentIntegrationTest {
    private lateinit var agent: Process
    private lateinit var dir: File
    private var port = 0
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val dialer get() = TcpDialer("127.0.0.1", port)

    private fun freePort() = ServerSocket(0).use { it.localPort }

    private lateinit var echo: ServerSocket

    @Before
    fun startAgent() {
        val bin = System.getenv("POCKETNIX_AGENT")
        assumeTrue("POCKETNIX_AGENT not set", !bin.isNullOrEmpty())
        dir = Files.createTempDirectory("pocketnix-agent").toFile()
        echo = ServerSocket(0)
        Thread {
            while (true) {
                val c = runCatching { echo.accept() }.getOrNull() ?: break
                Thread { c.use { s -> s.getOutputStream().write("echo:".toByteArray() + s.getInputStream().readBytes()) } }.start()
            }
        }.apply { isDaemon = true }.start()
        File(dir, "agent.json").writeText(
            """
            {"shell":"/bin/sh",
             "forwards":[{"name":"echo","guestPort":${echo.localPort},"hostPort":${freePort()}}],
             "terminal":{"profiles":[{"name":"shell"},
               {"name":"exit4","command":"echo hi; exit 4"},
               {"name":"size","command":"while read l; do stty size; done"}],
               "theme":{"colors":{"background":"#123456"}}}}
            """.trimIndent(),
        )
        port = freePort()
        agent = ProcessBuilder(bin, "--config", File(dir, "agent.json").path, "--listen", "tcp:127.0.0.1:$port", "--no-power")
            .redirectErrorStream(true).redirectOutput(File(dir, "agent.log")).start()
        // Wait for the listener.
        val deadline = System.currentTimeMillis() + 10_000
        while (runCatching { Socket("127.0.0.1", port).close() }.isFailure) {
            check(System.currentTimeMillis() < deadline) { "agent didn't listen: " + File(dir, "agent.log").readText() }
            Thread.sleep(50)
        }
    }

    @After
    fun stop() {
        scope.cancel()
        if (::agent.isInitialized) agent.destroy()
        if (::echo.isInitialized) echo.close()
        if (::dir.isInitialized) dir.deleteRecursively()
    }

    @Test
    fun controlChannelDeliversTheConfig() = runBlocking {
        val ctl = ControlChannel(scope, "test")
        ctl.start(dialer)
        val cfg = withTimeout(10_000) { ctl.config.filterNotNull().first() }
        assertEquals("#123456", cfg.theme.colors["background"])
        assertEquals(listOf("shell", "exit4", "size"), cfg.profiles.map { it.name })
        assertTrue(ctl.request("ping"))
        // The agent runs with --no-power: poweroff is refused, and request says so.
        assertEquals(false, ctl.request("poweroff"))
        ctl.stop()
    }

    private class Recorder : TerminalBackend.Sink {
        val out = ByteArrayOutputStream()
        val done = CountDownLatch(1)
        @Volatile var status: Int? = null
        @Volatile var reason: String? = null

        override fun onOutput(data: ByteArray, offset: Int, count: Int): Boolean {
            synchronized(out) { out.write(data, offset, count) }
            return true
        }

        override fun onExit(status: Int?, reason: String?) {
            this.status = status
            this.reason = reason
            done.countDown()
        }

        fun text() = synchronized(out) { out.toString(Charsets.UTF_8.name()) }

        fun awaitText(want: String) {
            val deadline = System.currentTimeMillis() + 10_000
            while (!text().contains(want)) {
                check(System.currentTimeMillis() < deadline) { "no '$want' in '${text()}'" }
                Thread.sleep(20)
            }
        }
    }

    @Test
    fun ptySessionReportsItsExitStatus() {
        val sink = Recorder()
        PtyBackend(dialer, "exit4", "test").start(80, 24, 10, 20, sink)
        assertTrue(sink.done.await(10, TimeUnit.SECONDS))
        assertEquals(4, sink.status)
        assertTrue(sink.text(), sink.text().contains("hi"))
    }

    @Test
    fun ptySessionFollowsResizes() {
        val sink = Recorder()
        val pty = PtyBackend(dialer, "size", "test")
        pty.start(80, 24, 10, 20, sink)
        pty.write("\r".toByteArray(), 0, 1)
        sink.awaitText("24 80")
        pty.resize(132, 50, 10, 20)
        // The resize goes out on its own thread; the next line must come after it.
        Thread.sleep(200)
        pty.write("\r".toByteArray(), 0, 1)
        sink.awaitText("50 132")
        pty.close()
        assertTrue(sink.done.await(10, TimeUnit.SECONDS))
    }

    @Test
    fun unknownProfileIsReported() {
        val sink = Recorder()
        PtyBackend(dialer, "nope", "test").start(80, 24, 10, 20, sink)
        assertTrue(sink.done.await(10, TimeUnit.SECONDS))
        assertTrue(sink.reason.orEmpty(), sink.reason.orEmpty().contains("unknown profile"))
    }

    @Test
    fun portForwardReachesTheGuestPort() = runBlocking {
        val ctl = ControlChannel(scope, "test")
        ctl.start(dialer)
        val cfg = withTimeout(10_000) { ctl.config.filterNotNull().first() }
        val forwards = PortForwards("test")
        forwards.apply(cfg.forwards, dialer)
        try {
            Socket("127.0.0.1", cfg.forwards.single().hostPort).use { s ->
                s.getOutputStream().write("ping".toByteArray())
                s.shutdownOutput()
                assertEquals("echo:ping", String(s.getInputStream().readBytes()))
            }
        } finally {
            forwards.apply(emptyList(), null)
            ctl.stop()
        }
    }
}
