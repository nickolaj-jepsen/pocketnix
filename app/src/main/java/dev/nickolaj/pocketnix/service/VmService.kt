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
import dev.nickolaj.pocketnix.engine.VmState
import dev.nickolaj.pocketnix.vm
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.launch

/** Keeps the app process, and so the VM's engine and tabs, alive while the VM runs. */
class VmService : Service() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private var watch: Job? = null
    private var lastStartId = 0

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        getSystemService(NotificationManager::class.java).createNotificationChannel(
            NotificationChannel(CHANNEL, getString(R.string.notification_channel), NotificationManager.IMPORTANCE_LOW),
        )
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        // Every start must reach startForeground, or the system kills the app.
        startForeground(NOTIFICATION, notification("Starting"), ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        lastStartId = startId
        vm.serviceStarted()
        if (intent?.action == ACTION_STOP) vm.stop()
        if (watch == null) {
            watch = scope.launch {
                combine(vm.state, vm.control.link, vm.stopping) { s, link, stopping -> Triple(s, link, stopping) }
                    .collect { (s, link, stopping) ->
                        when {
                            s == VmState.Stopped || s is VmState.Failed -> {
                                // Only for the latest start: a restart's start may be on its way.
                                if (stopSelfResult(lastStartId)) stopForeground(STOP_FOREGROUND_REMOVE)
                            }
                            else -> {
                                val text = when {
                                    stopping -> "Stopping"
                                    s == VmState.Starting -> "Starting"
                                    link is dev.nickolaj.pocketnix.agent.ControlChannel.Link.Up -> "Running"
                                    else -> "Booting"
                                }
                                getSystemService(NotificationManager::class.java).notify(NOTIFICATION, notification(text))
                            }
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

    private fun notification(text: String): Notification {
        val open = PendingIntent.getActivity(this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE)
        val stop = PendingIntent.getService(
            this, 1, Intent(this, VmService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE,
        )
        return Notification.Builder(this, CHANNEL)
            .setSmallIcon(android.R.drawable.ic_menu_manage)
            .setContentTitle(getString(R.string.app_name))
            .setContentText(text)
            .setContentIntent(open)
            .setOngoing(true)
            .addAction(Notification.Action.Builder(null, "Stop", stop).build())
            .build()
    }

    companion object {
        const val ACTION_STOP = "dev.nickolaj.pocketnix.action.STOP"
        private const val CHANNEL = "vm"
        private const val NOTIFICATION = 1
    }
}
