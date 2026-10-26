package dev.nickolaj.pocketnix.agent

import java.io.EOFException
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.nio.ByteBuffer

/** PTY and FileOpen stream frames: [type u8][length u32 BE][payload]. */
object Frames {
    const val DATA: Int = 0
    const val RESIZE: Int = 1
    const val EXIT: Int = 2
    // FileOpen streams (go/internal/protocol FileFrame*).
    const val FILE_READ: Int = 0x10
    const val FILE_WRITE: Int = 0x11
    const val FILE_SYNC: Int = 0x12
    const val FILE_COMMIT: Int = 0x13
    const val FILE_OK: Int = 0x20
    const val FILE_ERROR: Int = 0x21
    const val MAX_PAYLOAD = 1 shl 20

    class Frame(val type: Int, val payload: ByteArray)

    /** Encodes one frame; write it with a single write so frames don't interleave. */
    fun encode(type: Int, payload: ByteArray, offset: Int = 0, count: Int = payload.size): ByteArray {
        require(count <= MAX_PAYLOAD) { "frame payload $count bytes" }
        return ByteBuffer.allocate(5 + count).put(type.toByte()).putInt(count).put(payload, offset, count).array()
    }

    fun write(out: OutputStream, type: Int, payload: ByteArray, offset: Int = 0, count: Int = payload.size) {
        out.write(encode(type, payload, offset, count))
        out.flush()
    }

    /** Reads one frame, or null at a clean end of stream. */
    fun read(input: InputStream): Frame? {
        val type = input.read()
        if (type < 0) return null
        val header = ByteArray(4)
        readFully(input, header)
        val n = ByteBuffer.wrap(header).int
        if (n < 0 || n > MAX_PAYLOAD) throw IOException("frame length $n")
        val payload = ByteArray(n)
        readFully(input, payload)
        return Frame(type, payload)
    }

    internal fun readFully(input: InputStream, b: ByteArray, count: Int = b.size) {
        var off = 0
        while (off < count) {
            val r = input.read(b, off, count - off)
            if (r < 0) throw IOException("stream ended inside a frame", EOFException())
            off += r
        }
    }

    fun resize(rows: Int, cols: Int, xPixels: Int = 0, yPixels: Int = 0): ByteArray =
        ByteBuffer.allocate(8)
            .putShort(rows.coerceIn(0, 0xffff).toShort())
            .putShort(cols.coerceIn(0, 0xffff).toShort())
            .putShort(xPixels.coerceIn(0, 0xffff).toShort())
            .putShort(yPixels.coerceIn(0, 0xffff).toShort())
            .array()

    /** The exit status in an EXIT payload: the code, or minus the signal. */
    fun exitStatus(payload: ByteArray): Int {
        if (payload.size != 4) throw IOException("exit payload ${payload.size} bytes")
        return ByteBuffer.wrap(payload).int
    }
}

/** UDP stream datagrams: [length u16 BE][payload], each way. */
object Datagrams {
    const val MAX_PAYLOAD = 65507

    /** Encodes one datagram; write it with a single write so datagrams don't interleave. */
    fun encode(payload: ByteArray, offset: Int = 0, count: Int = payload.size): ByteArray {
        require(count <= MAX_PAYLOAD) { "datagram $count bytes" }
        return ByteBuffer.allocate(2 + count).putShort(count.toShort()).put(payload, offset, count).array()
    }

    /** Reads one datagram into [buf] (at least [MAX_PAYLOAD] bytes); its length, or -1 at a clean end of stream. */
    fun read(input: InputStream, buf: ByteArray): Int {
        val hi = input.read()
        if (hi < 0) return -1
        val lo = input.read()
        if (lo < 0) throw IOException("stream ended inside a datagram", EOFException())
        val n = (hi shl 8) or lo
        if (n > MAX_PAYLOAD || n > buf.size) throw IOException("datagram length $n")
        Frames.readFully(input, buf, n)
        return n
    }
}
