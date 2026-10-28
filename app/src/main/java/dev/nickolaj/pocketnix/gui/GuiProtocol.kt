// The app's side of the GUI protocol; docs/gui-protocol.md is the contract.
package dev.nickolaj.pocketnix.gui

import dev.nickolaj.pocketnix.agent.Frames
import org.json.JSONException
import org.json.JSONObject
import java.io.IOException
import java.io.InputStream
import java.nio.ByteBuffer

/** A violation of the GUI protocol: the connection is closed, and the reconnect replays everything. */
class GuiProtocolException(message: String) : IOException(message)

object Gui {
    const val VERSION = 2
    const val MIN_VERSION = 2

    const val JSON = 0x00
    const val COMMIT = 0x01
    const val COMMIT_DATA = 0x02
    const val ICON = 0x03
    const val PRESENTED = 0x10
    const val POINTER_MOTION = 0x20
    const val POINTER_BUTTON = 0x21
    const val POINTER_AXIS = 0x22
    const val POINTER_LEAVE = 0x23
    const val KEY = 0x30
    const val MODIFIERS = 0x31

    const val FLAG_BUFFER = 1 shl 0
    const val FLAG_FULL = 1 shl 1
    const val FLAG_SCROLL = 1 shl 2
    const val FLAG_HASH = 1 shl 3
    const val FLAG_GROUPED = 1 shl 4
    const val FLAG_HIDDEN = 1 shl 5

    const val FORMAT_RGBA8888 = 1
    const val FORMAT_RGBX8888 = 2
    const val ENCODING_RAW = 0
    const val ENCODING_ZSTD = 1

    const val HEADER_SIZE = 88
    const val MAX_RECTS = 4096
    // The largest buffer, per side and in bytes; the compositor refuses bigger ones (see COMMIT in gui-protocol.md).
    const val MAX_DIMENSION = 16384
    const val MAX_BUFFER_BYTES = 64L shl 20

    const val BTN_LEFT = 0x110
    const val BTN_RIGHT = 0x111
    const val BTN_MIDDLE = 0x112
    const val BTN_SIDE = 0x113
    const val BTN_EXTRA = 0x114

    const val AXIS_WHEEL = 0
    const val AXIS_FINGER = 1
    const val AXIS_CONTINUOUS = 2

    const val MOD_SHIFT = 1 shl 0
    const val MOD_CTRL = 1 shl 1
    const val MOD_ALT = 1 shl 2
    const val MOD_LOGO = 1 shl 3
    const val MOD_CAPS = 1 shl 4
    const val MOD_NUM = 1 shl 5

    // An `icon` request's bounds; the compositor clamps others.
    const val MIN_ICON_SIZE = 16
    const val MAX_ICON_SIZE = 512

    fun hello(app: String, hashes: Boolean): ByteArray = json(
        JSONObject().put("type", "hello").put("version", VERSION).put("minVersion", MIN_VERSION).put("app", app).put("hashes", hashes),
    )

    /** [refreshMilliHz]: the display's refresh rate in mHz. */
    fun output(width: Int, height: Int, scale: Float, refreshMilliHz: Int): ByteArray = json(
        JSONObject().put("type", "output").put("width", width).put("height", height).put("scale", scale.toDouble()).put("refresh", refreshMilliHz),
    )

    fun window(state: WindowState): ByteArray = json(
        JSONObject()
            .put("type", "window")
            .put("id", state.id.toUInt().toLong())
            .put("width", state.width)
            .put("height", state.height)
            .put("scale", state.scale.toDouble())
            .put("focused", state.focused)
            .put("visible", state.visible)
            .put("maximized", state.maximized)
            .put("fullscreen", state.fullscreen),
    )

    /** New windows' size factors: [size] for any entry, [apps] by entry id for those that differ. */
    fun settings(size: Float, apps: Map<String, Float>): ByteArray {
        val o = JSONObject()
        for ((id, f) in apps) o.put(id, JSONObject().put("size", f.toDouble()))
        return json(JSONObject().put("type", "settings").put("size", size.toDouble()).put("apps", o))
    }

    fun close(window: Int): ByteArray = json(JSONObject().put("type", "close").put("id", window.toUInt().toLong()))

    /** Moves [window] into the terminal tab with agent id [tab], or "" to its own task. */
    fun move(window: Int, tab: String): ByteArray = json(JSONObject().put("type", "move").put("id", window.toUInt().toLong()).put("tab", tab))

    /** Asks for icon [key] rendered at [size] × [size] pixels. */
    fun icon(key: String, size: Int): ByteArray = json(JSONObject().put("type", "icon").put("icon", key).put("size", size))

