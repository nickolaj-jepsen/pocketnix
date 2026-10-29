package dev.nickolaj.pocketnix.gui

import java.io.File
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.net.UnixDomainSocketAddress
import java.nio.channels.Channels
import java.nio.channels.SocketChannel
import java.nio.file.Files
import java.nio.file.Path
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Before
import org.junit.Test

/**
 * The decoder and the reference copy against a real compositor and a real
 * client: every commit is mirrored on the JVM and its content hash compared
 * with the compositor's. Opt-in through POCKETNIX_COMPOSITOR, the compositor
 * binary, which `nix run .#gradle` sets.
 *
 * The client is foot from PATH, or POCKETNIX_GUI_CLIENT (a shell command,
 * e.g. a full path to gtk3-widget-factory).
 */
class CompositorIntegrationTest {
    private lateinit var dir: File
    private lateinit var compositor: Process
    private var client: Process? = null
    private val conns = mutableListOf<Conn>()

    @Before
    fun startCompositor() {
        val bin = System.getenv("POCKETNIX_COMPOSITOR")
        assumeTrue("POCKETNIX_COMPOSITOR not set", !bin.isNullOrEmpty())
        // Short: a socket path must fit in sun_path.
        dir = Files.createTempDirectory(Path.of("/tmp"), "pnx-gui").toFile()
        compositor = ProcessBuilder(bin, "--gui-socket", File(dir, "gui.sock").path, "--wayland-display", WAYLAND, "--log-hashes")
            .redirectErrorStream(true)
            .redirectOutput(File(dir, "compositor.log"))
            .apply { environment()["XDG_RUNTIME_DIR"] = dir.path }
            .start()
        val deadline = System.currentTimeMillis() + 10_000
        while (!File(dir, "gui.sock").exists() || !File(dir, WAYLAND).exists()) {
            check(compositor.isAlive) { "compositor exited: ${log()}" }
            check(System.currentTimeMillis() < deadline) { "compositor's sockets didn't appear: ${log()}" }
            Thread.sleep(50)
        }
    }

    @After
    fun stop() {
        for (c in conns) c.close()
        client?.let {
            it.destroy()
            it.waitFor(5, TimeUnit.SECONDS)
        }
        if (::compositor.isInitialized) {
            compositor.destroy()
            compositor.waitFor(5, TimeUnit.SECONDS)
        }
        if (::dir.isInitialized) dir.deleteRecursively()
    }

    private fun log(): String = runCatching { File(dir, "compositor.log").readText().takeLast(4000) }.getOrDefault("")

    private fun startClient() {
        val custom = System.getenv("POCKETNIX_GUI_CLIENT")
        val cmd = if (!custom.isNullOrEmpty()) {
            listOf("sh", "-c", custom)
        } else {
            // Scrolls on its own: whole-window damage that the compositor sends as scrolls and rects.
            listOf("foot", "sh", "-c", "for i in \$(seq 1 2000); do echo line \$i; sleep 0.01; done; sleep 600")
        }
        client = try {
            ProcessBuilder(cmd)
                .redirectErrorStream(true)
                .redirectOutput(File(dir, "client.log"))
                .apply {
                    environment()["WAYLAND_DISPLAY"] = WAYLAND
                    environment()["XDG_RUNTIME_DIR"] = dir.path
                    environment().remove("DISPLAY")
                }
                .start()
        } catch (e: IOException) {
            assumeTrue("can't start the client (${cmd.first()}): ${e.message}; set POCKETNIX_GUI_CLIENT", false)
            null
        }
    }

    /** One app connection, read on its own thread. */
    private inner class Conn {
        private val ch = SocketChannel.open(UnixDomainSocketAddress.of(File(dir, "gui.sock").toPath()))
        val input: InputStream = Channels.newInputStream(ch)
        val output: OutputStream = Channels.newOutputStream(ch)
        val messages = LinkedBlockingQueue<Any>()

        init {
            conns += this
            output.write(Gui.hello("pocketnix test", hashes = true))
            Thread {
                val r = GuiReader(input)
                try {
                    while (true) messages.add(r.next() ?: break)
                    messages.add(EOF)
                } catch (e: Exception) {
                    messages.add(e)
                }
            }.apply { isDaemon = true }.start()
        }

        fun next(timeoutMs: Long = 10_000): Any = messages.poll(timeoutMs, TimeUnit.MILLISECONDS) ?: throw AssertionError("nothing in ${timeoutMs}ms; log: ${log()}")

        fun send(frame: ByteArray) {
            output.write(frame)
            output.flush()
        }

        fun close() = runCatching { ch.close() }
    }

