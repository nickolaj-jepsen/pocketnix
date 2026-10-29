package dev.nickolaj.pocketnix

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.util.Log
import org.json.JSONObject

/**
 * A guest notification with an id (go/internal/protocol/notify.go): from
 * the guest's notification daemon, so it can be replaced, withdrawn and
 * answered.
 */
data class GuestNotification(
    /** The guest's id, a uint32. */
    val id: Long,
    val title: String,
    val body: String,
    val appName: String,
    /** low, normal or critical. */
    val urgency: String,
    /** Key to label, in order; "default" is the notification's own tap, not a button. */
    val actions: List<Pair<String, String>>,
) {
    val buttons: List<Pair<String, String>> get() = actions.filter { it.first != DEFAULT_ACTION }
    val hasDefault: Boolean get() = actions.any { it.first == DEFAULT_ACTION }

    /** The Android notification id: stable per guest id, so a replacement replaces. */
    val androidId: Int get() = id.toInt()

    val channel: String get() = when (urgency) {
        "low" -> GuestNotifications.CHANNEL_LOW
        "critical" -> GuestNotifications.CHANNEL_CRITICAL
        else -> GuestNotifications.CHANNEL
    }

    companion object {
        const val DEFAULT_ACTION = "default"

        /** The notify event's notification, or null for an older agent's, which has no id. */
        fun parse(msg: JSONObject): GuestNotification? {
            val id = msg.optLong("notification")
            if (id == 0L) return null
            val a = msg.optJSONArray("actions")
            return GuestNotification(
                id = id,
                title = msg.optString("title"),
                body = msg.optString("body"),
                appName = msg.optString("appName"),
                urgency = msg.optString("urgency").ifEmpty { "normal" },
                actions = if (a == null) emptyList() else (0 until a.length()).map { a.getJSONObject(it) }.map { it.optString("key") to it.optString("label") },
            )
        }
    }
}

/**
 * Posts and withdraws [GuestNotification]s. Its actions and a dismissal go
 * through [GuestNotificationReceiver] to the guest ([Vm.answerNotification]).
 */
class GuestNotifications(private val context: Context, private val hostname: () -> String?) {
    private val manager = context.getSystemService(NotificationManager::class.java)

    init {
        manager.createNotificationChannels(
            listOf(
                NotificationChannel(CHANNEL_LOW, context.getString(R.string.guest_low_channel), NotificationManager.IMPORTANCE_LOW),
                NotificationChannel(CHANNEL_CRITICAL, context.getString(R.string.guest_critical_channel), NotificationManager.IMPORTANCE_HIGH),
            ),
        )
    }

    fun post(n: GuestNotification) {
        if (context.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) return
        val content = if (n.hasDefault) {
            answer(n.id, GuestNotification.DEFAULT_ACTION)
        } else {
            // Opens the app (a receiver may not start an activity) and tells the guest it was dismissed.
            val open = Intent(context, MainActivity::class.java)
                .setData(uri(n.id, "open"))
                .putExtra(MainActivity.EXTRA_GUEST_NOTIFICATION, n.id)
                .addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP)
            PendingIntent.getActivity(context, 0, open, FLAGS)
        }
        val dismissed = Intent(context, GuestNotificationReceiver::class.java)
            .setAction(GuestNotificationReceiver.ACTION_DISMISSED)
            .setData(uri(n.id, null))
            .putExtra(GuestNotificationReceiver.EXTRA_ID, n.id)
        val b = Notification.Builder(context, n.channel)
            .setSmallIcon(R.drawable.ic_pocketnix)
            .setContentTitle(n.title.ifEmpty { n.appName.ifEmpty { hostname() ?: "pocketnix" } })
            .setContentText(n.body)
            .setStyle(Notification.BigTextStyle().bigText(n.body))
            .setSubText(listOfNotNull(hostname(), n.appName.takeIf { it.isNotEmpty() }).joinToString(" · ").ifEmpty { null })
            .setContentIntent(content)
            .setDeleteIntent(PendingIntent.getBroadcast(context, 0, dismissed, FLAGS))
            .setAutoCancel(true)
            // A replacement (a progress update) doesn't ring again.
            .setOnlyAlertOnce(true)
        for ((key, label) in n.buttons.take(MAX_BUTTONS)) {
            b.addAction(Notification.Action.Builder(null, label.ifEmpty { key }, answer(n.id, key)).build())
        }
        manager.notify(TAG, n.androidId, b.build())
    }

    fun cancel(id: Long) = manager.cancel(TAG, id.toInt())

    private fun answer(id: Long, key: String): PendingIntent {
        val i = Intent(context, GuestNotificationReceiver::class.java)
            .setAction(GuestNotificationReceiver.ACTION_CHOSEN)
            .setData(uri(id, key))
            .putExtra(GuestNotificationReceiver.EXTRA_ID, id)
            .putExtra(GuestNotificationReceiver.EXTRA_KEY, key)
        return PendingIntent.getBroadcast(context, 0, i, FLAGS)
    }

    companion object {
        // Tagged, so guest ids don't collide with the app's own notification ids.
        const val TAG = "guest"
        const val CHANNEL = "guest"
        const val CHANNEL_LOW = "guest-low"
        const val CHANNEL_CRITICAL = "guest-critical"
        private const val MAX_BUTTONS = 3
        private const val FLAGS = PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT

        // PendingIntents differ by data, not extras: one per notification and action.
        private fun uri(id: Long, key: String?): Uri =
            Uri.Builder().scheme("pocketnix-notification").authority(id.toString()).apply { if (key != null) appendPath(key) }.build()
    }
}

/** A guest notification's action or dismissal; not exported, only its own PendingIntents reach it. */
class GuestNotificationReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        runCatching {
            val id = intent.getLongExtra(EXTRA_ID, 0)
            if (id == 0L) return
            when (intent.action) {
                ACTION_CHOSEN -> {
                    // Android removes a notification on its own tap (auto-cancel), not on a button.
                    context.getSystemService(NotificationManager::class.java).cancel(GuestNotifications.TAG, id.toInt())
                    context.vm.answerNotification(id, intent.getStringExtra(EXTRA_KEY) ?: return)
                }
                ACTION_DISMISSED -> context.vm.answerNotification(id, null)
            }
        }.onFailure { Log.e(TAG, "guest notification answer", it) }
    }

    companion object {
        private const val TAG = "GuestNotification"
        const val ACTION_CHOSEN = "dev.nickolaj.pocketnix.action.GUEST_NOTIFICATION_CHOSEN"
        const val ACTION_DISMISSED = "dev.nickolaj.pocketnix.action.GUEST_NOTIFICATION_DISMISSED"
        const val EXTRA_ID = "dev.nickolaj.pocketnix.extra.ID"
        const val EXTRA_KEY = "dev.nickolaj.pocketnix.extra.KEY"
    }
}