    /** Starts entry [app] (its [action], or "" for none) as launch [id]; [tab] "" gives its windows their own task. */
    fun launch(id: String, app: String, action: String, tab: String): ByteArray =
        json(JSONObject().put("type", "launch").put("id", id).put("app", app).put("action", action).put("tab", tab))

    fun launchCancel(id: String): ByteArray = json(JSONObject().put("type", "launch-cancel").put("id", id))

    fun json(o: JSONObject): ByteArray = Frames.encode(JSON, o.toString().toByteArray(Charsets.UTF_8))

    fun presented(window: Int, seq: Int): ByteArray = binary(PRESENTED, 8) { putInt(window).putInt(seq) }

    fun pointerMotion(window: Int, x: Float, y: Float): ByteArray = binary(POINTER_MOTION, 12) { putInt(window).putFloat(x).putFloat(y) }

    /** [button]: a BTN_* code. */
    fun pointerButton(window: Int, x: Float, y: Float, button: Int, pressed: Boolean): ByteArray =
        binary(POINTER_BUTTON, 17) { putInt(window).putFloat(x).putFloat(y).putInt(button).put(if (pressed) 1 else 0) }

    /** Positive [dx]/[dy] scroll right and down (Wayland's direction); [stop] bit 0 horizontal, bit 1 vertical. */
    fun pointerAxis(window: Int, x: Float, y: Float, source: Int, stop: Int, dx: Float, dy: Float, v120x: Int, v120y: Int): ByteArray =
        binary(POINTER_AXIS, 30) {
            putInt(window).putFloat(x).putFloat(y).put(source.toByte()).put(stop.toByte()).putFloat(dx).putFloat(dy).putInt(v120x).putInt(v120y)
        }

    fun pointerLeave(window: Int): ByteArray = binary(POINTER_LEAVE, 4) { putInt(window) }

    /** [key]: a Linux evdev KEY_* code. */
    fun key(window: Int, key: Int, pressed: Boolean): ByteArray = binary(KEY, 9) { putInt(window).putInt(key).put(if (pressed) 1 else 0) }

    /** [mods]: MOD_* bits. */
    fun modifiers(window: Int, mods: Int): ByteArray = binary(MODIFIERS, 5) { putInt(window).put(mods.toByte()) }

    private inline fun binary(type: Int, size: Int, fill: ByteBuffer.() -> Unit): ByteArray {
        val b = ByteBuffer.allocate(5 + size).put(type.toByte()).putInt(size)
        b.fill()
        check(!b.hasRemaining()) { "frame 0x${type.toString(16)} short of $size bytes" }
        return b.array()
    }
}

/** The Android window's state for a toplevel, as the `window` message carries it. */
data class WindowState(
    val id: Int,
    val width: Int,
    val height: Int,
    val scale: Float,
    val focused: Boolean,
    val visible: Boolean,
    val maximized: Boolean,
    val fullscreen: Boolean = false,
)

/** One surface commit, its COMMIT_DATA frames gathered. Ids are u32s held as Int bit patterns. */
class Commit(
    val flags: Int,
    val surface: Int,
    val window: Int,
    val seq: Int,
    val z: Int,
    val format: Int,
    val encoding: Int,
    val transform: Int,
    val x: Int,
    val y: Int,
    val dstWidth: Int,
    val dstHeight: Int,
    val width: Int,
    val height: Int,
    val srcX: Int,
    val srcY: Int,
    val srcWidth: Int,
    val srcHeight: Int,
    val scroll: Int,
    /** x, y, w, h per rect, in buffer pixels. */
    val rects: IntArray,
    /** The pixel data as sent: raw, or concatenated zstd frames. */
    val data: ByteArray,
    val hash: Long,
) {
    val buffer: Boolean get() = flags and Gui.FLAG_BUFFER != 0
    val full: Boolean get() = flags and Gui.FLAG_FULL != 0
    val scrolled: Boolean get() = flags and Gui.FLAG_SCROLL != 0
    val hashed: Boolean get() = flags and Gui.FLAG_HASH != 0
    val grouped: Boolean get() = flags and Gui.FLAG_GROUPED != 0
    val hidden: Boolean get() = flags and Gui.FLAG_HIDDEN != 0
    val rectCount: Int get() = rects.size / 4

    /** The decompressed data's exact length: every rect's rows, packed. */
    val pixelBytes: Long get() = (0 until rectCount).sumOf { rects[it * 4 + 2].toLong() * rects[it * 4 + 3] * 4 }
}

