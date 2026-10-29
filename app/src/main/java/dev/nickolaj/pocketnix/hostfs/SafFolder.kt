package dev.nickolaj.pocketnix.hostfs

import android.content.ContentResolver
import android.database.Cursor
import android.net.Uri
import android.os.ParcelFileDescriptor
import android.provider.DocumentsContract
import android.provider.DocumentsContract.Document
import android.system.ErrnoException
import android.system.Os
import android.system.OsConstants
import dev.nickolaj.pocketnix.GuestBridge
import dev.nickolaj.pocketnix.agent.GuestFile
import java.io.FileNotFoundException
import java.io.IOException

/**
 * A folder the user granted through the Storage Access Framework (a tree
 * URI). Documents have ids, not paths, so paths resolve by listing their
 * parents, and the ids found are remembered until an operation on one fails.
 * Open files are the provider's descriptors, read and written in place:
 * ExternalStorageProvider's are real, seekable files.
 */
class SafFolder(private val resolver: ContentResolver, private val tree: Uri) : HostFolderFs {
    private val rootId = DocumentsContract.getTreeDocumentId(tree)

    // Path → document id, most recently used last.
    private val ids = object : LinkedHashMap<String, String>(256, 0.75f, true) {
        override fun removeEldestEntry(eldest: MutableMap.MutableEntry<String, String>?) = size > MAX_IDS
    }

    private fun uri(id: String): Uri = DocumentsContract.buildDocumentUriUsingTree(tree, id)

    private fun id(path: String): String {
        if (path.isEmpty()) return rootId
        synchronized(ids) { ids[path] }?.let { return it }
        children(parentOf(path))
        return synchronized(ids) { ids[path] } ?: throw FileNotFoundException(path)
    }

    /** Forgets the ids of [path] and everything under it. */
    private fun forget(path: String) = synchronized(ids) {
        ids.keys.removeAll { it == path || it.startsWith("$path/") }
    }

    /** Runs [block] on [path]'s document, once more with fresh ids when a remembered one went stale. */
    private fun <T> withId(path: String, block: (String) -> T): T {
        try {
            return block(id(path))
        } catch (e: Exception) {
            // Some providers reject an id they no longer know with IllegalArgumentException.
            if (path.isEmpty() || (e !is FileNotFoundException && e !is IllegalArgumentException)) throw e
        }
        forget(path)
        return block(id(path))
    }

    private fun children(dir: String): List<GuestFile> = withId(dir) { dirId ->
        val out = mutableListOf<GuestFile>()
        query(DocumentsContract.buildChildDocumentsUriUsingTree(tree, dirId)) { c ->
            val (id, f) = row(c, dir)
            if (f.name.isEmpty() || f.name == "." || f.name == ".." || '/' in f.name) return@query
            synchronized(ids) { ids[f.path] = id }
            out += f
        }
        out
    }

    override fun stat(path: String): GuestFile = withId(path) { id ->
        var found: GuestFile? = null
        query(uri(id)) { c -> found = row(c, parentOf(path)).second }
        val f = found ?: throw FileNotFoundException(path)
        // The tree's root is named by the provider, not by its path.
        if (path.isEmpty()) f.copy(path = "", name = "") else f.copy(path = path)
    }

    override fun list(path: String): List<GuestFile> = children(path)

    override fun create(dir: String, name: String, isDir: Boolean): GuestFile {
        val mime = if (isDir) Document.MIME_TYPE_DIR else GuestBridge.mimeType(name)
        val created = withId(dir) { DocumentsContract.createDocument(resolver, uri(it), mime, name) }
            ?: throw IOException("the provider didn't create $name")
        val id = DocumentsContract.getDocumentId(created)
        var f: GuestFile? = null
        query(created) { c -> f = row(c, dir).second }
        val file = f ?: throw IOException("the provider lost $name")
        synchronized(ids) { ids[file.path] = id }
        return file
    }

    override fun delete(path: String) {
        withId(path) { if (!DocumentsContract.deleteDocument(resolver, uri(it))) throw IOException("the provider didn't delete $path") }
        forget(path)
    }

