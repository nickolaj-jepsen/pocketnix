package dev.nickolaj.pocketnix.files

import android.content.Context
import android.database.Cursor
import android.database.MatrixCursor
import android.os.Bundle
import android.os.CancellationSignal
import android.os.Handler
import android.os.HandlerThread
import android.os.ParcelFileDescriptor
import android.os.ProxyFileDescriptorCallback
import android.os.storage.StorageManager
import android.provider.DocumentsContract
import android.provider.DocumentsContract.Document
import android.provider.DocumentsContract.Root
import android.provider.DocumentsProvider
import android.system.ErrnoException
import android.system.OsConstants
import android.util.Log
import android.webkit.MimeTypeMap
import dev.nickolaj.pocketnix.GuestBridge
import dev.nickolaj.pocketnix.R
import dev.nickolaj.pocketnix.agent.GuestFile
import dev.nickolaj.pocketnix.agent.GuestFiles
import dev.nickolaj.pocketnix.agent.RangedFile
import dev.nickolaj.pocketnix.vm
import java.io.File
import java.io.FileNotFoundException

/**
 * The guest's file-provider root (`pocketnix.guest.files.root`) in the Files
 * app and every file picker, read-write through the agent's file streams.
 * While the VM is stopped the root is still listed, and says to start it.
 *
 * Document ids are "/" + the path under the root.
 */
class GuestDocumentsProvider : DocumentsProvider() {
    // The copy fallback's close callbacks (the upload of a written file) run here.
    private val io by lazy { Handler(HandlerThread("guest-documents").apply { start() }.looper) }

    override fun onCreate(): Boolean {
        // Copies a previous process left: their descriptors died with it.
        documentsDir().deleteRecursively()
        return true
    }

    private val ctx: Context get() = context!!

    private fun documentsDir() = File(ctx.cacheDir, "documents")

    private fun files(): GuestFiles = ctx.vm.files() ?: throw FileNotFoundException("the VM isn't running; start it in pocketnix")

    private fun path(docId: String): String {
        require(docId.startsWith("/")) { "bad document id $docId" }
        return docId.removePrefix("/")
    }

    override fun queryRoots(projection: Array<out String>?): Cursor {
        val c = MatrixCursor(projection ?: ROOT_COLUMNS)
        val host = ctx.vm.config.value?.hostname
        val up = ctx.vm.files() != null
        c.newRow().apply {
            add(Root.COLUMN_ROOT_ID, ROOT)
            add(Root.COLUMN_DOCUMENT_ID, "/")
            add(Root.COLUMN_TITLE, ctx.getString(R.string.app_name))
            add(Root.COLUMN_SUMMARY, if (up) host else "Start the VM to see its files")
            add(Root.COLUMN_FLAGS, if (up) Root.FLAG_SUPPORTS_CREATE or Root.FLAG_SUPPORTS_IS_CHILD else 0)
            add(Root.COLUMN_ICON, R.drawable.ic_pocketnix)
        }
        c.setNotificationUri(ctx.contentResolver, DocumentsContract.buildRootsUri(authority(ctx)))
        return c
    }

    override fun queryDocument(documentId: String, projection: Array<out String>?): Cursor {
        val c = MatrixCursor(projection ?: DOCUMENT_COLUMNS)
        val f = ctx.vm.files()
        when {
            f != null -> row(c, call { f.stat(path(documentId)) })
            // The root exists while stopped, so pickers can show why it's empty.
            documentId == "/" -> row(c, GuestFile("", ctx.getString(R.string.app_name), dir = true, size = 0, mtime = 0, writable = false))
            else -> throw FileNotFoundException(documentId)
        }
        return c
    }

    override fun queryChildDocuments(parentDocumentId: String, projection: Array<out String>?, sortOrder: String?): Cursor {
        val c = MatrixCursor(projection ?: DOCUMENT_COLUMNS)
        c.setNotificationUri(ctx.contentResolver, DocumentsContract.buildChildDocumentsUri(authority(ctx), parentDocumentId))
        val f = ctx.vm.files()
        if (f == null) {
            c.extras = Bundle().apply { putString(DocumentsContract.EXTRA_ERROR, "Start the VM in pocketnix to see its files.") }
            return c
        }
        for (e in call { f.list(path(parentDocumentId)) }.sortedWith(compareBy({ !it.dir }, { it.name.lowercase() }))) row(c, e)
        return c
    }