sealed interface GuiMessage {
    data class Hello(val version: Int, val compositor: String, val instance: String) : GuiMessage
    data class Error(val message: String) : GuiMessage
    data class Toplevel(
        val id: Int,
        val parent: Int,
        val title: String,
        val appId: String,
        val width: Int,
        val height: Int,
        val minWidth: Int,
        val minHeight: Int,
        val maxWidth: Int,
        val maxHeight: Int,
        val decoration: String,
        /** The window it draws in: its own [id] for a window, the topmost ancestor's for a dialog. */
        val window: Int = id,
        /** The app-list entry it belongs to, or "". */
        val app: String = "",
        /** The launch it was bound to when it first mapped, or "". */
        val launch: String = "",
        /** "" for its own task, else the terminal tab it follows. */
        val tab: String = "",
        /** The scale its pixel sizes were computed with; 0 from a compositor that didn't say. */
        val scale: Float = 0f,
        /** Whether its client has a wl_touch. */
        val touch: Boolean = false,
    ) : GuiMessage {
        val isDialog: Boolean get() = window != id
    }
    data class ToplevelRemoved(val id: Int) : GuiMessage
    data class ToplevelRequest(val id: Int, val request: String) : GuiMessage
    /** [toplevel]: the toplevel whose tree it's in (0 from a compositor that didn't say). */
    data class Surface(val id: Int, val window: Int, val role: String, val parent: Int, val toplevel: Int = 0) : GuiMessage
    data class SurfaceRemoved(val id: Int) : GuiMessage
    data class Cursor(val window: Int, val name: String) : GuiMessage
    data object Ready : GuiMessage
    class Apps(val apps: List<AppEntry>) : GuiMessage

    /** An ICON answer: an empty [png] means the guest has no icon there. */
    class Icon(val key: String, val size: Int, val png: ByteArray) : GuiMessage
    data class LaunchStarted(val id: String) : GuiMessage
    data class LaunchFailed(val id: String, val reason: String, val message: String) : GuiMessage

    /** A client activated [window]; [launch] is the launch whose token it was, or "". */
    data class Activate(val window: Int, val launch: String) : GuiMessage

    /** Commits applied atomically: every one but the last had GROUPED set. */
    class Group(val commits: List<Commit>) : GuiMessage
}

/**
 * Reads compositor → app frames into [GuiMessage]s. Unknown frame types,
 * JSON types and fields are skipped; any other violation throws
 * [GuiProtocolException]. Not thread-safe: one reader thread.
 */
class GuiReader(private val input: InputStream) {
    private var lastSeq = 0L

    /** The next message, or null at a clean end of stream. */
    fun next(): GuiMessage? {
        val group = ArrayList<Commit>()
        while (true) {
            val f = Frames.read(input) ?: return null
            when (f.type) {
                Gui.JSON -> {
                    if (group.isNotEmpty()) throw GuiProtocolException("JSON inside a commit group")
                    parseJson(f.payload)?.let { return it }
                }
                Gui.COMMIT -> {
                    val c = readCommit(f.payload)
                    if (group.any { it.surface == c.surface }) throw GuiProtocolException("surface ${c.surface.u} twice in a group")
                    group += c
                    if (!c.grouped) return GuiMessage.Group(group)
                }
                Gui.COMMIT_DATA -> throw GuiProtocolException("COMMIT_DATA outside a commit")
                Gui.ICON -> {
                    if (group.isNotEmpty()) throw GuiProtocolException("ICON inside a commit group")
                    return readIcon(f.payload)
                }
            }
        }
    }

