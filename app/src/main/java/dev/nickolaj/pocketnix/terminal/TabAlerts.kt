package dev.nickolaj.pocketnix.terminal

import android.Manifest
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.SystemClock
import dev.nickolaj.pocketnix.MainActivity
import dev.nickolaj.pocketnix.R

/**
 * Session-aware tabs: an OSC 9/777 notification from a tab nobody watches,
 * or a bell while the app is in the background, becomes an Android
 * notification that opens that tab. Badges are [Tab.attention].
 */
class TabAlerts(private val context: Context, private val hostname: () -> String?) : Tabs.Alerts {
    private val manager = context.getSystemService(NotificationManager::class.java)
    private val lastBell = HashMap<Int, Long>()

    init {
        manager.createNotificationChannel(
            NotificationChannel(CHANNEL, context.getString(R.string.terminal_channel), NotificationManager.IMPORTANCE_DEFAULT)
        )
    }

    override fun alert(tab: Tab, watched: Boolean, appVisible: Boolean, title: String?, body: String?) {
        val bell = body == null
        val now = SystemClock.elapsedRealtime()
        if (!shouldNotify(watched, appVisible, bell, lastBell[tab.id]?.let { now - it })) return
        if (bell) lastBell[tab.id] = now
        if (context.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) return
        val open = PendingIntent.getActivity(
            context,
            tab.id,
            Intent(context, MainActivity::class.java).putExtra(MainActivity.EXTRA_TAB, tab.id).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )
        val where = listOfNotNull(hostname()?.takeIf { it.isNotEmpty() }, tab.title).joinToString(" · ")
        val n = Notification.Builder(context, CHANNEL)
            .setSmallIcon(android.R.drawable.stat_notify_chat)
            .setContentTitle(title?.takeIf { it.isNotEmpty() } ?: where)
            .setContentText(if (bell) "Bell" else body)
            .setSubText(if (title.isNullOrEmpty()) null else where)
            .setContentIntent(open)
            .setAutoCancel(true)
            .build()
        manager.notify(NOTIFICATION_BASE + tab.id, n)
    }

    override fun seen(tab: Tab) {
        manager.cancel(NOTIFICATION_BASE + tab.id)
    }

    companion object {
        private const val CHANNEL = "terminal"

        // Below this the VM service's notification.
        private const val NOTIFICATION_BASE = 1000
        private const val BELL_EVERY_MS = 30_000L

        /**
         * A notification asks to be seen, so it posts unless its tab is in
         * view; a bell is noisier (every failed completion rings), so it posts
         * only from the background, at most once per [BELL_EVERY_MS] a tab.
         */
        fun shouldNotify(watched: Boolean, appVisible: Boolean, bell: Boolean, sinceLastBellMs: Long?): Boolean = when {
            watched -> false
            !bell -> true
            appVisible -> false
            else -> sinceLastBellMs == null || sinceLastBellMs >= BELL_EVERY_MS
        }
    }
}