    override fun openDocument(documentId: String, mode: String, signal: CancellationSignal?): ParcelFileDescriptor {
        val f = files()
        val p = path(documentId)
        val ranged = call { f.openRanged(p, mode) } ?: return openCopy(f, documentId, p, mode)
        // Its own thread: a slow guest read mustn't stall every other open file.
        val thread = HandlerThread("guest-document").apply { start() }
        // A proxy fd takes only these three; truncating and appending are RangedFile's.
        val pfdMode = when {
            mode == "r" -> ParcelFileDescriptor.MODE_READ_ONLY
            mode.contains('r') -> ParcelFileDescriptor.MODE_READ_WRITE
            else -> ParcelFileDescriptor.MODE_WRITE_ONLY
        }
        return try {
            ctx.getSystemService(StorageManager::class.java)
                .openProxyFileDescriptor(pfdMode, Ranged(ranged, documentId, thread), Handler(thread.looper))
        } catch (e: Exception) {
            ranged.close()
            thread.quitSafely()
            throw FileNotFoundException(e.message).apply { initCause(e) }
        }
    }

    /**
     * A seekable descriptor served from ranged reads and writes: a writer's
     * bytes go to the guest as they come, and land when it closes.
     */
    private inner class Ranged(
        private val file: RangedFile,
        private val documentId: String,
        private val thread: HandlerThread,
    ) : ProxyFileDescriptorCallback() {
        // The writer already saw an error, so it knows its save failed.
        private var failed = false

        override fun onGetSize(): Long = file.size

        override fun onRead(offset: Long, size: Int, data: ByteArray): Int = io("read") {
            var n = 0
            while (n < size) {
                val got = file.read(offset + n, data, n, size - n)
                if (got == 0) break
                n += got
            }
            n
        }

        override fun onWrite(offset: Long, size: Int, data: ByteArray): Int = io("write") { file.write(offset, data, 0, size) }

        override fun onFsync() = io("fsync") { file.sync() }

        override fun onRelease() {
            try {
                if (file.changed && !failed) {
                    file.commit()
                    notifyChanged(parentOf(documentId))
                } else {
                    file.close()
                }
            } catch (e: Exception) {
                Log.e(TAG, "commit $documentId", e)
                PendingUploads.notify(ctx, "Couldn't save ${documentId.substringAfterLast('/')} in the guest", "The guest lost the changes; save again.")
            } finally {
                thread.quitSafely()
            }
        }

        private fun <T> io(op: String, block: () -> T): T = try {
            block()
        } catch (e: Exception) {
            Log.w(TAG, "$op $documentId: ${e.message}")
            failed = true
            throw ErrnoException(op, OsConstants.EIO)
        }
    }

    /**
     * For agents without ranged access: a local copy, uploaded when a writer
     * closes it (apps want seekable descriptors, which a stream isn't).
     */
    private fun openCopy(f: GuestFiles, documentId: String, p: String, mode: String): ParcelFileDescriptor {
        val pfdMode = ParcelFileDescriptor.parseMode(mode)
        val tmp = File.createTempFile("doc", null, documentsDir().apply { mkdirs() })
        // "w" and "wt" start empty; reading and appending need what's there.
        val copied = mode.contains('r') || mode.contains('a')
        if (copied) {
            try {
                call { f.read(p).use { input -> tmp.outputStream().use { input.copyTo(it, GuestFiles.COPY_BUFFER) } } }
            } catch (e: Exception) {
                tmp.delete()
                throw e
            }
        }
        if (!mode.contains('w')) {
            return ParcelFileDescriptor.open(tmp, pfdMode).also { tmp.delete() }
        }
        // An "rw" open that wrote nothing needn't upload the whole file again.
        val unchanged = if (copied) tmp.length() to tmp.lastModified() else null
        return ParcelFileDescriptor.open(tmp, pfdMode, io) { e ->
            if (e != null) {
                Log.w(TAG, "$documentId: writer failed, not uploading", e)
                tmp.delete()
                return@open
            }
            if (unchanged == tmp.length() to tmp.lastModified()) {
                tmp.delete()
                return@open
            }
            try {
                tmp.inputStream().use { input -> f.write(p, tmp.length()).use { input.copyTo(it, GuestFiles.COPY_BUFFER) } }
                tmp.delete()
                notifyChanged(parentOf(documentId))
            } catch (t: Exception) {
                Log.e(TAG, "upload $documentId", t)
                PendingUploads.keep(ctx, p, tmp)
            }
        }
    }

