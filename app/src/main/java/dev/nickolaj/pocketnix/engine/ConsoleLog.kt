package dev.nickolaj.pocketnix.engine

import com.termux.terminal.TerminalBackend
import java.io.OutputStream
import java.util.concurrent.CopyOnWriteArrayList

/**
 * The hvc0 console across VM runs: recent output for a newly attached tab,
 * live output for attached ones, and input to the running VM.
 */
class ConsoleLog(private val capacity: Int = 256 * 1024) {
    private val ring = ByteArray(capacity)
    private var start = 0
    private var size = 0
    private val listeners = CopyOnWriteArrayList<TerminalBackend.Sink>()

    /** The running VM's console input; null while stopped. */
    @Volatile var input: OutputStream? = null

    /**
     * Records output and passes it to attached tabs. Called from the console
     * pump; a slow tab blocks it, which is what the guest's console wants.
     */
    fun append(b: ByteArray, off: Int, n: Int) {
        synchronized(this) {
            for (i in 0 until n) {
                ring[(start + size) % capacity] = b[off + i]
                if (size < capacity) size++ else start = (start + 1) % capacity
            }
            for (l in listeners) if (!l.onOutput(b, off, n)) listeners.remove(l)
        }
    }

    fun append(s: String) = s.toByteArray(Charsets.UTF_8).let { append(it, 0, it.size) }

    /** Replays recent output to [sink], then keeps it attached. Blocks while the sink is behind. */
    fun attach(sink: TerminalBackend.Sink) {
        synchronized(this) {
            val snapshot = ByteArray(size)
            for (i in 0 until size) snapshot[i] = ring[(start + i) % capacity]
            if (snapshot.isEmpty() || sink.onOutput(snapshot, 0, snapshot.size)) listeners.add(sink)
        }
    }

    fun detach(sink: TerminalBackend.Sink) {
        listeners.remove(sink)
    }

    /** A terminal backend showing this console. Its session never ends: the console outlives VM runs. */
    fun backend(): TerminalBackend = object : TerminalBackend {
        private var sink: TerminalBackend.Sink? = null

        override fun start(columns: Int, rows: Int, cellWidthPixels: Int, cellHeightPixels: Int, sink: TerminalBackend.Sink) {
            this.sink = sink
            // attach can block on the main thread's draining, so never run it there.
            Thread({ attach(sink) }, "console-attach").apply { isDaemon = true }.start()
        }

        override fun write(data: ByteArray, offset: Int, count: Int) {
            val out = input ?: return
            runCatching {
                out.write(data, offset, count)
                out.flush()
            }
        }

        // hvc0 has no window size; `resize` in the console shell sets it.
        override fun resize(columns: Int, rows: Int, cellWidthPixels: Int, cellHeightPixels: Int) {}

        override fun close() {
            sink?.let { detach(it) }
        }
    }
}
