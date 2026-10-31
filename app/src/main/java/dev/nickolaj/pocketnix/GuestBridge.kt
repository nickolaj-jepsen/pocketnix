package dev.nickolaj.pocketnix

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.ActivityNotFoundException
import android.content.ClipData
import android.content.ClipDescription
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.PersistableBundle
import android.provider.DocumentsContract
import android.util.Log
import android.webkit.MimeTypeMap
import android.widget.Toast
import androidx.core.content.FileProvider
import dev.nickolaj.pocketnix.files.GuestDocumentsProvider
import dev.nickolaj.pocketnix.files.Transfers
import java.io.File
import org.json.JSONObject

/** Guest programs reaching Android: see docs/architecture.md, Integrations. */
class GuestBridge(private val context: Context, private val hostname: () -> String?) {
    private val manager = context.getSystemService(NotificationManager::class.java)
    private val clipboard = context.getSystemService(ClipboardManager::class.java)
    private var nextId = NOTIFICATION_BASE
    private val notifications = GuestNotifications(context, hostname)

    /** What the guest has of the clipboard, so a clip isn't sent twice or handed straight back. */
    @Volatile var clipSync = ClipboardSync()
        private set
    private val clipLock = Any()

    init {
        manager.createNotificationChannel(
            NotificationChannel(CHANNEL, context.getString(R.string.guest_channel), NotificationManager.IMPORTANCE_DEFAULT)
        )
    }

    /** Acts on an agent push; [appVisible] decides whether an open can start an activity. */
    fun onEvent(msg: JSONObject, appVisible: Boolean) {
        when (msg.optString("type")) {
            "notify" -> GuestNotification.parse(msg)?.let { notifications.post(it) }
                ?: notify(msg.optString("title"), msg.optString("body"), null)

            "notify-close" -> notifications.cancel(msg.optLong("notification"))

            "open" -> open(msg, appVisible)

            "share" -> share(msg, appVisible)

            "guest-clipboard" -> {
                val copy = GuestCopy.parse(msg) ?: return
                // A clip or an intent goes through one binder transaction (~1 MB, UTF-16): more throws.
                val text = copy.text.orEmpty().take(MAX_CLIP_CHARS)
                setGuestClip(ClipData.newPlainText(hostname() ?: "pocketnix", text), copy.sensitive, text)
            }

            else -> Log.i(TAG, "ignoring agent push ${msg.optString("type")}")
        }
    }

    /**
     * Puts a guest's copy [clip] on Android's clipboard, kept out of previews
     * when [sensitive]; [text] is its text, or null for typed data.
     */
    fun setGuestClip(clip: ClipData, sensitive: Boolean, text: String?) {
        if (sensitive) clip.description.extras = PersistableBundle().apply { putBoolean(ClipDescription.EXTRA_IS_SENSITIVE, true) }
        clipboard.setPrimaryClip(clip)
        synchronized(clipLock) { clipSync = clipSync.guestCopied(text) }
    }

    /**
     * Records that [clip], sent when the state was [before], reached the
     * guest (see [ClipboardSync.afterSend]).
     */
    fun clipSent(before: ClipboardSync, clip: ClipSnapshot) = synchronized(clipLock) {
        clipSync = clipSync.afterSend(before, clip)
    }

    private fun open(msg: JSONObject, appVisible: Boolean) {
        val url = msg.optString("url")
        if (url.isNotEmpty()) openUrl(url, appVisible) else openDocument(msg.optString("path"), appVisible)
    }

    private fun share(msg: JSONObject, appVisible: Boolean) {
        val paths = msg.optJSONArray("paths")?.let { a -> (0 until a.length()).map { a.getString(it) } }.orEmpty()
        if (paths.isEmpty()) {
            shareText(msg.optString("text"), appVisible)
        } else {
            shareFiles(paths.map(::documentUri), paths.map(::mimeType), appVisible, paths.joinToString(", "))
        }
    }

    /**
     * Opens [url]: a guest's request through a chooser, or with [chosen] (a
     * link the user picked in the app) straight in its default app.
     */
    fun openUrl(url: String, appVisible: Boolean, chosen: Boolean = false) {
        val intent = Intent(Intent.ACTION_VIEW, Uri.parse(url))
        // Without a chooser, only activities that declare themselves safe for links (browsers and the like) may take it.
        if (chosen) intent.addCategory(Intent.CATEGORY_BROWSABLE)
        open(intent, url, appVisible, chosen)
    }

    /** Opens the document at [path], relative to the files root, as [openUrl] does. */
    fun openDocument(path: String, appVisible: Boolean, chosen: Boolean = false) {
        val intent = Intent(Intent.ACTION_VIEW).setDataAndType(documentUri(path), mimeType(path)).addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        open(intent, path, appVisible, chosen)
    }

