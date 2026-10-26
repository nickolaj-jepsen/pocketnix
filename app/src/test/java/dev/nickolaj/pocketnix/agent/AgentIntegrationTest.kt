package dev.nickolaj.pocketnix.agent

import com.termux.terminal.TerminalBackend
import dev.nickolaj.pocketnix.forward.PortForwards
import dev.nickolaj.pocketnix.forward.UdpForwarder
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.async
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
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Test
import java.io.ByteArrayOutputStream
import java.io.File
import java.net.DatagramPacket
import java.net.DatagramSocket
import java.net.InetAddress
import java.net.ServerSocket
import java.net.SocketTimeoutException
import java.net.Socket
import java.net.UnixDomainSocketAddress
import java.nio.ByteBuffer
import java.nio.channels.SocketChannel
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
    private fun freeUdpPort() = DatagramSocket(0).use { it.localPort }

    private lateinit var echo: ServerSocket
    private lateinit var udpEcho: DatagramSocket

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
        udpEcho = DatagramSocket(0, InetAddress.getByName("127.0.0.1"))
        Thread {
            val buf = ByteArray(65535)
            while (true) {
                val p = DatagramPacket(buf, buf.size)
                runCatching { udpEcho.receive(p) }.getOrNull() ?: break
                val reply = "echo:".toByteArray() + p.data.copyOfRange(0, p.length)
                runCatching { udpEcho.send(DatagramPacket(reply, reply.size, p.socketAddress)) }
            }
        }.apply { isDaemon = true }.start()
        File(dir, "history").writeText("ls\nmake\n")
        File(dir, "agent.json").writeText(
            """
            {"shell":"/bin/sh", "history":"${File(dir, "history").path}",
             "filesRoot":"${File(dir, "files").apply { mkdirs() }.path}", "share":{"directory":"${File(dir, "inbox").path}"},
             "forwards":[{"name":"echo","guestPort":${echo.localPort},"hostPort":${freePort()}},
               {"name":"uecho","guestPort":${udpEcho.localPort},"hostPort":${freeUdpPort()},"protocol":"udp"}],
             "terminal":{"profiles":[{"name":"shell"},
               {"name":"exit4","command":"echo hi; exit 4"},
               {"name":"size","command":"while read l; do stty size; done"}],
               "theme":{"colors":{"background":"#123456"}}}}
            """.trimIndent(),
        )
        port = freePort()
        agent = ProcessBuilder(
            bin, "--config", File(dir, "agent.json").path, "--listen", "tcp:127.0.0.1:$port", "--no-power",
            "--guest-socket", File(dir, "guest.sock").path,
        )
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
        if (::udpEcho.isInitialized) udpEcho.close()
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

    @Test
    fun updateStateAndHistory() = runBlocking {
        val ctl = ControlChannel(scope, "test")
        ctl.start(dialer)
        val u = withTimeout(10_000) { ctl.updates.filterNotNull().first() }
        assertEquals("idle", u.state)
        assertEquals(emptyList<String>(), u.sources)
        // No source configured: refused, and the refusal says why.
        val refused = runCatching { ctl.call(JSONObject().put("type", "update-check")) }.exceptionOrNull()
        assertTrue("$refused", refused is AgentException && refused.message!!.contains("no update source"))
        val h = ctl.call(JSONObject().put("type", "history"))
        assertEquals(listOf("make", "ls"), (0 until h.getJSONArray("entries").length()).map { h.getJSONArray("entries").getString(it) })
        // An updates refresh answers outside the requests, so the next request still pairs with its reply.
        ctl.refreshUpdates()
        assertTrue(ctl.request("ping"))
        ctl.stop()
    }

    @Test
    fun fileProviderAndShares() {
        val files = GuestFiles(dialer, "test")
        files.write("notes.txt", 5).use { it.write("hello".toByteArray()) }
        assertEquals("hello", files.read("notes.txt").use { it.readBytes().decodeToString() })
        assertEquals("hello", File(dir, "files/notes.txt").readText())
        val made = files.create("", "notes.txt", isDir = false)
        assertEquals("notes (1).txt", made.name)
        val dir2 = files.create("", "sub", isDir = true)
        assertTrue(dir2.dir)
        assertEquals("moved.txt", files.rename("notes (1).txt", "moved.txt").path)
        assertEquals(listOf("moved.txt", "notes.txt", "sub"), files.list("").map { it.name }.sorted())
        files.delete("sub")
        assertEquals(listOf("moved.txt", "notes.txt"), files.list("").map { it.name }.sorted())
        // Refused paths surface as AgentException.
        assertTrue(runCatching { files.read("../agent.json") }.exceptionOrNull() is AgentException)

        val landed = files.share("photo.jpg", 4, "JPEG".byteInputStream())
        assertEquals(File(dir, "inbox/photo.jpg").path, landed)
        assertEquals("JPEG", File(landed).readText())
    }

    @Test
    fun rangedFileAccess() {
        val files = GuestFiles(dialer, "test")
        val big = ByteArray(700_000) { (it % 251).toByte() }
        File(dir, "files/big.bin").writeBytes(big)

        // Page-sized reads, as a proxy fd asks, across the read-ahead's edges.
        files.openRanged("big.bin", "r")!!.use { f ->
            assertEquals(big.size.toLong(), f.size)
            val out = ByteArrayOutputStream()
            val page = ByteArray(4096)
            var at = 0L
            while (true) {
                val n = f.read(at, page, 0, page.size)
                if (n == 0) break
                out.write(page, 0, n)
                at += n
            }
            assertTrue(out.toByteArray().contentEquals(big))
            assertEquals(0, f.read(big.size + 10L, page, 0, page.size))
        }

        // "rw" edits in place; others see the old file until the commit.
        File(dir, "files/notes.txt").writeText("hello world")
        val rw = files.openRanged("notes.txt", "rw")!!
        rw.write(6, "WORLD".toByteArray(), 0, 5)
        val back = ByteArray(32)
        assertEquals("hello WORLD", String(back, 0, rw.read(0, back, 0, back.size)))
        rw.sync()
        assertEquals("hello world", File(dir, "files/notes.txt").readText())
        rw.commit()
        assertEquals("hello WORLD", File(dir, "files/notes.txt").readText())

        // An open that's closed without a commit changes nothing.
        files.openRanged("notes.txt", "w")!!.apply { write(0, "gone".toByteArray(), 0, 4) }.close()
        assertEquals("hello WORLD", File(dir, "files/notes.txt").readText())

        // "wa" appends whatever offset the writer is at.
        files.openRanged("notes.txt", "wa")!!.apply { write(0, "!".toByteArray(), 0, 1) }.commit()
        assertEquals("hello WORLD!", File(dir, "files/notes.txt").readText())

        // Refusals are AgentException; an agent without ranged access says so in a way open() recognizes.
        assertTrue(runCatching { files.openRanged("../agent.json", "r") }.exceptionOrNull() is AgentException)
        val unknown = runCatching { dialer.open(Protocol.hello("file", "test") { put("op", "bogus") }) }.exceptionOrNull()
        assertTrue("$unknown", unknown is AgentException && unknown.message!!.startsWith("unknown file operation"))
    }

    @Test
    fun guestEventsReachTheApp() = runBlocking {
        val ctl = ControlChannel(scope, "test")
        ctl.start(dialer)
        withTimeout(10_000) { ctl.updates.filterNotNull().first() }
        val event = scope.async { withTimeout(10_000) { ctl.events.first() } }
        // Give the collector a moment to subscribe before the push.
        Thread.sleep(100)
        val reply = guest("""{"type":"notify","title":"make","body":"done"}""")
        assertEquals("{}", reply)
        val ev = event.await()
        assertEquals(listOf("notify", "make", "done"), listOf(ev.getString("type"), ev.getString("title"), ev.getString("body")))
        // Android's clipboard reaches a guest paste.
        ctl.call(JSONObject().put("type", "set-clipboard").put("text", "from android"))
        assertEquals("from android", JSONObject(guest("""{"type":"paste"}""")).getString("text"))
        ctl.stop()
    }

    @Test
    fun aGuestPickGetsTheAppsFiles() = runBlocking {
        val ctl = ControlChannel(scope, "test")
        ctl.start(dialer)
        withTimeout(10_000) { ctl.updates.filterNotNull().first() }
        val event = scope.async { withTimeout(10_000) { ctl.events.first() } }
        Thread.sleep(100)
        val reply = scope.async { guest("""{"type":"pick","types":["image/*"]}""") }
        val ev = event.await()
        assertEquals("pick", ev.getString("type"))
        val ref = ev.getString("ref")
        val landed = GuestFiles(dialer, "test").share("cat.jpg", 4, "JPEG".byteInputStream(), ref)
        ctl.call(JSONObject().put("type", "guest-done").put("ref", ref))
        val paths = JSONObject(reply.await()).getJSONArray("paths")
        assertEquals(landed, paths.getString(0))
        assertEquals("JPEG", File(landed).readText())
        // Android's clipboard types reach the guest; its bytes stay put until a paste asks.
        ctl.call(JSONObject().put("type", "set-clipboard").put("text", "").put("types", JSONArray(listOf("image/png"))))
        assertEquals("image/png", JSONObject(guest("""{"type":"clipboard-types"}""")).getJSONArray("types").getString(0))
        ctl.stop()
    }

    /** One request on the agent's guest socket, as pocketnix-guest sends it. */
    private fun guest(line: String): String {
        SocketChannel.open(UnixDomainSocketAddress.of(File(dir, "guest.sock").path)).use { ch ->
            ch.write(ByteBuffer.wrap("$line\n".toByteArray()))
            val buf = ByteBuffer.allocate(4096)
            val out = StringBuilder()
            while (!out.endsWith("\n")) {
                buf.clear()
                if (ch.read(buf) < 0) break
                out.append(String(buf.array(), 0, buf.position()))
            }
            return out.toString().trim()
        }
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
            Socket("127.0.0.1", cfg.forwards.single { !it.udp }.hostPort).use { s ->
                s.getOutputStream().write("ping".toByteArray())
                s.shutdownOutput()
                assertEquals("echo:ping", String(s.getInputStream().readBytes()))
            }
        } finally {
            forwards.apply(emptyList(), null)
            ctl.stop()
        }
    }

    /** Sends [msg] from [client] to the phone-side [port] and returns the reply, or null if none came. */
    private fun roundTrip(client: DatagramSocket, port: Int, msg: String): String? {
        val out = msg.toByteArray()
        client.send(DatagramPacket(out, out.size, InetAddress.getByName("127.0.0.1"), port))
        val p = DatagramPacket(ByteArray(65535), 65535)
        return try {
            client.receive(p)
            String(p.data, 0, p.length)
        } catch (_: SocketTimeoutException) {
            null
        }
    }

    private fun udpClient() = DatagramSocket(0, InetAddress.getByName("127.0.0.1")).apply { soTimeout = 5_000 }

    @Test
    fun udpForwardRelaysEachClientsDatagrams() = runBlocking {
        val ctl = ControlChannel(scope, "test")
        ctl.start(dialer)
        val cfg = withTimeout(10_000) { ctl.config.filterNotNull().first() }
        val f = cfg.forwards.single { it.udp }
        val forwards = PortForwards("test")
        assertEquals(emptyList<String>(), forwards.apply(cfg.forwards, dialer))
        try {
            udpClient().use { a ->
                udpClient().use { b ->
                    // Two client addresses are two flows; each reply goes back to its sender.
                    assertEquals("echo:from-a", roundTrip(a, f.hostPort, "from-a"))
                    assertEquals("echo:from-b", roundTrip(b, f.hostPort, "from-b"))
                    val big = "x".repeat(60_000)
                    assertEquals("echo:$big", roundTrip(a, f.hostPort, big))
                    assertEquals("echo:", roundTrip(b, f.hostPort, ""))
                }
            }
        } finally {
            forwards.apply(emptyList(), null)
            ctl.stop()
        }
    }

    @Test
    fun udpFlowsAreCappedAndReaped() {
        val f = Forward("uecho", udpEcho.localPort, freeUdpPort(), lan = false, udp = true)
        val fw = UdpForwarder(f, dialer, "test", idleMs = 400, maxFlows = 1)
        try {
            udpClient().use { a ->
                udpClient().use { b ->
                    assertEquals("echo:1", roundTrip(a, f.hostPort, "1"))
                    // A second address past the cap is dropped, not queued.
                    b.soTimeout = 1_000
                    assertEquals(null, roundTrip(b, f.hostPort, "2"))
                    // Once a's flow idles out, b gets the slot.
                    Thread.sleep(1_500)
                    b.soTimeout = 5_000
                    assertEquals("echo:3", roundTrip(b, f.hostPort, "3"))
                }
            }
        } finally {
            fw.close()
        }
    }
}
