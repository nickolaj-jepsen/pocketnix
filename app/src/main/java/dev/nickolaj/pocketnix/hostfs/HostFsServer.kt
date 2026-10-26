package dev.nickolaj.pocketnix.hostfs

import android.util.Log
import dev.nickolaj.pocketnix.agent.AgentStream
import dev.nickolaj.pocketnix.agent.Frames
import dev.nickolaj.pocketnix.agent.GuestFile
import org.json.JSONArray
import org.json.JSONObject
import java.io.Closeable
import java.io.FileNotFoundException
import java.io.IOException
import java.nio.ByteBuffer
import java.nio.file.AccessDeniedException
import java.nio.file.DirectoryNotEmptyException
import java.nio.file.FileAlreadyExistsException
import java.nio.file.NoSuchFileException
import java.nio.file.NotDirectoryException
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.Executor
import java.util.concurrent.atomic.AtomicLong

/**
 * One shared folder's files, as a hostfs stream asks for them. Paths are
 * relative to the folder and slash-separated; "" is the folder itself. Calls
 * block, and throw [HostFsException] (or an IOException [HostFsServer] maps
 * to an errno) when they fail.
 */
interface HostFolderFs {
    fun stat(path: String): GuestFile

    fun list(path: String): List<GuestFile>

    /** Makes [name] in directory [dir]; a provider may pick a free name like it. */
    fun create(dir: String, name: String, isDir: Boolean): GuestFile

    /** Deletes [path], a directory with everything in it. */
    fun delete(path: String)

    /** Moves [path] to [to], which doesn't exist; EXDEV when the provider can't move it to another directory. */
    fun rename(path: String, to: String): GuestFile

    /** Opens a file in an Android open mode: "r", "rw" or "rwt". */
    fun open(path: String, mode: String): HostFile
}

/** An open file of a shared folder: positional reads and writes, safe from several threads. */
interface HostFile : Closeable {
    /** Reads up to [len] bytes at [offset] into [buf]; fewer only at the end of the file. */
    fun read(offset: Long, buf: ByteArray, len: Int): Int

    fun write(offset: Long, data: ByteArray, off: Int, len: Int)

    fun truncate(size: Long)

    fun sync()
}

/** A failed operation, with the Linux errno name ("ENOENT", …) the guest sees. */
class HostFsException(val errno: String, message: String) : IOException(message)

/**
 * Answers the agent's file operations for one folder (go/internal/protocol
 * hostfs.go) on any number of its hostfs streams, which share the open files.
 */
