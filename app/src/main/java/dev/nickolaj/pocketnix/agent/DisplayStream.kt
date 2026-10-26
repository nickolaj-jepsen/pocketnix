package dev.nickolaj.pocketnix.agent

import android.util.Log
import java.util.concurrent.atomic.AtomicBoolean

/**
 * One display stream: the guest compositor's output, handed to [sink] band by
 * band. The agent sends only what changed, so a still screen costs nothing;
 * the sink keeps the last full frame.
 */
class DisplayStream(
    private val dialer: AgentDialer,
    private val appVersion: String,
    private val sink: Sink,
) {
    /** Where frames go: the UI thread's business, not the reader's. */
    interface Sink {
        fun onGeometry(width: Int, height: Int)
        fun onBand(y: Int, height: Int, stride: Int, pixels: ByteArray)

        /** A damage rect: what a compositor's damage really is. */
        fun onRect(x: Int, y: Int, width: Int, height: Int, stride: Int, pixels: ByteArray)

        /** The stream ended: null when the app closed it, else why. */
        fun onEnd(reason: String?)
    }

    @Volatile private var stream: AgentStream? = null
    private val closed = AtomicBoolean(false)

    fun start() {
        Thread({ run() }, "display-reader").apply { isDaemon = true }.start()
    }

    fun close() {
        closed.set(true)
        stream?.let { runCatching { it.close() } }
    }

    /** Sends one input event; false once the stream is gone. */
    fun sendInput(kind: Int, a: Int, b: Int): Boolean {
        val s = stream ?: return false
        return runCatching {
            synchronized(s) {
                s.output.write(Frames.displayInput(kind, a, b))
                s.output.flush()
            }
        }.isSuccess
    }

    private fun run() {
        var reason: String? = null
        try {
            val (s, _) = dialer.open(Protocol.hello("display", appVersion))
            stream = s
            if (closed.get()) {
                runCatching { s.close() }
                return
            }
            var sized = false
            while (true) {
                val f = Frames.read(s.input) ?: break
                when (f.type) {
                    Frames.DISPLAY_GEOMETRY -> {
                        val g = Frames.geometry(f.payload)
                        sized = true
                        sink.onGeometry(g.width, g.height)
                    }
                    Frames.DISPLAY_BAND -> {
                        if (!sized) throw AgentException("display band before its geometry")
                        val band = Frames.band(f.payload)
                        sink.onBand(band.y, band.height, band.stride, band.pixels)
                    }
                    Frames.DISPLAY_RECT -> {
                        if (!sized) throw AgentException("display rect before its geometry")
                        val r = Frames.rect(f.payload)
                        sink.onRect(r.x, r.y, r.width, r.height, r.stride, r.pixels)
                    }
                    // Unknown types are for a newer agent to add.
                }
            }
            if (!closed.get()) reason = "Connection to the guest lost"
        } catch (e: AgentException) {
            reason = "The agent refused the display: ${e.message}"
        } catch (e: java.io.IOException) {
            if (!closed.get()) {
                Log.w(TAG, "display stream failed", e)
                reason = if (stream == null) "Couldn't reach the agent: ${e.message}" else "Connection to the guest lost"
            }
        } catch (e: Exception) {
            // A garbled frame must end the stream, not the app.
            reason = "Display failed: ${e.message ?: e.javaClass.simpleName}"
        } finally {
            close()
            sink.onEnd(reason)
        }
    }

    private companion object {
        const val TAG = "DisplayStream"
    }
}
