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
            record(b, off, n)
            for (l in listeners) if (!l.onOutput(b, off, n)) listeners.remove(l)
        }
    }

    private fun record(b: ByteArray, off: Int, n: Int) {
        // Only the last [capacity] bytes of a big write survive anyway.
        val skip = (n - capacity).coerceAtLeast(0)
        var from = off + skip
        var left = n - skip
        while (left > 0) {
            val end = (start + size) % capacity
            val chunk = minOf(left, capacity - end)
            System.arraycopy(b, from, ring, end, chunk)
            from += chunk
            left -= chunk
            val overflow = (size + chunk - capacity).coerceAtLeast(0)
            size = minOf(size + chunk, capacity)
            start = (start + overflow) % capacity
        }
    }

    /** The recorded output, oldest first. */
    @Synchronized
    fun snapshot(): ByteArray {
        val out = ByteArray(size)
        val first = minOf(size, capacity - start)
        System.arraycopy(ring, start, out, 0, first)
        System.arraycopy(ring, 0, out, first, size - first)
        return out
    }

    fun append(s: String) = s.toByteArray(Charsets.UTF_8).let { append(it, 0, it.size) }

    /** Replays recent output to [sink], then keeps it attached. Blocks while the sink is behind. */
    fun attach(sink: TerminalBackend.Sink) {
        synchronized(this) {
            val snapshot = snapshot()
            if (snapshot.isEmpty() || sink.onOutput(snapshot, 0, snapshot.size)) listeners.add(sink)
        }
    }

    /** Attaches [sink] to live output only, with no replay. */
    fun listen(sink: TerminalBackend.Sink) {
        listeners.add(sink)
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
