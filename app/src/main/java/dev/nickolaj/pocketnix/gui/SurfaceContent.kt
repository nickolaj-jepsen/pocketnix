package dev.nickolaj.pocketnix.gui

import java.nio.ByteBuffer
import java.nio.ByteOrder

fun interface Unpacker {
    /** Decompresses [src] (concatenated zstd frames) into exactly [dstLen] bytes of [dst] at [dstOff], or throws. */
    fun unpack(src: ByteArray, dst: ByteArray, dstOff: Int, dstLen: Int)

    /** Copies the first [len] bytes of the guest pages [src] lists (a PAGES commit's data) into [dst], or throws. */
    fun guestPages(src: ByteArray, dst: ByteArray, len: Int): Unit = throw GuiProtocolException("a PAGES commit without guest RAM")
}

/**
 * Pixel arrays a surface's content let go of, for the next commit's data: a
 * fresh full-frame array costs its page faults on first touch and the GC's
 * work, every frame. Thread-safe.
 */
object SpareArrays {
    // One spare per surface that redraws whole; more only holds memory.
    private const val KEEP = 2
    private val spare = ArrayDeque<ByteArray>()

    /** An array of [size] bytes, with any content. */
    @Synchronized
    fun take(size: Int): ByteArray {
        val i = spare.indexOfFirst { it.size == size }
        return if (i >= 0) spare.removeAt(i) else ByteArray(size)
    }

    /** Hands back [a], which nothing may use again. */
    @Synchronized
    fun give(a: ByteArray) {
        if (spare.size == KEEP) spare.removeFirst()
        spare.addLast(a)
    }
}

/**
 * A surface's reference copy (gui-protocol.md, Applying a commit): its
 * content after every commit so far, `width × 4` bytes per row in wire byte
 * order. Buffers are filled from it; content hashes are computed over it.
 */
class SurfaceContent {
    var width = 0
        private set
    var height = 0
        private set
    var format = 0
        private set
    var pixels: ByteArray? = null
        private set

    fun clear() {
        pixels = null
        width = 0
        height = 0
        format = 0
    }

    /**
     * Applies a BUFFER commit, which must be [unpacked]; throws
     * [GuiProtocolException] for one that doesn't fit the content. A FULL
     * commit's data becomes the content, and the content it replaces goes to
     * [SpareArrays].
     */
    fun apply(c: Commit) {
        require(c.buffer) { "commit without BUFFER" }
        require(c.encoding == Gui.ENCODING_RAW) { "commit ${c.seq.u} isn't unpacked" }
        if (c.full) {
            pixels?.takeIf { it !== c.data }?.let(SpareArrays::give)
            pixels = c.data
            width = c.width
            height = c.height
            format = c.format
            return
        }
        val p = pixels ?: throw GuiProtocolException("partial commit ${c.seq.u} for surface ${c.surface.u} without content")
        if (c.width != width || c.height != height || c.format != format) {
            throw GuiProtocolException("partial commit ${c.seq.u} for a ${c.width}x${c.height} format ${c.format} buffer onto ${width}x$height format $format")
        }
        if (c.scrolled) scroll(p, c.scroll)
        val stride = width * 4
        var off = 0
        for (i in 0 until c.rectCount) {
            val rx = c.rects[i * 4]
            val ry = c.rects[i * 4 + 1]
            val row = c.rects[i * 4 + 2] * 4
            for (y in ry until ry + c.rects[i * 4 + 3]) {
                System.arraycopy(c.data, off, p, y * stride + rx * 4, row)
                off += row
            }
        }
    }

    /** Row y of the new content is row y + [by] of the old. */
    private fun scroll(p: ByteArray, by: Int) {
        if (by == 0 || by >= height || by <= -height) return
        val stride = width * 4
        if (by > 0) {
            System.arraycopy(p, by * stride, p, 0, (height - by) * stride)
        } else {
            System.arraycopy(p, 0, p, -by * stride, (height + by) * stride)
        }
    }

    fun hash(): Long = pixels?.let { contentHash(it, 0, it.size) } ?: 0L
}

