package dev.nickolaj.pocketnix

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.provider.DocumentsContract
import android.util.Log
import android.webkit.MimeTypeMap
import dev.nickolaj.pocketnix.files.GuestDocumentsProvider
import org.json.JSONObject

/**
 * Guest programs reaching Android (design.md, Integrations and UX):
 * `notify-send` becomes a notification, `xdg-open` opens a URL or a file
 * (through the file provider), and a guest copy sets the clipboard.
 */
class GuestBridge(private val context: Context, private val hostname: () -> String?) {
    private val manager = context.getSystemService(NotificationManager::class.java)
    private val clipboard = context.getSystemService(ClipboardManager::class.java)
    private var nextId = NOTIFICATION_BASE

    /** What a guest copy last put on the clipboard, so it isn't handed straight back. */
    @Volatile var lastGuestCopy: String? = null
        private set

    init {
        manager.createNotificationChannel(
            NotificationChannel(CHANNEL, context.getString(R.string.guest_channel), NotificationManager.IMPORTANCE_DEFAULT),
        )
    }

    /** Acts on an agent push; [appVisible] decides whether an open can start an activity. */
    fun onEvent(msg: JSONObject, appVisible: Boolean) {
        when (msg.optString("type")) {
            "notify" -> notify(msg.optString("title"), msg.optString("body"), null)
            "open" -> open(msg, appVisible)
            "guest-clipboard" -> {
                // A clip goes through one binder transaction (~1 MB, UTF-16): more throws.
                val text = msg.optString("text").take(MAX_CLIP_CHARS)
                lastGuestCopy = text
                clipboard.setPrimaryClip(ClipData.newPlainText(hostname() ?: "pocketnix", text))
            }
            else -> Log.i(TAG, "ignoring agent push ${msg.optString("type")}")
        }
    }

    private fun open(msg: JSONObject, appVisible: Boolean) {
        val url = msg.optString("url")
        val intent = if (url.isNotEmpty()) {
            Intent(Intent.ACTION_VIEW, Uri.parse(url))
        } else {
            val path = msg.optString("path")
            val uri = DocumentsContract.buildDocumentUri(GuestDocumentsProvider.authority(context), GuestDocumentsProvider.docId(path))
            Intent(Intent.ACTION_VIEW).setDataAndType(uri, mimeType(path)).addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        }
        val chooser = Intent.createChooser(intent, null).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        // Android stops background apps starting activities; a notification can.
        if (appVisible && runCatching { context.startActivity(chooser) }.isSuccess) return
        val what = url.ifEmpty { msg.optString("path") }
        notify("Open from ${hostname() ?: "the guest"}", what, PendingIntent.getActivity(context, nextId, chooser, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT))
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
        // Clear of the VM service's (1) and the tabs' (1000 + tab).
        private const val NOTIFICATION_BASE = 100_000
        private const val MAX_NOTIFICATIONS = 50
        private const val MAX_CLIP_CHARS = 100_000

        fun mimeType(path: String): String =
            MimeTypeMap.getSingleton().getMimeTypeFromExtension(path.substringAfterLast('.', "").lowercase()) ?: "application/octet-stream"
    }
}
