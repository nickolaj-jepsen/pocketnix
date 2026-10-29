package dev.nickolaj.pocketnix.agent

import java.io.EOFException
import java.io.FilterInputStream
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import org.json.JSONObject

/** One file under the file provider's root (go/internal/protocol FileInfo). */
data class GuestFile(
    /** Relative to the root, slash-separated; "" is the root. */
    val path: String,
    val name: String,
    val dir: Boolean,
    val size: Long,
    /** Unix milliseconds. */
    val mtime: Long,
    val writable: Boolean
) {
    companion object {
        fun parse(o: JSONObject) = GuestFile(
            path = o.optString("path"),
            name = o.optString("name"),
            dir = o.optBoolean("dir"),
            size = o.optLong("size"),
            mtime = o.optLong("mtime"),
            writable = o.optBoolean("writable")
        )
    }
}

/**
 * The agent's file-provider and share streams: one connection per
 * operation. Calls block; [AgentException] means the agent refused.
 */
class GuestFiles(private val dialer: AgentDialer, private val appVersion: String) {
    private fun open(op: String, path: String, extra: JSONObject.() -> Unit = {}) = dialer.open(
        Protocol.hello("file", appVersion) {
            put("op", op)
            put("path", path)
            extra()
        }
    )

    private fun simple(op: String, path: String, extra: JSONObject.() -> Unit = {}): JSONObject {
        val (s, hello) = open(op, path, extra)
        s.close()
        return hello
    }

    private fun JSONObject.file(): GuestFile = GuestFile.parse(optJSONObject("file") ?: throw IOException("no file in the answer"))

    fun stat(path: String): GuestFile = simple("stat", path).file()

    fun list(path: String): List<GuestFile> {
        val (s, hello) = open("list", path)
        return s.use { (0 until hello.optLong("size")).map { GuestFile.parse(JSONObject(Protocol.readLine(s.input))) } }
    }

    /** The file's bytes; closing the stream ends the operation. */
    fun read(path: String): InputStream {
        val (s, hello) = open("read", path)
        val size = hello.optLong("size")
        return object : FilterInputStream(LimitedInput(s.input, size)) {
            override fun close() = s.close()
        }
    }

    /**
     * Replaces the file with the [size] bytes written to the returned stream;
     * closing it commits, throwing when the agent couldn't. Fewer bytes leave
     * the old file as it was.
     */
    fun write(path: String, size: Long): OutputStream {
        val (s, _) = open("write", path) { put("size", size) }
        return object : OutputStream() {
            override fun write(b: Int) = s.output.write(b)
            override fun write(b: ByteArray, off: Int, len: Int) = s.output.write(b, off, len)
            override fun flush() = s.output.flush()
            override fun close() {
                s.use {
                    s.output.flush()
                    s.shutdownOutput()
                    done(s)
                }
            }
        }
    }

    /**
     * Opens [path] for ranged access in an Android open [mode] ("r", "w",
     * "wa", "rw", …), or null when the agent predates it.
     */
    fun openRanged(path: String, mode: String): RangedFile? {
        val (s, hello) = try {
            open("open", path) { put("mode", mode) }
        } catch (e: AgentException) {
            if (e.message.orEmpty().startsWith("unknown file operation")) return null
            throw e
        }
        return RangedFile(s, hello.optJSONObject("file")?.optLong("size") ?: 0, mode)
    }

    fun create(dir: String, name: String, isDir: Boolean): GuestFile = simple("create", dir) {
        put("name", name)
        put("dir", isDir)
    }.file()

    fun delete(path: String) {
        simple("delete", path)
    }

    fun rename(path: String, name: String): GuestFile = simple("rename", path) { put("name", name) }.file()

    /**
     * Shares [size] bytes from [input] as [name]; returns where it landed in
     * the guest. With [ref], they answer that guest request (a pick's file, a
     * paste's data) instead.
     */
    fun share(name: String, size: Long, input: InputStream, ref: String? = null): String {
        val (s, _) = dialer.open(
            Protocol.hello("share", appVersion) {
                put("name", name)
                put("size", size)
                if (ref != null) put("ref", ref)
            }
        )
        return s.use {
            val copied = input.copyTo(s.output, COPY_BUFFER)
            if (copied != size) throw IOException("read $copied of $size bytes")
            s.output.flush()
            s.shutdownOutput()
            done(s)
        }
    }

    /**
     * Takes the bytes of the guest request [ref] (a save, a typed copy),
     * waiting until the guest has them, and hands [store] their name, size
     * and bytes, which it must read to the end. The agent learns whether
     * [store] succeeded; its exception is rethrown.
     */
    fun fetch(ref: String, store: (name: String, size: Long, input: InputStream) -> Unit) {
        val (s, hello) = dialer.open(Protocol.hello("save", appVersion) { put("ref", ref) })
        s.use {
            val size = hello.optLong("size")
            val input = LimitedInput(s.input, size)
            try {
                store(hello.optString("name"), size, input)
                if (input.read() >= 0) throw IOException("the store read fewer than $size bytes")
            } catch (e: Exception) {
                runCatching { Protocol.writeJson(s.output, JSONObject().put("type", "error").put("message", e.message ?: e.toString())) }
                throw e
            }
            Protocol.writeJson(s.output, JSONObject().put("type", "done"))
        }
    }

    /** Reads a stream's closing Done (its path) or Error. */
    private fun done(s: AgentStream): String {
        val msg = JSONObject(Protocol.readLine(s.input))
        if (msg.optString("type") == "error") throw AgentException(msg.optString("message"))
        return msg.optString("path")
    }

    companion object {
        /** For copies to and from file streams: vsock moves big writes far cheaper than copyTo's 8 KiB. */
        const val COPY_BUFFER = 64 * 1024
    }
}

/** The [left] bytes the agent announced; a stream that ends sooner is an error, not a short file. */
internal class LimitedInput(input: InputStream, private var left: Long) : FilterInputStream(input) {
    override fun read(): Int {
        if (left <= 0) return -1
        val b = super.read()
        if (b < 0) throw truncated()
        left--
        return b
    }

    override fun read(b: ByteArray, off: Int, len: Int): Int {
        if (left <= 0) return -1
        if (len == 0) return 0
        val n = super.read(b, off, minOf(len.toLong(), left).toInt())
        if (n < 0) throw truncated()
        left -= n
        return n
    }

    private fun truncated() = EOFException("the guest's file stream ended $left bytes early")
}
