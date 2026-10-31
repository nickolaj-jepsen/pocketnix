package dev.nickolaj.pocketnix.service

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.IBinder
import dev.nickolaj.pocketnix.MainActivity
import dev.nickolaj.pocketnix.R
import dev.nickolaj.pocketnix.managed.ManagedGuest
import dev.nickolaj.pocketnix.vm
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch

/**
 * Keeps the app process alive while [ManagedGuest.install] downloads and
 * unpacks the base image: minutes of work that Android would otherwise end
 * with the app's process once it's in the background.
 */
class SetupService : Service() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private var watch: Job? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        getSystemService(NotificationManager::class.java).createNotificationChannel(
            NotificationChannel(CHANNEL, getString(R.string.notification_channel), NotificationManager.IMPORTANCE_LOW)
        )
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        // Every start must reach startForeground, or the system kills the app.
        startForeground(NOTIFICATION, notification(0, 0, "Setting up"), ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
        if (watch == null) {
            watch = scope.launch {
                vm.managed.setup.collect { s ->
                    if (s is ManagedGuest.Setup.Working) {
                        val what = if (s.unpacking) "Unpacking NixOS" else "Downloading NixOS"
                        getSystemService(NotificationManager::class.java).notify(NOTIFICATION, notification(s.done, s.total, what))
                    } else {
                        stopForeground(STOP_FOREGROUND_REMOVE)
                        stopSelf()
                    }
                }
            }
        }
        return START_NOT_STICKY
    }

    override fun onDestroy() {
        scope.cancel()
        super.onDestroy()
    }

    private fun notification(done: Long, total: Long, text: String): Notification {
        val open = PendingIntent.getActivity(this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE)
        // Progress in MiB, so it fits an Int.
        val max = (total shr 20).toInt()
        return Notification.Builder(this, CHANNEL)
            .setSmallIcon(android.R.drawable.ic_menu_manage)
            .setContentTitle(text)
            .setContentIntent(open)
            .setOngoing(true)
            .setProgress(max, (done shr 20).toInt(), max == 0)
            .build()
    }

    private companion object {
        // VmService's channel: both say what the VM is doing.
        const val CHANNEL = VmService.CHANNEL
        const val NOTIFICATION = 2
    }
}
