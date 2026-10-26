package dev.nickolaj.pocketnix.agent

import org.json.JSONObject
import java.io.FilterInputStream
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream

/** One file under the file provider's root (go/internal/protocol FileInfo). */
data class GuestFile(
    /** Relative to the root, slash-separated; "" is the root. */
    val path: String,
    val name: String,
    val dir: Boolean,
    val size: Long,
    /** Unix milliseconds. */
    val mtime: Long,
    val writable: Boolean,
) {
    companion object {
        fun parse(o: JSONObject) = GuestFile(
            path = o.optString("path"),
            name = o.optString("name"),
            dir = o.optBoolean("dir"),
            size = o.optLong("size"),
            mtime = o.optLong("mtime"),
            writable = o.optBoolean("writable"),
        )
    }
}

/**
 * The agent's file-provider and share streams: one connection per
 * operation. Calls block; [AgentException] means the agent refused.
 */
class GuestFiles(private val dialer: AgentDialer, private val appVersion: String) {
    private fun open(op: String, path: String, extra: JSONObject.() -> Unit = {}) =
        dialer.open(Protocol.hello("file", appVersion) { put("op", op); put("path", path); extra() })

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
     * Replaces the file with what's written to the returned stream; closing it
     * commits, throwing when the agent couldn't.
     */
    fun write(path: String): OutputStream {
        val (s, _) = open("write", path)
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

    fun create(dir: String, name: String, isDir: Boolean): GuestFile =
        simple("create", dir) { put("name", name); put("dir", isDir) }.file()

    fun delete(path: String) {
        simple("delete", path)
    }

    fun rename(path: String, name: String): GuestFile = simple("rename", path) { put("name", name) }.file()

    /** Shares [size] bytes from [input] as [name]; returns where it landed in the guest. */
    fun share(name: String, size: Long, input: InputStream): String {
        val (s, _) = dialer.open(Protocol.hello("share", appVersion) { put("name", name); put("size", size) })
        return s.use {
            val copied = input.copyTo(s.output)
            if (copied != size) throw IOException("read $copied of $size bytes")
            s.output.flush()
            s.shutdownOutput()
            done(s)
        }
    }

    /** Reads a stream's closing Done (its path) or Error. */
    private fun done(s: AgentStream): String {
        val msg = JSONObject(Protocol.readLine(s.input))
        if (msg.optString("type") == "error") throw AgentException(msg.optString("message"))
        return msg.optString("path")
    }
}

private class LimitedInput(input: InputStream, private var left: Long) : FilterInputStream(input) {
    override fun read(): Int {
        if (left <= 0) return -1
        return super.read().also { if (it >= 0) left-- }
    }

    override fun read(b: ByteArray, off: Int, len: Int): Int {
        if (left <= 0) return -1
        val n = super.read(b, off, minOf(len.toLong(), left).toInt())
        if (n > 0) left -= n
        return n
    }
}
