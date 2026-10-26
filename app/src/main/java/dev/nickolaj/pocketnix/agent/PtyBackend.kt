package dev.nickolaj.pocketnix.agent

import com.termux.terminal.TerminalBackend
import java.io.IOException
import java.util.concurrent.CountDownLatch
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicBoolean

/** A terminal tab: one PTY stream to the agent, running [profile] (null for the default). */
class PtyBackend(
    private val dialer: AgentDialer,
    private val profile: String?,
    private val appVersion: String,
) : TerminalBackend {
    @Volatile private var stream: AgentStream? = null
    private val connected = CountDownLatch(1)
    private val closed = AtomicBoolean(false)
    private val exited = AtomicBoolean(false)
    // Resizes come from the main thread, which mustn't block on the socket.
    private val sender = Executors.newSingleThreadExecutor { r -> Thread(r, "pty-resize").apply { isDaemon = true } }
    private lateinit var sink: TerminalBackend.Sink

    override fun start(columns: Int, rows: Int, cellWidthPixels: Int, cellHeightPixels: Int, sink: TerminalBackend.Sink) {
        this.sink = sink
        Thread({ run(columns, rows) }, "pty-reader").apply { isDaemon = true }.start()
    }

    private fun run(columns: Int, rows: Int) {
        var status: Int? = null
        var reason: String? = null
        try {
            val (s, _) = dialer.open(
                Protocol.hello("pty", appVersion) {
                    profile?.let { put("profile", it) }
                    put("rows", rows)
                    put("cols", columns)
                },
            )
            stream = s
            connected.countDown()
            if (closed.get()) return
            while (true) {
                val f = Frames.read(s.input) ?: break
                when (f.type) {
                    Frames.DATA -> if (!sink.onOutput(f.payload, 0, f.payload.size)) break
                    Frames.EXIT -> {
                        status = Frames.exitStatus(f.payload)
                        break
                    }
                }
            }
            if (status == null && !closed.get()) reason = "Connection to the guest lost"
        } catch (e: AgentException) {
            reason = "The agent refused the session: ${e.message}"
        } catch (e: IOException) {
            if (!closed.get()) reason = if (stream == null) "Couldn't reach the agent: ${e.message}" else "Connection to the guest lost"
        } finally {
            connected.countDown()
            close()
            if (exited.compareAndSet(false, true)) sink.onExit(status, reason)
        }
    }

    override fun write(data: ByteArray, offset: Int, count: Int) {
        connected.await()
        val s = stream ?: throw IOException("not connected")
        val frame = Frames.encode(Frames.DATA, data, offset, count)
        synchronized(s) {
            s.output.write(frame)
            s.output.flush()
        }
    }

    override fun resize(columns: Int, rows: Int, cellWidthPixels: Int, cellHeightPixels: Int) {
        if (closed.get()) return
        // A close racing this can shut the executor down first; the resize no longer matters then.
        runCatching {
            sender.execute {
                connected.await()
                val s = stream ?: return@execute
                runCatching {
                    synchronized(s) { Frames.write(s.output, Frames.RESIZE, Frames.resize(rows, columns)) }
                }
            }
        }
    }

    override fun close() {
        if (!closed.compareAndSet(false, true)) return
        sender.shutdown()
        stream?.let { runCatching { it.close() } }
    }
}
