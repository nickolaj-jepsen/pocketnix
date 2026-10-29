package dev.nickolaj.pocketnix.gui

import android.util.Log
import dev.nickolaj.pocketnix.agent.AgentDialer
import dev.nickolaj.pocketnix.agent.AgentStream
import dev.nickolaj.pocketnix.agent.Protocol
import dev.nickolaj.pocketnix.agent.open
import java.io.EOFException
import java.io.IOException
import java.util.concurrent.LinkedBlockingQueue

/**
 * The one GUI stream: dials it, says hello, and hands the compositor's
 * messages to [listener] on its own thread, reconnecting with backoff until
 * [stop]. The agent restarting or the compositor closing over an error only
 * costs a reconnect, whose replay restores everything.
 */
class GuiLink(
    private val appVersion: String,
    private val listener: Listener
) {
    /** Called on the link's thread, in order. */
    interface Listener {
        /** The compositor said hello; messages follow, starting with its replay. */
        fun connected(hello: GuiMessage.Hello)

        fun message(msg: GuiMessage)

        /** The connection [connected] announced ended: null when [stop] ended it. */
        fun disconnected(error: Throwable?)
    }

    @Volatile private var thread: Thread? = null

    @Volatile private var stream: AgentStream? = null

    @Volatile private var writer: Writer? = null

    val connected: Boolean get() = writer != null

    /** (Re)starts the link; [hashes] asks the compositor for content hashes. */
    fun start(dialer: AgentDialer, hashes: Boolean = false) {
        stop()
        val t = Thread({ run(dialer, hashes) }, "gui-link").apply { isDaemon = true }
        thread = t
        t.start()
    }

    /** Stops the link; returns at once, the thread ends on its own. */
    fun stop() {
        val t = thread ?: return
        thread = null
        t.interrupt()
        stream?.let { runCatching { it.close() } }
    }

    /** Queues one frame for the compositor; dropped while not connected. */
    fun send(frame: ByteArray) {
        writer?.send(frame)
    }

    private fun run(dialer: AgentDialer, hashes: Boolean) {
        val me = Thread.currentThread()
        var backoff = MIN_BACKOFF_MS
        while (thread === me) {
            var s: AgentStream? = null
            var w: Writer? = null
            var announced = false
            var error: Throwable? = null
            try {
                s = dialer.open(Protocol.hello("gui", appVersion)).first
                stream = s
                if (thread !== me) break
                w = Writer(s)
                w.send(Gui.hello(appVersion, hashes))
                val reader = GuiReader(s.input)
                val hello = when (val first = reader.next() ?: throw EOFException("the compositor closed before its hello")) {
                    is GuiMessage.Hello -> first
                    is GuiMessage.Error -> throw IOException("the compositor refused: ${first.message}")
                    else -> throw GuiProtocolException("expected hello, got $first")
                }
                if (hello.version !in Gui.MIN_VERSION..Gui.VERSION) {
                    throw GuiProtocolException("the compositor chose GUI protocol ${hello.version}; the app speaks ${Gui.MIN_VERSION}–${Gui.VERSION}")
                }
                writer = w
                announced = true
                backoff = MIN_BACKOFF_MS
                Log.i(TAG, "connected to ${hello.compositor} (instance ${hello.instance})")
                listener.connected(hello)
                while (true) {
                    val msg = reader.next() ?: break
                    // Stopped: what's still arriving is no one's.
                    if (thread !== me) break
                    listener.message(msg)
                }
            } catch (e: GuiProtocolException) {
                error = e
                Log.w(TAG, "protocol error: ${e.message}")
            } catch (e: IOException) {
                // The agent or compositor isn't up yet, or went away.
                error = e
                if (announced) Log.i(TAG, "connection lost: ${e.message}")
            } catch (e: Exception) {
                error = e
                Log.w(TAG, "link failed", e)
            } catch (e: OutOfMemoryError) {
                // A big commit on a small heap: uncaught, it would end this thread and nothing would reconnect.
                error = e
                Log.w(TAG, "link ran out of memory", e)
            } finally {
                writer = null
                w?.close()
                s?.let { runCatching { it.close() } }
                if (stream === s) stream = null
                if (announced) {
                    runCatching { listener.disconnected(if (thread === me) error ?: EOFException("the compositor closed") else null) }
                        .onFailure { Log.w(TAG, "disconnected handler failed", it) }
                }
            }
            try {
                Thread.sleep(backoff)
            } catch (_: InterruptedException) {
                break
            }
            backoff = (backoff * 2).coerceAtMost(MAX_BACKOFF_MS)
        }
    }

    /** Writes queued frames on its own thread: input comes from the main thread, which mustn't touch a socket. */
    private class Writer(private val s: AgentStream) {
        private val queue = LinkedBlockingQueue<ByteArray>()
        private val thread = Thread({ loop() }, "gui-writer").apply { isDaemon = true }

        init {
            thread.start()
        }

        fun send(frame: ByteArray) {
            queue.add(frame)
        }

        fun close() {
            queue.add(CLOSE)
        }

        private fun loop() {
            try {
                while (true) {
                    var f = queue.take()
                    if (f === CLOSE) return
                    // Batch what's queued into one flush.
                    while (f !== CLOSE) {
                        s.output.write(f)
                        f = queue.poll() ?: break
                    }
                    s.output.flush()
                    if (f === CLOSE) return
                }
            } catch (e: IOException) {
                // The reader sees the same connection end.
                runCatching { s.close() }
            }
        }

        private companion object {
            val CLOSE = ByteArray(0)
        }
    }

    private companion object {
        const val TAG = "GuiLink"
        const val MIN_BACKOFF_MS = 250L
        const val MAX_BACKOFF_MS = 5_000L
    }
}
