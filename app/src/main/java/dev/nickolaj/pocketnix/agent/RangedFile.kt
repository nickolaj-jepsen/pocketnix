package dev.nickolaj.pocketnix.agent

import java.io.Closeable
import java.io.IOException
import java.nio.ByteBuffer

/**
 * A FileOpen stream (go/internal/protocol): ranged reads of one guest file,
 * and writes to a copy the agent puts in place on [commit]. Not thread-safe;
 * [close] without a commit discards the writes.
 */
class RangedFile internal constructor(
    private val s: AgentStream,
    /** The size as opened (0 for a truncating mode). */
    size: Long,
    private val mode: String,
) : Closeable {
    var size: Long = size
        private set

    /** Whether a commit would change the file: a write, or a truncating open. */
    val changed: Boolean get() = wrote || truncates

    private val truncates = mode == "w" || mode.contains('t')
    private val appends = mode.contains('a')
    private var wrote = false

    // Read-ahead: readers through a proxy fd ask in pages, each a round trip.
    private var cache = ByteArray(0)
    private var cacheAt = 0L

    /** Reads up to [len] bytes at [offset] into [b]; 0 at or past the end. */
    fun read(offset: Long, b: ByteArray, off: Int, len: Int): Int {
        if (len == 0 || offset >= size) return 0
        if (offset < cacheAt || offset >= cacheAt + cache.size) {
            val want = maxOf(len, READ_AHEAD).coerceAtMost(Frames.MAX_PAYLOAD)
            cache = call(Frames.FILE_READ, ByteBuffer.allocate(12).putLong(offset).putInt(want).array())
            cacheAt = offset
            if (cache.isEmpty()) return 0
        }
        val start = (offset - cacheAt).toInt()
        val n = minOf(len, cache.size - start)
        System.arraycopy(cache, start, b, off, n)
        return n
    }

    /** Writes [len] bytes of [b] at [offset] (at the end, for an appending mode); returns the count. */
    fun write(offset: Long, b: ByteArray, off: Int, len: Int): Int {
        val at = if (appends) size else offset
        var done = 0
        while (done < len) {
            val n = minOf(len - done, Frames.MAX_PAYLOAD - 8)
            call(Frames.FILE_WRITE, ByteBuffer.allocate(8 + n).putLong(at + done).put(b, off + done, n).array())
            done += n
        }
        wrote = true
        size = maxOf(size, at + len)
        cache = ByteArray(0)
        return len
    }

    fun sync() {
        call(Frames.FILE_SYNC, ByteArray(0))
    }

    /** Puts the written file in place and ends the stream; throws when the agent couldn't. */
    fun commit() {
        s.use { call(Frames.FILE_COMMIT, ByteArray(0)) }
    }

    override fun close() = s.close()

    private fun call(type: Int, payload: ByteArray): ByteArray {
        synchronized(s) { Frames.write(s.output, type, payload) }
        val f = Frames.read(s.input) ?: throw IOException("the guest closed the file")
        return when (f.type) {
            Frames.FILE_OK -> f.payload
            Frames.FILE_ERROR -> throw AgentException(f.payload.decodeToString())
            else -> throw IOException("unexpected file frame ${f.type}")
        }
    }

    private companion object {
        const val READ_AHEAD = 256 * 1024
    }
}