    private fun parseJson(payload: ByteArray): GuiMessage? {
        val o = try {
            JSONObject(String(payload, Charsets.UTF_8))
        } catch (e: JSONException) {
            throw GuiProtocolException("malformed JSON: ${e.message}")
        }
        return try {
            when (o.optString("type")) {
                "hello" -> GuiMessage.Hello(o.getInt("version"), o.optString("compositor"), o.getString("instance"))
                "error" -> GuiMessage.Error(o.optString("message"))
                "toplevel" -> GuiMessage.Toplevel(
                    id = o.id("id"),
                    parent = o.optId("parent"),
                    title = o.optString("title"),
                    appId = o.optString("appId"),
                    width = o.optInt("width"),
                    height = o.optInt("height"),
                    minWidth = o.optInt("minWidth"),
                    minHeight = o.optInt("minHeight"),
                    maxWidth = o.optInt("maxWidth"),
                    maxHeight = o.optInt("maxHeight"),
                    decoration = o.optString("decoration"),
                    window = o.optId("window").takeIf { it != 0 } ?: o.id("id"),
                    app = o.optString("app"),
                    launch = o.optString("launch"),
                    tab = o.optString("tab"),
                    scale = o.optDouble("scale", 0.0).toFloat().takeIf { it.isFinite() && it > 0f } ?: 0f,
                    touch = o.optBoolean("touch"),
                )
                "toplevel-removed" -> GuiMessage.ToplevelRemoved(o.id("id"))
                "toplevel-request" -> GuiMessage.ToplevelRequest(o.id("id"), o.optString("request"))
                "surface" -> GuiMessage.Surface(o.id("id"), o.id("window"), o.optString("role"), o.optId("parent"), o.optId("toplevel"))
                "surface-removed" -> GuiMessage.SurfaceRemoved(o.id("id"))
                "cursor" -> GuiMessage.Cursor(o.optId("window"), o.optString("name"))
                "ready" -> GuiMessage.Ready
                "apps" -> GuiMessage.Apps(AppEntry.parseList(o.getJSONArray("apps")))
                "launch-started" -> GuiMessage.LaunchStarted(o.getString("id"))
                "launch-failed" -> GuiMessage.LaunchFailed(o.getString("id"), o.optString("reason"), o.optString("message"))
                "activate" -> GuiMessage.Activate(o.id("window"), o.optString("launch"))
                else -> null
            }
        } catch (e: JSONException) {
            throw GuiProtocolException("${o.optString("type")}: ${e.message}")
        }
    }

    /** `[size u16][keyLength u8][key][png]`. */
    private fun readIcon(payload: ByteArray): GuiMessage.Icon {
        if (payload.size < 3) throw GuiProtocolException("ICON ${payload.size} bytes")
        val size = ((payload[0].toInt() and 0xff) shl 8) or (payload[1].toInt() and 0xff)
        val keyLength = payload[2].toInt() and 0xff
        if (3 + keyLength > payload.size) throw GuiProtocolException("ICON key past its frame")
        val key = String(payload, 3, keyLength, Charsets.UTF_8)
        if (!validIconKey(key)) throw GuiProtocolException("ICON key '$key'")
        return GuiMessage.Icon(key, size, payload.copyOfRange(3 + keyLength, payload.size))
    }

