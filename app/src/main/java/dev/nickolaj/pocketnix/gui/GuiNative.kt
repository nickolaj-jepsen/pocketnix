package dev.nickolaj.pocketnix.gui

import android.hardware.HardwareBuffer
import android.os.Process
import java.util.concurrent.Callable
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.Executors

/** The renderer's C half (app/src/main/cpp/gui.c). */
object GuiNative : Unpacker {
    init {
        System.loadLibrary("pocketnix_gui")
    }

    // Threads besides the caller's that decode a commit's zstd frames; the compositor splits large data into up to four.
    private const val UNPACK_THREADS = 3

    private val poolTids = ConcurrentHashMap.newKeySet<Int>()

    private val pool = Executors.newFixedThreadPool(UNPACK_THREADS) { r ->
        Thread({
            poolTids += Process.myTid()
            r.run()
        }, "gui-unpack").apply { isDaemon = true }
    }

    /** The tids of the threads that decode beside the caller, of those started so far: they belong in its hint session. */
    fun unpackTids(): IntArray = poolTids.toIntArray().apply { sort() }

    @JvmStatic private external fun zstdDecompress(src: ByteArray, srcOff: Int, srcLen: Int, dst: ByteArray, off: Int, len: Int): Int

    /**
     * [src]'s zstd frames: the offset each ends at and its decompressed size,
     * interleaved. Null when a frame doesn't state its size.
     */
    @JvmStatic private external fun zstdFrames(src: ByteArray): LongArray?

    /**
     * Copies [rects] (x, y, w, h each; null for the whole buffer) of [src],
     * packed rows [srcWidth] × 4 bytes wide, into [buffer]. Returns 0 or a
     * negative error.
     */
    @JvmStatic external fun copyRects(buffer: HardwareBuffer, src: ByteArray, srcWidth: Int, rects: IntArray?, count: Int): Int

    /** Decodes [src]'s frames side by side, each into its own part of [dst]. */
    override fun unpack(src: ByteArray, dst: ByteArray, dstOff: Int, dstLen: Int) {
        val frames = zstdFrames(src)
        if (frames == null || frames.size <= 2) {
            if (zstdDecompress(src, 0, src.size, dst, dstOff, dstLen) != dstLen) throw GuiProtocolException("zstd data doesn't decompress to $dstLen bytes")
            return
        }
        val total = (1 until frames.size step 2).sumOf { frames[it] }
        if (total != dstLen.toLong()) throw GuiProtocolException("zstd data decompresses to $total bytes, not $dstLen")
        val parts = ArrayList<() -> Boolean>()
        var srcAt = 0
        var dstAt = dstOff
        for (i in 0 until frames.size / 2) {
            val (s, d) = srcAt to dstAt
            val srcLen = frames[2 * i].toInt() - s
            val len = frames[2 * i + 1].toInt()
            parts += { zstdDecompress(src, s, srcLen, dst, d, len) == len }
            srcAt += srcLen
            dstAt += len
        }
        val others = parts.drop(1).map { pool.submit(Callable(it)) }
        val mine = parts[0]()
        // Every part must finish before dst is used, failed or not.
        val ok = others.map { f -> runCatching { f.get() }.getOrDefault(false) }.all { it } && mine
        if (!ok) throw GuiProtocolException("zstd data doesn't decompress")
    }
}