    /** Mirrors a connection's messages into [state] the way the app does, acknowledging every group. */
    private inner class Mirror(val state: GuiState) {
        var hashed = 0
        var partial = 0
        val mismatches = mutableListOf<String>()
        val newWindows = mutableListOf<Int>()

        init {
            state.onHash = { s, seq, expected, actual ->
                hashed++
                if (expected != actual) mismatches += "surface ${s.u} seq ${seq.u}: %016x != %016x".format(expected, actual)
            }
        }

        fun handle(c: Conn, m: Any) {
            when (m) {
                is GuiMessage.Toplevel -> if (state.toplevel(m)) newWindows += m.id

                is GuiMessage.ToplevelRemoved -> state.toplevelRemoved(m.id)

                is GuiMessage.Surface -> state.surface(m)

                is GuiMessage.SurfaceRemoved -> state.surfaceRemoved(m.id)

                GuiMessage.Ready -> state.ready()

                is GuiMessage.Group -> {
                    state.apply(m)
                    partial += m.commits.count { it.buffer && !it.full }
                    for (w in m.commits.map { it.window }.distinct()) c.send(Gui.presented(w, state.windows.getValue(w).lastSeq))
                }

                is GuiMessage.Error -> throw AssertionError("compositor error: ${m.message}")

                is Throwable -> throw AssertionError("connection failed", m)

                EOF -> throw AssertionError("compositor closed the connection; log: ${log()}")

                else -> {}
            }
        }
    }

    private fun handshake(c: Conn): GuiMessage.Hello {
        val hello = c.next() as GuiMessage.Hello
        assertEquals(Gui.VERSION, hello.version)
        c.send(Gui.output(1080, 2400, 2.625f, 60_000))
        return hello
    }

    private fun windowState(id: Int) = Gui.window(WindowState(id, 1080, 2000, 2.625f, focused = true, visible = true, maximized = true))

    @Test
    fun mirrorsAClientExactlyAcrossAReconnect() {
        val state = GuiState(JavaUnpacker)
        val mirror = Mirror(state)
        val first = Conn()
        val hello = handshake(first)
        state.hello(hello)
        startClient()

        // Until the client has drawn a while, including damage that isn't the whole buffer.
        val deadline = System.currentTimeMillis() + 30_000
        var input = false
        while ((mirror.hashed < 60 || mirror.partial < 5) && System.currentTimeMillis() < deadline) {
            mirror.handle(first, first.next())
            for (id in mirror.newWindows) first.send(windowState(id))
            mirror.newWindows.clear()
            val w = state.windows.keys.firstOrNull()
            if (!input && w != null && mirror.hashed > 10) {
                // Input it must parse; a violation would close the connection.
                input = true
                first.send(Gui.pointerMotion(w, 100f, 120.5f))
                first.send(Gui.pointerButton(w, 100f, 120.5f, Gui.BTN_LEFT, true))
                first.send(Gui.pointerButton(w, 100f, 120.5f, Gui.BTN_LEFT, false))
                first.send(Gui.pointerAxis(w, 100f, 120.5f, Gui.AXIS_WHEEL, 0, 0f, 39.375f, 0, 120))
                first.send(Gui.modifiers(w, Gui.MOD_SHIFT))
                first.send(Gui.key(w, 30, true))
                first.send(Gui.key(w, 30, false))
                first.send(Gui.modifiers(w, 0))
                first.send(Gui.pointerLeave(w))
            }
        }
        assertTrue("no window mapped; log: ${log()}", state.windows.isNotEmpty())
        assertEquals(emptyList<String>(), mirror.mismatches)
        assertTrue("only ${mirror.hashed} hashed commits", mirror.hashed >= 60)
        assertTrue("only ${mirror.partial} commits that weren't FULL", mirror.partial >= 5)

        // A second app connection takes over; the first is closed, and the replay restores everything.
        val windows = state.windows.keys.toSet()
        val second = Conn()
        val again = handshake(second)
        assertEquals("the same compositor", hello.instance, again.instance)
        assertTrue("an empty replay", state.hello(again).isEmpty())
        for (id in windows) second.send(windowState(id))
        val firstEnded = generateSequence { first.next() }.first { it === EOF || it is Throwable }
        assertTrue(firstEnded === EOF || firstEnded is IOException)

        val before = mirror.hashed
        var ready = false
        while (!ready || mirror.hashed < before + 20) {
            val m = second.next()
            if (m === GuiMessage.Ready) {
                ready = true
                assertEquals(windows, state.windows.keys)
                assertTrue(state.surfaces.values.filter { it.role == "toplevel" }.all { it.mapped })
            }
            mirror.handle(second, m)
        }
        assertEquals(emptyList<String>(), mirror.mismatches)
    }

    private companion object {
        const val WAYLAND = "pnx-test-wl"
        val EOF = Any()
    }
}