    override fun createDocument(parentDocumentId: String, mimeType: String, displayName: String): String {
        val dir = mimeType == Document.MIME_TYPE_DIR
        var name = displayName
        if (!dir && !name.contains('.')) {
            MimeTypeMap.getSingleton().getExtensionFromMimeType(mimeType)?.let { name = "$name.$it" }
        }
        val made = call { files().create(path(parentDocumentId), name, dir) }
        notifyChanged(parentDocumentId)
        return docId(made.path)
    }

    override fun deleteDocument(documentId: String) {
        call { files().delete(path(documentId)) }
        notifyChanged(parentOf(documentId))
    }

    override fun renameDocument(documentId: String, displayName: String): String {
        val renamed = call { files().rename(path(documentId), displayName) }
        notifyChanged(parentOf(documentId))
        return docId(renamed.path)
    }

    override fun isChildDocument(parentDocumentId: String, documentId: String): Boolean =
        parentDocumentId == "/" || documentId.startsWith("$parentDocumentId/")

    override fun getDocumentType(documentId: String): String =
        if (documentId == "/") Document.MIME_TYPE_DIR else queryDocument(documentId, arrayOf(Document.COLUMN_MIME_TYPE)).use {
            it.moveToFirst()
            it.getString(0)
        }

    private fun row(c: MatrixCursor, f: GuestFile) {
        var flags = 0
        if (f.writable) {
            flags = if (f.dir) Document.FLAG_DIR_SUPPORTS_CREATE else Document.FLAG_SUPPORTS_WRITE
            if (f.path.isNotEmpty()) flags = flags or Document.FLAG_SUPPORTS_DELETE or Document.FLAG_SUPPORTS_RENAME
        }
        c.newRow().apply {
            add(Document.COLUMN_DOCUMENT_ID, docId(f.path))
            add(Document.COLUMN_DISPLAY_NAME, f.name.ifEmpty { ctx.vm.config.value?.hostname ?: ctx.getString(R.string.app_name) })
            add(Document.COLUMN_MIME_TYPE, if (f.dir) Document.MIME_TYPE_DIR else GuestBridge.mimeType(f.name))
            add(Document.COLUMN_SIZE, if (f.dir) null else f.size)
            add(Document.COLUMN_LAST_MODIFIED, f.mtime.takeIf { it > 0 })
            add(Document.COLUMN_FLAGS, flags)
        }
    }

    private fun notifyChanged(parentDocumentId: String) {
        ctx.contentResolver.notifyChange(DocumentsContract.buildChildDocumentsUri(authority(ctx), parentDocumentId), null)
    }

    /** Agent failures surface as FileNotFoundException, which DocumentsUI shows. */
    private fun <T> call(block: () -> T): T = try {
        block()
    } catch (e: FileNotFoundException) {
        throw e
    } catch (e: Exception) {
        throw FileNotFoundException(e.message).apply { initCause(e) }
    }

    companion object {
        private const val TAG = "GuestDocuments"
        private const val ROOT = "guest"
        private val ROOT_COLUMNS = arrayOf(
            Root.COLUMN_ROOT_ID, Root.COLUMN_DOCUMENT_ID, Root.COLUMN_TITLE, Root.COLUMN_SUMMARY, Root.COLUMN_FLAGS, Root.COLUMN_ICON,
        )
        private val DOCUMENT_COLUMNS = arrayOf(
            Document.COLUMN_DOCUMENT_ID, Document.COLUMN_DISPLAY_NAME, Document.COLUMN_MIME_TYPE,
            Document.COLUMN_SIZE, Document.COLUMN_LAST_MODIFIED, Document.COLUMN_FLAGS,
        )

        fun authority(context: Context) = "${context.packageName}.files"

        fun docId(path: String) = "/$path"

        private fun parentOf(docId: String) = docId.substringBeforeLast('/', "").ifEmpty { "/" }

        /** Tells pickers the root changed, e.g. when the VM starts or stops. */
        fun rootsChanged(context: Context) {
            context.contentResolver.notifyChange(DocumentsContract.buildRootsUri(authority(context)), null)
        }
    }
}
