package dev.nickolaj.pocketnix.gui

import android.hardware.HardwareBuffer

/** The renderer's C half (app/src/main/cpp/gui.c). */
object GuiNative : Unpacker {
    init {
        System.loadLibrary("pocketnix_gui")
    }

    @JvmStatic private external fun zstdDecompress(src: ByteArray, dst: ByteArray, off: Int, len: Int): Int

    /**
     * Copies [rects] (x, y, w, h each; null for the whole buffer) of [src],
     * packed rows [srcWidth] × 4 bytes wide, into [buffer]. Returns 0 or a
     * negative error.
     */
    @JvmStatic external fun copyRects(buffer: HardwareBuffer, src: ByteArray, srcWidth: Int, rects: IntArray?, count: Int): Int

    override fun unpack(src: ByteArray, dst: ByteArray, dstOff: Int, dstLen: Int) {
        if (zstdDecompress(src, dst, dstOff, dstLen) != dstLen) throw GuiProtocolException("zstd data doesn't decompress to $dstLen bytes")
    }
}