    private fun readCommit(payload: ByteArray): Commit {
        if (payload.size < Gui.HEADER_SIZE) throw GuiProtocolException("commit header ${payload.size} bytes")
        val b = ByteBuffer.wrap(payload)
        val headerSize = b.getShort(0).toInt() and 0xffff
        val flags = b.getShort(2).toInt() and 0xffff
        val surface = b.getInt(4)
        val window = b.getInt(8)
        val seq = b.getInt(12)
        val z = b.getShort(16).toInt() and 0xffff
        val format = b.get(18).toInt() and 0xff
        val encoding = b.get(19).toInt() and 0xff
        val transform = b.get(20).toInt() and 0xff
        val x = b.getInt(24)
        val y = b.getInt(28)
        val dstWidth = b.u32(32)
        val dstHeight = b.u32(36)
        val width = b.u32(40)
        val height = b.u32(44)
        val srcX = b.u32(48)
        val srcY = b.u32(52)
        val srcWidth = b.u32(56)
        val srcHeight = b.u32(60)
        val scroll = b.getInt(64)
        val rectCount = b.u32(68)
        val dataLength = b.getInt(72).toLong() and 0xffffffffL
        val hash = b.getLong(80)

        val seqU = seq.toLong() and 0xffffffffL
        if (seqU <= lastSeq) throw GuiProtocolException("commit seq $seqU after $lastSeq")
        lastSeq = seqU
        if (headerSize < Gui.HEADER_SIZE) throw GuiProtocolException("commit headerSize $headerSize")
        if (rectCount > Gui.MAX_RECTS) throw GuiProtocolException("commit with $rectCount rects")
        val rectsEnd = headerSize.toLong() + 16L * rectCount
        if (rectsEnd > payload.size) throw GuiProtocolException("commit rects past its frame")
        val buffer = flags and Gui.FLAG_BUFFER != 0
        val hidden = flags and Gui.FLAG_HIDDEN != 0
        if (hidden && buffer) throw GuiProtocolException("commit both HIDDEN and BUFFER")
        if (!buffer && (rectCount != 0 || dataLength != 0L)) throw GuiProtocolException("commit without BUFFER carries data")
        if (!buffer && flags and (Gui.FLAG_FULL or Gui.FLAG_SCROLL) != 0) throw GuiProtocolException("FULL or SCROLL without BUFFER")
        if (flags and Gui.FLAG_FULL != 0 && flags and Gui.FLAG_SCROLL != 0) throw GuiProtocolException("commit both FULL and SCROLL")

        val rects = IntArray(rectCount * 4)
        for (i in rects.indices) rects[i] = b.u32(headerSize + i * 4)
        if (buffer) {
            if (width !in 1..Gui.MAX_DIMENSION || height !in 1..Gui.MAX_DIMENSION || width.toLong() * height * 4 > Gui.MAX_BUFFER_BYTES) {
                throw GuiProtocolException("buffer ${width}x$height")
            }
            if (format != Gui.FORMAT_RGBA8888 && format != Gui.FORMAT_RGBX8888) throw GuiProtocolException("pixel format $format")
            if (encoding != Gui.ENCODING_RAW && encoding != Gui.ENCODING_ZSTD) throw GuiProtocolException("encoding $encoding")
            if (rectCount == 0) throw GuiProtocolException("BUFFER commit without rects")
            for (i in 0 until rectCount) {
                val rx = rects[i * 4].toLong()
                val ry = rects[i * 4 + 1].toLong()
                val rw = rects[i * 4 + 2].toLong()
                val rh = rects[i * 4 + 3].toLong()
                if (rx + rw > width || ry + rh > height) throw GuiProtocolException("rect $rx,$ry ${rw}x$rh outside ${width}x$height")
            }
            if (flags and Gui.FLAG_FULL != 0 && (rectCount != 1 || rects[0] != 0 || rects[1] != 0 || rects[2] != width || rects[3] != height)) {
                throw GuiProtocolException("FULL commit whose rect isn't the buffer")
            }
        }

        var pixelBytes = 0L
        for (i in 0 until rectCount) pixelBytes += rects[i * 4 + 2].toLong() * rects[i * 4 + 3] * 4
        // Rects may overlap, but the compositor keeps their sum within the buffer's size.
        if (pixelBytes > Gui.MAX_BUFFER_BYTES) throw GuiProtocolException("commit $seqU with $pixelBytes pixel bytes")
        val most = when {
            !buffer -> 0L
            encoding == Gui.ENCODING_RAW -> pixelBytes
            // zstd's worst-case expansion, with room for many small frames.
            else -> pixelBytes + pixelBytes / 128 + (1L shl 20)
        }
        if (encoding == Gui.ENCODING_RAW && dataLength != most || dataLength > most) {
            throw GuiProtocolException("commit $seqU has $dataLength data bytes for $pixelBytes pixel bytes")
        }
        val data = ByteArray(dataLength.toInt())
        val first = (payload.size - rectsEnd).toInt().coerceAtMost(data.size)
        System.arraycopy(payload, rectsEnd.toInt(), data, 0, first)
        var have = first
        while (have < data.size) {
            val f = Frames.read(input) ?: throw GuiProtocolException("stream ended inside commit $seqU")
            if (f.type != Gui.COMMIT_DATA) throw GuiProtocolException("frame 0x${f.type.toString(16)} inside commit $seqU")
            if (f.payload.size < 4) throw GuiProtocolException("COMMIT_DATA ${f.payload.size} bytes")
            val dseq = ByteBuffer.wrap(f.payload).getInt(0)
            if (dseq != seq) throw GuiProtocolException("COMMIT_DATA for ${dseq.u} inside commit $seqU")
            val n = f.payload.size - 4
            if (n > data.size - have) throw GuiProtocolException("COMMIT_DATA past commit $seqU's dataLength")
            System.arraycopy(f.payload, 4, data, have, n)
            have += n
        }

        return Commit(
            flags, surface, window, seq, z, format, encoding, transform, x, y, dstWidth, dstHeight, width, height,
            srcX, srcY, srcWidth, srcHeight, scroll, rects, data, hash,
        )
    }
}

private fun ByteBuffer.u32(index: Int): Int {
    val v = getInt(index)
    if (v < 0) throw GuiProtocolException("field at $index out of range: ${v.toLong() and 0xffffffffL}")
    return v
}

private fun JSONObject.id(key: String): Int {
    val v = getLong(key)
    if (v !in 1..0xffffffffL) throw GuiProtocolException("$key $v")
    return v.toInt()
}

private fun JSONObject.optId(key: String): Int {
    val v = optLong(key)
    if (v !in 0..0xffffffffL) throw GuiProtocolException("$key $v")
    return v.toInt()
}

/** A u32 id for logs. */
internal val Int.u: Long get() = toLong() and 0xffffffffL