/**
 * The commit with its pixel data as raw bytes, whatever its encoding: the
 * one place that knows them. Throws [GuiProtocolException] for data that
 * doesn't decode. A PAGES commit's slot goes back to the compositor through
 * [released] once its pages are copied out. It reads no surface state, so
 * the slow part of applying a commit can run outside any lock.
 */
fun Commit.unpacked(unpacker: Unpacker, released: (seq: Int) -> Unit): Commit {
    if (!buffer || encoding == Gui.ENCODING_RAW) return this
    val out = SpareArrays.take(pixelBytes.toInt())
    when (encoding) {
        Gui.ENCODING_PAGES -> {
            try {
                unpacker.guestPages(data, out, pixelBytes.toInt())
            } catch (e: GuiProtocolException) {
                throw GuiProtocolException("commit ${seq.u}: ${e.message}")
            }
            released(seq)
        }

        else -> try {
            unpacker.unpack(data, out, 0, pixelBytes.toInt())
        } catch (e: GuiProtocolException) {
            throw e
        } catch (e: Exception) {
            throw GuiProtocolException("commit ${seq.u}: ${e.message}")
        }
    }
    return Commit(
        flags, surface, window, seq, z, format, Gui.ENCODING_RAW, transform, x, y, dstWidth, dstHeight, width, height,
        srcX, srcY, srcWidth, srcHeight, scroll, rects, out, hash
    )
}

/** gui-protocol.md's Content hash, over packed rows. */
fun contentHash(b: ByteArray, off: Int, len: Int): Long {
    val prime = 0x100000001b3L
    var h = -0x340d631b7bdddcdbL // 0xcbf29ce484222325
    val le = ByteBuffer.wrap(b).order(ByteOrder.LITTLE_ENDIAN)
    val chunks = off + len / 8 * 8
    var i = off
    while (i < chunks) {
        h = java.lang.Long.rotateLeft((h xor le.getLong(i)) * prime, 29)
        i += 8
    }
    while (i < off + len) {
        h = (h xor (b[i].toLong() and 0xff)) * prime
        i++
    }
    return h
}

/**
 * What changed in a surface's recent content versions, so a buffer that
 * holds an older version gets everything since (gui-protocol.md, Buffer
 * rotation). Version [serial] is the current content; 0 is none.
 */
class DamageHistory(private val capacity: Int = 16) {
    var serial = 0L
        private set
    private val ring = arrayOfNulls<IntArray>(capacity)

    /** A new content version that changed [rects] (x, y, w, h each), or the whole buffer for null. */
    fun record(rects: IntArray?) {
        serial++
        ring[(serial % capacity).toInt()] = rects ?: WHOLE
    }

    /**
     * The rects changed after version [since]: empty when it's current, null
     * when it's the whole buffer (a whole-buffer change in between, a buffer
     * never filled (`since` < 1), or older than the history reaches).
     */
    fun since(since: Long): IntArray? {
        if (since == serial) return IntArray(0)
        if (since < 1 || since > serial || serial - since >= capacity) return null
        var n = 0
        for (v in since + 1..serial) {
            val r = ring[(v % capacity).toInt()]!!
            if (r === WHOLE) return null
            n += r.size
        }
        val out = IntArray(n)
        var o = 0
        for (v in since + 1..serial) {
            val r = ring[(v % capacity).toInt()]!!
            System.arraycopy(r, 0, out, o, r.size)
            o += r.size
        }
        return if (out.size / 4 > MAX_RECTS) bounds(out) else out
    }

    private companion object {
        val WHOLE = IntArray(0)

        // Past this, one bounding box copies less overhead than many small rects.
        const val MAX_RECTS = 64

        fun bounds(r: IntArray): IntArray {
            var l = Int.MAX_VALUE
            var t = Int.MAX_VALUE
            var right = 0
            var bottom = 0
            for (i in 0 until r.size / 4) {
                l = minOf(l, r[i * 4])
                t = minOf(t, r[i * 4 + 1])
                right = maxOf(right, r[i * 4] + r[i * 4 + 2])
                bottom = maxOf(bottom, r[i * 4 + 1] + r[i * 4 + 3])
            }
            return intArrayOf(l, t, right - l, bottom - t)
        }
    }
}