class HostFsServer(
    private val fs: HostFolderFs,
    private val writable: Boolean,
    private val workers: Executor,
) : Closeable {
    private val handles = ConcurrentHashMap<Long, HostFile>()
    private val nextHandle = AtomicLong()

    /**
     * Reads [s]'s requests until it ends, each answered on [workers], so a
     * slow one doesn't hold up the rest. Throws when the stream breaks.
     */
    fun serve(s: AgentStream) {
        while (true) {
            val frame = Frames.read(s.input) ?: return
            if (frame.type != REQUEST) throw IOException("unexpected hostfs frame ${frame.type}")
            val b = ByteBuffer.wrap(frame.payload)
            val id = b.int
            val headerLen = b.int
            if (headerLen < 0 || headerLen > frame.payload.size - PREFIX) throw IOException("hostfs header of $headerLen bytes")
            val header = String(frame.payload, PREFIX, headerLen, Charsets.UTF_8)
            val data = frame.payload.copyOfRange(PREFIX + headerLen, frame.payload.size)
            workers.execute {
                val (reply, out) = answer(header, data)
                try {
                    val payload = encode(id, reply, out)
                    synchronized(s) { Frames.write(s.output, REPLY, payload) }
                } catch (e: Exception) {
                    Log.w(TAG, "reply: ${e.message}")
                    runCatching { s.close() }
                }
            }
        }
    }

    /** The reply to one request, and its data; never throws. */
    internal fun answer(header: String, data: ByteArray): Pair<JSONObject, ByteArray?> = try {
        run(JSONObject(header), data)
    } catch (e: Exception) {
        JSONObject().put("error", e.message ?: e.javaClass.simpleName).put("errno", errnoOf(e)) to null
    }

    private fun run(req: JSONObject, data: ByteArray): Pair<JSONObject, ByteArray?> {
        val reply = JSONObject()
        val op = req.optString("op")
        if (op !in READS && !writable) throw HostFsException("EROFS", "the folder is shared read-only")
        val path = clean(req.optString("path"))
        when (op) {
            "stat" -> reply.put("file", fs.stat(path).json())
            "list" -> reply.put("files", JSONArray(fs.list(path).map { it.json() }))
            "create" -> reply.put("file", fs.create(path, name(req.optString("name")), req.optBoolean("dir")).json())
            "delete" -> {
                if (path.isEmpty()) throw HostFsException("EBUSY", "can't delete the folder itself")
                fs.delete(path)
            }
            "rename" -> reply.put("file", fs.rename(path, clean(req.optString("to"))).json())
            "open" -> {
                val mode = req.optString("mode")
                if (mode !in MODES) throw HostFsException("EINVAL", "open mode $mode")
                if (mode != "r" && !writable) throw HostFsException("EROFS", "the folder is shared read-only")
                val id = nextHandle.incrementAndGet()
                handles[id] = fs.open(path, mode)
                reply.put("handle", id)
            }
            "read" -> {
                val buf = ByteArray(req.optLong("size").coerceIn(0, MAX_DATA.toLong()).toInt())
                val n = handle(req).read(req.optLong("offset"), buf, buf.size)
                return reply to buf.copyOf(n.coerceAtLeast(0))
            }
            "write" -> {
                handle(req).write(req.optLong("offset"), data, 0, data.size)
                reply.put("size", data.size)
            }
            "truncate" -> {
                val size = req.optLong("size")
                if (req.optLong("handle") != 0L) handle(req).truncate(size) else fs.open(path, "rw").use { it.truncate(size) }
            }
            "sync" -> handle(req).sync()
            "close" -> (handles.remove(req.optLong("handle")) ?: throw HostFsException("EBADF", "no such handle")).close()
            else -> throw HostFsException("ENOSYS", "unknown operation $op")
        }
        return reply to null
    }

    private fun handle(req: JSONObject): HostFile =
        handles[req.optLong("handle")] ?: throw HostFsException("EBADF", "no such handle")

    /** Closes every open file, for a folder no longer shared. */
    override fun close() {
        for (id in handles.keys.toList()) handles.remove(id)?.let { runCatching { it.close() } }
    }

    companion object {
        private const val TAG = "HostFsServer"
        const val REQUEST = 0x30
        const val REPLY = 0x31
        private const val PREFIX = 8
        /** go/internal/protocol MaxHostFSData. */
        const val MAX_DATA = 512 shl 10
        private val READS = setOf("stat", "list", "open", "read", "sync", "close")
        private val MODES = setOf("r", "rw", "rwt")

        fun encode(id: Int, header: JSONObject, data: ByteArray?): ByteArray {
            val h = header.toString().toByteArray(Charsets.UTF_8)
            val d = data ?: ByteArray(0)
            return ByteBuffer.allocate(PREFIX + h.size + d.size).putInt(id).putInt(h.size).put(h).put(d).array()
        }

        /** A relative path without "." or ".." steps; "" for the folder. */
        internal fun clean(p: String): String {
            val parts = p.split('/').filter { it.isNotEmpty() && it != "." }
            if (p.startsWith("/") || ".." in parts) throw HostFsException("EINVAL", "bad path $p")
            return parts.joinToString("/")
        }

        private fun name(n: String): String {
            if (n.isEmpty() || n == "." || n == ".." || '/' in n || '\u0000' in n) throw HostFsException("EINVAL", "bad name $n")
            return n
        }

        internal fun errnoOf(e: Exception): String = when (e) {
            is HostFsException -> e.errno
            is FileNotFoundException, is NoSuchFileException -> "ENOENT"
            is FileAlreadyExistsException -> "EEXIST"
            is DirectoryNotEmptyException -> "ENOTEMPTY"
            is NotDirectoryException -> "ENOTDIR"
            is AccessDeniedException, is SecurityException -> "EACCES"
            is UnsupportedOperationException -> "ENOTSUP"
            else -> "EIO"
        }

        private fun GuestFile.json(): JSONObject = JSONObject()
            .put("path", path)
            .put("name", name)
            .put("dir", dir)
            .put("size", size)
            .put("mtime", mtime)
            .put("writable", writable)
    }
}
