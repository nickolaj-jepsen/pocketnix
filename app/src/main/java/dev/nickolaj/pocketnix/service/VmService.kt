package dev.nickolaj.pocketnix.service

import android.annotation.SuppressLint
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.IBinder
import android.os.PowerManager
import android.util.Log
import dev.nickolaj.pocketnix.MainActivity
import dev.nickolaj.pocketnix.R
import dev.nickolaj.pocketnix.VmPhase
import dev.nickolaj.pocketnix.agent.WakeHolder
import dev.nickolaj.pocketnix.gui.StopActivity
import dev.nickolaj.pocketnix.vm
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch

/** Keeps the app process, and so the VM's engine and tabs, alive while the VM runs. */
class VmService : Service() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private var watch: Job? = null
    private var lastStartId = 0
    private var wakeWatch: Job? = null
    private var wakeLock: PowerManager.WakeLock? = null

    /** Why the phone is kept awake, for the notification; null while it may sleep. */
    private var wakeLine: String? = null
    private var lastText = notificationText(VmPhase.Starting)

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        getSystemService(NotificationManager::class.java).createNotificationChannel(
            NotificationChannel(CHANNEL, getString(R.string.notification_channel), NotificationManager.IMPORTANCE_LOW)
        )
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        // Every start must reach startForeground, or the system kills the app.
        startForeground(NOTIFICATION, notification(lastText), ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        lastStartId = startId
        vm.serviceStarted()
        if (watch == null) {
            watch = scope.launch {
                vm.phase.collect { phase ->
                    if (phase.off) {
                        // Only for the latest start: a restart's start may be on its way.
                        if (stopSelfResult(lastStartId)) stopForeground(STOP_FOREGROUND_REMOVE)
                    } else {
                        getSystemService(NotificationManager::class.java).notify(NOTIFICATION, notification(notificationText(phase)))
                    }
                }
            }
        }
        if (wakeWatch == null) {
            wakeWatch = scope.launch { vm.wakeHolders.collect { holdWake(it) } }
        }
        return START_NOT_STICKY
    }

    /** Holds the wake lock while the guest asks for it (docs/configuration.md, Power), and says so. */
    @SuppressLint("WakelockTimeout") // held exactly as long as the guest's inhibitor; released on every way out
    private fun holdWake(holders: List<WakeHolder>?) {
        val lock = wakeLock ?: getSystemService(PowerManager::class.java)
            .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "pocketnix:guest")
            .apply { setReferenceCounted(false) }
            .also { wakeLock = it }
        if (holders != null) {
            if (!lock.isHeld) Log.i(TAG, "wake lock for $holders")
            lock.acquire()
        } else if (lock.isHeld) {
            Log.i(TAG, "wake lock released")
            lock.release()
        }
        val line = holders?.let(::wakeText)
        if (line == wakeLine) return
        wakeLine = line
        if (!vm.phase.value.off) {
            getSystemService(NotificationManager::class.java).notify(NOTIFICATION, notification(lastText))
        }
    }

    override fun onDestroy() {
        scope.cancel()
        wakeLock?.takeIf { it.isHeld }?.release()
        super.onDestroy()
    }

    private fun notification(text: String): Notification {
        lastText = text
        val line = wakeLine?.let { "$text · $it" } ?: text
        val open = PendingIntent.getActivity(this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE)
        // An activity, so it can ask first while GUI windows are open.
        val stop = PendingIntent.getActivity(this, 1, StopActivity.intent(this), PendingIntent.FLAG_IMMUTABLE)
        return Notification.Builder(this, CHANNEL)
            .setSmallIcon(android.R.drawable.ic_menu_manage)
            .setContentTitle(getString(R.string.app_name))
            .setContentText(line)
            .setStyle(Notification.BigTextStyle().bigText(line))
            .setContentIntent(open)
            .setOngoing(true)
            .addAction(Notification.Action.Builder(null, "Stop", stop).build())
            .build()
    }

    companion object {
        internal const val CHANNEL = "vm"
        private const val NOTIFICATION = 1
        private const val TAG = "VmService"
    }
}

/** The foreground notification's line for [phase]. */
internal fun notificationText(phase: VmPhase): String = when (phase) {
    VmPhase.Stopped -> "Stopped"
    is VmPhase.Failed -> "Failed: ${phase.message}"
    VmPhase.Starting -> "Starting"
    VmPhase.Booting -> "Booting"
    VmPhase.AgentLate -> "Booting, no agent yet: the console tab shows the boot"
    is VmPhase.Running -> phase.host?.let { "Running · $it" } ?: "Running"
    is VmPhase.Refused -> "The agent refused pocketnix: ${phase.reason}"
    VmPhase.Stopping -> "Stopping"
}
