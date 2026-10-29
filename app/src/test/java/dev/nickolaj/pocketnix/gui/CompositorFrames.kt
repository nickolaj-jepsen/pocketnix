package dev.nickolaj.pocketnix.gui

import dev.nickolaj.pocketnix.agent.Frames
import io.airlift.compress.zstd.ZstdCompressor
import java.io.ByteArrayOutputStream
import java.nio.ByteBuffer
import org.json.JSONObject

/** Compositor → app frames, written the way docs/gui-protocol.md specifies them. */
class CompositorFrames {
    val out = ByteArrayOutputStream()
    private var seq = 0

    fun bytes(): ByteArray = out.toByteArray()

    fun json(o: JSONObject) = apply { out.write(Frames.encode(Gui.JSON, o.toString().toByteArray())) }

    fun raw(type: Int, payload: ByteArray) = apply { out.write(Frames.encode(type, payload)) }

    fun hello(instance: String = "inst-1") = json(JSONObject().put("type", "hello").put("version", 2).put("compositor", "test").put("instance", instance))

    /** A window, or with [parent] a dialog drawn in [window]. */
    fun toplevel(id: Int, title: String = "", window: Int = id, parent: Int = 0, app: String = "", launch: String = "") = json(
        JSONObject().put("type", "toplevel").put("id", id).put("window", window).put("parent", parent).put("title", title)
            .put("appId", "test").put("app", app).put("launch", launch).put("tab", "")
            .put("width", 0).put("height", 0).put("decoration", "client")
    )

    fun surface(id: Int, window: Int, role: String = "toplevel", parent: Int = 0, toplevel: Int = if (role == "toplevel") id else window) = json(
        JSONObject().put("type", "surface").put("id", id).put("window", window).put("toplevel", toplevel).put("role", role).put("parent", parent)
    )

    fun ready() = json(JSONObject().put("type", "ready"))

    /**
     * One COMMIT and its COMMIT_DATA frames. [pixels] are the packed rect
     * rows; [split] is the most data bytes per frame, to exercise
     * COMMIT_DATA.
     */
    fun commit(
        surface: Int,
        window: Int,
        width: Int,
        height: Int,
        rects: List<IntArray>,
        pixels: ByteArray,
        flags: Int = Gui.FLAG_BUFFER,
        zstd: Boolean = false,
        scroll: Int = 0,
        hash: Long? = null,
        x: Int = 0,
        y: Int = 0,
        z: Int = 0,
        split: Int = Frames.MAX_PAYLOAD - 4,
        seq: Int = ++this.seq,
        headerSize: Int = Gui.HEADER_SIZE,
        /** The data as sent, when it isn't [pixels] compressed in one go. */
        encoded: ByteArray? = null
    ) = apply {
        val data = encoded ?: if (zstd) compress(pixels) else pixels
        val f = flags or (if (hash != null) Gui.FLAG_HASH else 0)
        val head = ByteBuffer.allocate(headerSize + 16 * rects.size)
            .putShort(headerSize.toShort()).putShort(f.toShort()).putInt(surface).putInt(window).putInt(seq)
            .putShort(z.toShort()).put(Gui.FORMAT_RGBA8888.toByte()).put((if (zstd) 1 else 0).toByte()).put(0).put(0).putShort(0)
            .putInt(x).putInt(y).putInt(width).putInt(height).putInt(width).putInt(height)
            .putInt(0).putInt(0).putInt(width).putInt(height)
            .putInt(scroll).putInt(rects.size).putInt(data.size).putInt(0).putLong(hash ?: 0)
        head.position(headerSize)
        for (r in rects) for (v in r) head.putInt(v)
        val first = minOf(data.size, Frames.MAX_PAYLOAD - head.capacity(), split)
        out.write(Frames.encode(Gui.COMMIT, head.array() + data.copyOfRange(0, first)))
        var off = first
        while (off < data.size) {
            val n = minOf(split, data.size - off)
            out.write(Frames.encode(Gui.COMMIT_DATA, ByteBuffer.allocate(4 + n).putInt(seq).put(data, off, n).array()))
            off += n
        }
    }

    /** A FULL commit of [content] (width × height × 4 bytes). */
    fun full(surface: Int, window: Int, width: Int, height: Int, content: ByteArray, extra: Int = 0, zstd: Boolean = false) = commit(surface, window, width, height, listOf(intArrayOf(0, 0, width, height)), content, Gui.FLAG_BUFFER or Gui.FLAG_FULL or extra, zstd)

    companion object {
        fun compress(b: ByteArray): ByteArray {
            val c = ZstdCompressor()
            val out = ByteArray(c.maxCompressedLength(b.size))
            val n = c.compress(b, 0, b.size, out, 0, out.size)
            return out.copyOf(n)
        }
    }
}

/** aircompressor's pure-Java zstd, standing in for the app's native one. */
object JavaUnpacker : Unpacker {
    override fun unpack(src: ByteArray, dst: ByteArray, dstOff: Int, dstLen: Int) {
        val n = io.airlift.compress.zstd.ZstdDecompressor().decompress(src, 0, src.size, dst, dstOff, dstLen)
        if (n != dstLen) throw GuiProtocolException("zstd data decompressed to $n bytes, not $dstLen")
    }
}
