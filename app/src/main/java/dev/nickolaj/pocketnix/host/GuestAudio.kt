package dev.nickolaj.pocketnix.host

import android.media.AudioAttributes
import android.media.AudioFormat
import android.media.AudioTrack
import android.os.SystemClock
import android.util.Log
import dev.nickolaj.pocketnix.agent.AgentDialer
import dev.nickolaj.pocketnix.agent.AgentException
import dev.nickolaj.pocketnix.agent.AgentStream
import dev.nickolaj.pocketnix.agent.Protocol
import dev.nickolaj.pocketnix.agent.open
import java.io.IOException
import java.io.InputStream
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicLong

/**
 * Plays the guest's sound from the agent's `audio` stream (go/internal/protocol's
 * StreamAudio): raw PCM, signed 16-bit, 48 kHz, stereo. Reconnects with backoff
 * until [stop]; an agent that refuses the stream (no sound pipe, or older)
 * stops it until the next [start].
 */
class GuestAudio(private val appVersion: String) {
    enum class Status { OFF, DIALING, STREAMING, REFUSED }

    /** Where the stream is; [Status.REFUSED] lasts until the next [start]. */
    @Volatile var status = Status.OFF
        private set

    @Volatile private var thread: Thread? = null

    @Volatile private var stream: AgentStream? = null

    fun start(dialer: AgentDialer) {
        stop()
        status = Status.DIALING
        thread = Thread({ run(dialer) }, "guest-audio").apply {
            isDaemon = true
            start()
        }
    }

    fun stop() {
        val t = thread ?: return
        thread = null
        status = Status.OFF
        t.interrupt()
        stream?.let { runCatching { it.close() } }
    }

    private fun run(dialer: AgentDialer) {
        val me = Thread.currentThread()
        var backoff = MIN_BACKOFF_MS
        while (thread === me) {
            try {
                val s = dialer.open(Protocol.hello("audio", appVersion)).first
                stream = s
                // stop() may have run before the stream was set, and missed it.
                if (thread !== me) {
                    s.close()
                    return
                }
                backoff = MIN_BACKOFF_MS
                status = Status.STREAMING
                s.use { play(it.input, me) }
            } catch (e: AgentException) {
                Log.i(TAG, "no guest sound: ${e.message}")
                if (thread === me) status = Status.REFUSED
                return
            } catch (e: IOException) {
                // The agent restarted, or the guest's sound server went away.
                if (thread === me) status = Status.DIALING
            } finally {
                stream = null
            }
            try {
                Thread.sleep(backoff)
            } catch (_: InterruptedException) {
                return
            }
            backoff = (backoff * 2).coerceAtMost(MAX_BACKOFF_MS)
        }
    }

    private fun play(input: InputStream, me: Thread) {
        val format = AudioFormat.Builder().setEncoding(AudioFormat.ENCODING_PCM_16BIT).setSampleRate(RATE)
            .setChannelMask(AudioFormat.CHANNEL_OUT_STEREO).build()
        val min = AudioTrack.getMinBufferSize(RATE, AudioFormat.CHANNEL_OUT_STEREO, AudioFormat.ENCODING_PCM_16BIT)
        val track = AudioTrack.Builder()
            .setAudioAttributes(AudioAttributes.Builder().setUsage(AudioAttributes.USAGE_MEDIA).build())
            .setAudioFormat(format)
            .setTransferMode(AudioTrack.MODE_STREAM)
            .setPerformanceMode(AudioTrack.PERFORMANCE_MODE_LOW_LATENCY)
            .setBufferSizeInBytes(maxOf(min, BUFFER_BYTES))
            .build()
        // The stream sends nothing while the guest is silent, so reading would wait forever to pause.
        val lastData = AtomicLong(SystemClock.elapsedRealtime())
        val idle = Executors.newSingleThreadScheduledExecutor { Thread(it, "guest-audio-idle").apply { isDaemon = true } }
        idle.scheduleWithFixedDelay({
            if (track.playState == AudioTrack.PLAYSTATE_PLAYING && SystemClock.elapsedRealtime() - lastData.get() > IDLE_MS) track.pause()
        }, IDLE_MS, IDLE_MS / 2, TimeUnit.MILLISECONDS)
        try {
            val buf = ByteArray(READ_BYTES)
            // Bytes of a frame the last read cut in two.
            var have = 0
            while (thread === me) {
                val n = input.read(buf, have, buf.size - have)
                if (n < 0 || thread !== me) return
                lastData.set(SystemClock.elapsedRealtime())
                if (track.playState != AudioTrack.PLAYSTATE_PLAYING) track.play()
                val total = have + n
                val whole = total / FRAME_BYTES * FRAME_BYTES
                // Never blocks: what doesn't fit is dropped, so a guest clock that runs ahead of the
                // phone's can't build up latency in the stream.
                track.write(buf, 0, whole, AudioTrack.WRITE_NON_BLOCKING)
                have = total - whole
                System.arraycopy(buf, whole, buf, 0, have)
            }
        } finally {
            idle.shutdownNow()
            track.release()
        }
    }

    private companion object {
        const val TAG = "GuestAudio"
        const val RATE = 48_000
        const val FRAME_BYTES = 4

        // About 60 ms: the most sound queued for the speaker.
        const val BUFFER_BYTES = RATE / 1000 * 60 * FRAME_BYTES

        // 10 ms a read.
        const val READ_BYTES = RATE / 100 * FRAME_BYTES
        const val IDLE_MS = 1_000L
        const val MIN_BACKOFF_MS = 500L
        const val MAX_BACKOFF_MS = 10_000L
    }
}