    override fun rename(path: String, to: String): GuestFile {
        val from = parentOf(path)
        val into = parentOf(to)
        val name = to.substringAfterLast('/')
        var doc = withId(path) { uri(it) }
        if (from != into) {
            val flags = flags(doc)
            if (flags and Document.FLAG_SUPPORTS_MOVE == 0) throw HostFsException("EXDEV", "the provider can't move documents")
            doc = DocumentsContract.moveDocument(resolver, doc, uri(id(from)), uri(id(into)))
                ?: throw IOException("the provider didn't move $path")
        }
        forget(path)
        if (path.substringAfterLast('/') != name) {
            doc = DocumentsContract.renameDocument(resolver, doc, name) ?: throw IOException("the provider didn't rename $path")
        }
        forget(to)
        var f: GuestFile? = null
        query(doc) { c -> f = row(c, into).second }
        return f ?: throw IOException("the provider lost $to")
    }

    override fun open(path: String, mode: String): HostFile {
        val pfd = withId(path) { resolver.openFileDescriptor(uri(it), mode) } ?: throw FileNotFoundException(path)
        return Descriptor(pfd)
    }

    private fun flags(doc: Uri): Int {
        var flags = 0
        query(doc) { c -> flags = c.getInt(c.getColumnIndexOrThrow(Document.COLUMN_FLAGS)) }
        return flags
    }

    private fun query(uri: Uri, each: (Cursor) -> Unit) {
        val c = resolver.query(uri, COLUMNS, null, null, null) ?: throw FileNotFoundException(uri.toString())
        c.use { while (it.moveToNext()) each(it) }
    }

    /** A row's document id, and the file it is in [dir]. */
    private fun row(c: Cursor, dir: String): Pair<String, GuestFile> {
        val name = c.getString(c.getColumnIndexOrThrow(Document.COLUMN_DISPLAY_NAME)).orEmpty()
        val isDir = c.getString(c.getColumnIndexOrThrow(Document.COLUMN_MIME_TYPE)) == Document.MIME_TYPE_DIR
        val flags = c.getInt(c.getColumnIndexOrThrow(Document.COLUMN_FLAGS))
        val size = c.getColumnIndexOrThrow(Document.COLUMN_SIZE).let { if (c.isNull(it)) 0 else c.getLong(it) }
        val mtime = c.getColumnIndexOrThrow(Document.COLUMN_LAST_MODIFIED).let { if (c.isNull(it)) 0 else c.getLong(it) }
        val writable = flags and (if (isDir) Document.FLAG_DIR_SUPPORTS_CREATE else Document.FLAG_SUPPORTS_WRITE) != 0
        val path = if (dir.isEmpty()) name else "$dir/$name"
        return c.getString(c.getColumnIndexOrThrow(Document.COLUMN_DOCUMENT_ID)) to
            GuestFile(path, name, isDir, if (isDir) 0 else size, mtime, writable)
    }

    /** A provider's descriptor, read and written at offsets. */
    private class Descriptor(private val pfd: ParcelFileDescriptor) : HostFile {
        private val fd = pfd.fileDescriptor

        override fun read(offset: Long, buf: ByteArray, len: Int): Int = errno {
            var n = 0
            while (n < len) {
                val r = Os.pread(fd, buf, n, len - n, offset + n)
                if (r <= 0) break
                n += r
            }
            n
        }

        override fun write(offset: Long, data: ByteArray, off: Int, len: Int) = errno {
            var n = 0
            while (n < len) n += Os.pwrite(fd, data, off + n, len - n, offset + n)
        }

        override fun truncate(size: Long) = errno { Os.ftruncate(fd, size) }

        override fun sync() = errno { Os.fsync(fd) }

        override fun close() = pfd.close()

        private inline fun <T> errno(block: () -> T): T = try {
            block()
        } catch (e: ErrnoException) {
            throw HostFsException(OsConstants.errnoName(e.errno) ?: "EIO", e.message ?: "errno ${e.errno}")
        }
    }

    private companion object {
        const val MAX_IDS = 4096
        val COLUMNS = arrayOf(
            Document.COLUMN_DOCUMENT_ID,
            Document.COLUMN_DISPLAY_NAME,
            Document.COLUMN_MIME_TYPE,
            Document.COLUMN_SIZE,
            Document.COLUMN_LAST_MODIFIED,
            Document.COLUMN_FLAGS
        )

        fun parentOf(path: String) = path.substringBeforeLast('/', "")
    }
}
