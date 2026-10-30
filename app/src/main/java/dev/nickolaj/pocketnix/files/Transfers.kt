package dev.nickolaj.pocketnix.files

import android.content.ClipData
import android.content.ClipDescription
import android.content.Context
import android.net.Uri
import android.provider.DocumentsContract
import android.provider.OpenableColumns
import android.util.Log
import android.webkit.MimeTypeMap
import androidx.core.content.FileProvider
import dev.nickolaj.pocketnix.GuestBridge
import dev.nickolaj.pocketnix.agent.ControlChannel
import dev.nickolaj.pocketnix.agent.GuestFiles
import java.io.File
import java.io.IOException
import java.io.InputStream
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.runBlocking
import org.json.JSONObject

/**
 * Guest requests the app carries out (go/internal/protocol/transfers.go):
 * picks and saves through Android's document pickers ([PickerActivity]),
 * and clipboard data beyond text, which crosses only when asked for.
 */
class Transfers(
    private val context: Context,
    private val scope: CoroutineScope,
    private val control: ControlChannel,
    private val bridge: GuestBridge,
    private val files: () -> GuestFiles?,
    private val hostname: () -> String?
) {
    /** Android's clipboard as the app last saw it (on focus), which a guest's typed paste reads. */
    @Volatile var clip: ClipData? = null

    /** Acts on a transfer push; false for any other push. */
    fun onEvent(msg: JSONObject, appVisible: Boolean): Boolean {
        val host = hostname() ?: "the guest"
        when (msg.optString("type")) {
            "pick" -> bridge.startOrNotify(PickerActivity.intent(context, msg), appVisible, "Pick files for $host", "Tap to choose")
            "save" -> bridge.startOrNotify(PickerActivity.intent(context, msg), appVisible, "Save ${msg.optString("name")} from $host", "Tap to choose where")
            "guest-clipboard-data" -> io("guest clipboard") { guestClip(msg) }
            "clipboard-request" -> io("clipboard request") { sendClip(msg.optString("ref"), msg.optString("mime")) }
            else -> return false
        }
        return true
    }

    private fun io(what: String, block: () -> Unit) {
        scope.launch(Dispatchers.IO) { runCatching(block).onFailure { Log.w(TAG, "$what: ${it.message}") } }
    }

    private fun agentFiles() = files() ?: throw IOException("the VM isn't running")

    /** Sends the picked [uris] for the pick [ref], then ends it. Blocks. */
    fun picked(ref: String, uris: List<Uri>) {
        val f = agentFiles()
        try {
            for (uri in uris) withSize(context, uri) { name, size, input -> f.share(name, size, input, ref) }
        } catch (e: Exception) {
            done(ref, error = e.message ?: e.toString())
            throw e
        }
        done(ref)
    }

    /** Writes the save [ref] to [uri], a document just created for it. Blocks. */
    fun saveTo(ref: String, uri: Uri) {
        try {
            agentFiles().fetch(ref) { _, _, input ->
                val out = context.contentResolver.openOutputStream(uri, "wt") ?: throw IOException("can't write $uri")
                out.use { input.copyTo(it, GuestFiles.COPY_BUFFER) }
            }
        } catch (e: Exception) {
            // The picker made the document: don't leave it empty.
            runCatching { DocumentsContract.deleteDocument(context.contentResolver, uri) }
            throw e
        }
    }

    /** Ends the pick or save [ref] without files: the user backed out. Blocks. */
    fun cancel(ref: String) = done(ref, cancelled = true)

    private fun done(ref: String, cancelled: Boolean = false, error: String? = null) {
        val req = JSONObject().put("type", "guest-done").put("ref", ref)
        if (cancelled) req.put("cancelled", true)
        if (error != null) req.put("error", error)
        runBlocking { control.call(req) }
    }

    /** Fetches a guest's typed copy into the cache and puts it on Android's clipboard. */
    private fun guestClip(msg: JSONObject) {
        val mime = msg.optString("mime")
        val ext = MimeTypeMap.getSingleton().getExtensionFromMimeType(mime) ?: "bin"
        val dir = File(context.cacheDir, CLIP_DIR).apply { mkdirs() }
        val file = File(dir, "clip-${System.currentTimeMillis()}.$ext")
        try {
            agentFiles().fetch(msg.optString("ref")) { _, _, input -> file.outputStream().use { input.copyTo(it, GuestFiles.COPY_BUFFER) } }
        } catch (e: Exception) {
            file.delete()
            throw e
        }
        val uri = FileProvider.getUriForFile(context, clipAuthority(context), file)
        val label = hostname() ?: "pocketnix"
        bridge.setGuestClip(ClipData(ClipDescription(label, arrayOf(mime)), ClipData.Item(uri)), null)
        // The previous copies aren't on the clipboard any more.
        dir.listFiles()?.filter { it != file }?.forEach { it.delete() }
    }

    /** Sends the clipboard's data of [mime] for the paste [ref], or says why not. */
    private fun sendClip(ref: String, mime: String) {
        try {
            val f = agentFiles()
            val c = clip ?: throw IOException("the app hasn't seen the clipboard yet")
            for (i in 0 until c.itemCount) {
                val item = c.getItemAt(i)
                val uri = item.uri
                if (uri != null) {
                    val type = context.contentResolver.getType(uri) ?: continue
                    if (!mimeMatches(mime, type)) continue
                    withSize(context, uri) { _, size, input -> f.share("clipboard", size, input, ref) }
                    return
                }
                val text = when {
                    item.htmlText != null && mimeMatches(mime, "text/html") -> item.htmlText
                    item.text != null && mimeMatches(mime, "text/plain") -> item.text.toString()
                    else -> continue
                }
                val bytes = text.toByteArray()
                f.share("clipboard", bytes.size.toLong(), bytes.inputStream(), ref)
                return
            }
            throw IOException("the clipboard has no $mime any more")
        } catch (e: Exception) {
            runCatching { done(ref, error = e.message ?: e.toString()) }
            throw e
        }
    }

    companion object {
        private const val TAG = "Transfers"
        private const val CLIP_DIR = "clipboard"

        fun clipAuthority(context: Context) = "${context.packageName}.clipboard"

        /** Whether a requested MIME type, which may end in a wildcard subtype, covers [type]. */
        fun mimeMatches(want: String, type: String): Boolean {
            val w = want.substringBefore(';').trim().lowercase()
            val t = type.substringBefore(';').trim().lowercase()
            return w == t || w == "*/*" || (w.endsWith("/*") && t.startsWith(w.dropLast(1)))
        }
    }
}

/**
 * Opens [uri] and hands [block] its display name, size and bytes. A
 * provider that won't tell the size gets copied first: streams to the guest
 * send it up front.
 */
fun <T> withSize(context: Context, uri: Uri, block: (name: String, size: Long, input: InputStream) -> T): T {
    val resolver = context.contentResolver
    var name = "shared"
    var size = -1L
    resolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE), null, null, null)?.use { c ->
        if (c.moveToFirst()) {
            c.getString(0)?.let { name = it }
            if (!c.isNull(1)) size = c.getLong(1)
        }
    }
    if (size < 0) {
        val tmp = File.createTempFile("share", null, context.cacheDir)
        try {
            (resolver.openInputStream(uri) ?: throw IOException("can't read $uri")).use { input -> tmp.outputStream().use { input.copyTo(it) } }
            return tmp.inputStream().use { block(name, tmp.length(), it) }
        } finally {
            tmp.delete()
        }
    }
    return (resolver.openInputStream(uri) ?: throw IOException("can't read $uri")).use { block(name, size, it) }
}
