package dev.nickolaj.pocketnix

import android.Manifest
import android.app.ActivityManager
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.util.Log

/**
 * Tells the user once when Android's memory limiter killed the app, and so the
 * VM (app/AGENTS.md, MemoryLimiter): to them it looks like an unexplained
 * power-off. The kill is REASON_OTHER, so only its description tells it apart,
 * and the docs don't promise that description's format.
 */
object LimiterKills {
    const val MESSAGE = "Android closed pocketnix for using too much memory, which powered off the VM. " +
        "The VM may need less memory, in its settings."

    private const val MARK = "MemoryLimiter"
    private const val TAG = "LimiterKills"
    private const val CHANNEL = "app-alerts"
    private const val NOTIFICATION_TAG = "limiter-kill"

    /** How many of Android's exit records to look through: it keeps few per app. */
    private const val EXITS = 16

    data class Exit(val timestamp: Long, val description: String?)

    /** The newest limiter kill in [exits] after [seenUntil], or null. */
    fun newest(exits: List<Exit>, seenUntil: Long): Exit? = exits
        .filter { it.timestamp > seenUntil && it.description?.contains(MARK) == true }
        .maxByOrNull { it.timestamp }

    /** Android's exit records for this app, oldest or newest first; empty when it won't say. */
    fun read(context: Context): List<Exit> = runCatching {
        context.getSystemService(ActivityManager::class.java)
            .getHistoricalProcessExitReasons(null, 0, EXITS)
            .map { Exit(it.timestamp, it.description) }
    }.onFailure { Log.w(TAG, "reading exit reasons", it) }.getOrDefault(emptyList())

    /** Posts [MESSAGE], when the user allows notifications. */
    fun notify(context: Context) {
        if (context.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) return
        val manager = context.getSystemService(NotificationManager::class.java)
        manager.createNotificationChannel(
            NotificationChannel(CHANNEL, context.getString(R.string.app_alerts_channel), NotificationManager.IMPORTANCE_DEFAULT)
        )
        val open = PendingIntent.getActivity(context, 0, Intent(context, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE)
        val n = Notification.Builder(context, CHANNEL)
            .setSmallIcon(R.drawable.ic_pocketnix)
            .setContentTitle("Android stopped the VM")
            .setContentText(MESSAGE)
            .setStyle(Notification.BigTextStyle().bigText(MESSAGE))
            .setContentIntent(open)
            .setAutoCancel(true)
            .build()
        manager.notify(NOTIFICATION_TAG, 0, n)
    }
}