    private fun open(intent: Intent, what: String, appVisible: Boolean, chosen: Boolean) {
        if (chosen) {
            try {
                context.startActivity(intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
            } catch (_: ActivityNotFoundException) {
                Toast.makeText(context, "No app opens $what", Toast.LENGTH_LONG).show()
            }
            return
        }
        val chooser = Intent.createChooser(intent, null).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        startOrNotify(chooser, appVisible, "Open from ${hostname() ?: "the guest"}", what)
    }

    /**
     * Shares [text] through the share sheet; text too long for an intent
     * goes as a text file named after [name].
     */
    fun shareText(text: String, appVisible: Boolean, name: String = "text") {
        if (text.length <= MAX_CLIP_CHARS) {
            val send = Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_TEXT, text)
            val chooser = Intent.createChooser(send, null).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
            startOrNotify(chooser, appVisible, "Share from ${hostname() ?: "the guest"}", text.take(200))
            return
        }
        val dir = File(context.cacheDir, SHARE_DIR).apply { mkdirs() }
        val now = System.currentTimeMillis()
        val file = File(dir, "$name-$now.txt").apply { writeText(text) }
        // A receiving app may still be reading a recent share, so only old ones, or those past the newest few, go.
        dir.listFiles()?.sortedByDescending { it.lastModified() }?.forEachIndexed { i, f ->
            if (f != file && (i >= SHARE_KEEP_FILES || now - f.lastModified() > SHARE_KEEP_MS)) f.delete()
        }
        shareFiles(listOf(FileProvider.getUriForFile(context, Transfers.clipAuthority(context), file)), listOf("text/plain"), appVisible, file.name)
    }

    /** Shares the content [uris], of MIME [types], through the share sheet. */
    fun shareFiles(uris: List<Uri>, types: List<String>, appVisible: Boolean, what: String) {
        val distinct = types.distinct()
        val type = distinct.singleOrNull() ?: distinct.map { it.substringBefore('/') }.distinct().singleOrNull()?.let { "$it/*" } ?: "*/*"
        val intent = if (uris.size == 1) {
            Intent(Intent.ACTION_SEND).putExtra(Intent.EXTRA_STREAM, uris[0])
        } else {
            Intent(Intent.ACTION_SEND_MULTIPLE).putParcelableArrayListExtra(Intent.EXTRA_STREAM, ArrayList(uris))
        }
        intent.setType(type).addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION).apply {
            clipData = ClipData.newRawUri(null, uris[0]).apply { uris.drop(1).forEach { addItem(ClipData.Item(it)) } }
        }
        val chooser = Intent.createChooser(intent, null).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        startOrNotify(chooser, appVisible, "Share from ${hostname() ?: "the guest"}", what)
    }

    private fun documentUri(path: String) = DocumentsContract.buildDocumentUri(GuestDocumentsProvider.authority(context), GuestDocumentsProvider.docId(path))

    /**
     * Starts [intent] while the app is visible; Android stops background apps
     * starting activities, so otherwise a notification titled [title] does.
     */
    fun startOrNotify(intent: Intent, appVisible: Boolean, title: String, body: String) {
        if (appVisible && runCatching { context.startActivity(intent) }.isSuccess) return
        notify(title, body, PendingIntent.getActivity(context, nextId, intent, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT))
    }

    private fun notify(title: String, body: String, action: PendingIntent?) {
        if (context.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) return
        val open = action ?: PendingIntent.getActivity(context, 0, Intent(context, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE)
        val n = Notification.Builder(context, CHANNEL)
            .setSmallIcon(R.drawable.ic_pocketnix)
            .setContentTitle(title.ifEmpty { hostname() ?: "pocketnix" })
            .setContentText(body)
            .setStyle(Notification.BigTextStyle().bigText(body))
            .setSubText(hostname())
            .setContentIntent(open)
            .setAutoCancel(true)
            .build()
        manager.notify(nextId++, n)
        if (nextId >= NOTIFICATION_BASE + MAX_NOTIFICATIONS) nextId = NOTIFICATION_BASE
    }

    companion object {
        private const val TAG = "GuestBridge"
        private const val CHANNEL = "guest"

        // Clear of the VM service's and the tabs' notification ids.
        private const val NOTIFICATION_BASE = 100_000
        private const val MAX_NOTIFICATIONS = 50
        private const val MAX_CLIP_CHARS = 100_000

        // A <cache-path> of the clip provider (res/xml/clipboard_paths.xml), apart from the clipboard's, which a guest copy empties.
        private const val SHARE_DIR = "share"
        private const val SHARE_KEEP_MS = 24 * 60 * 60 * 1000L
        private const val SHARE_KEEP_FILES = 10

        fun mimeType(path: String): String = MimeTypeMap.getSingleton().getMimeTypeFromExtension(path.substringAfterLast('.', "").lowercase()) ?: "application/octet-stream"
    }
}
