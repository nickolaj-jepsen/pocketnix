package dev.nickolaj.pocketnix

import android.app.Activity
import android.content.Intent
import android.os.Bundle
import dev.nickolaj.pocketnix.engine.DiskImage

/**
 * Scripted control over adb (`am start -n <pkg>/dev.nickolaj.pocketnix.ControlActivity -a …`):
 * `pocketnix provision` starts the VM with ACTION_START. An invisible activity
 * with no history, so the system never replays its intent: restoring a task
 * relaunches it from its base intent, which booted the VM again when that was
 * the main activity.
 */
class ControlActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        when (intent?.action) {
            ACTION_START -> {
                start(intent)
                if (intent.getBooleanExtra(EXTRA_OPEN, true)) {
                    startActivity(Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
                }
            }
            ACTION_STOP -> vm.stop()
            // Debug builds: drive tabs from adb while the phone stays locked.
            ACTION_TAB -> if (BuildConfig.DEBUG) vm.debugOpenTab(intent.getStringExtra(EXTRA_PROFILE), intent.getStringExtra(EXTRA_INPUT))
            ACTION_DUMP -> if (BuildConfig.DEBUG) vm.debugDumpTabs()
        }
        finish()
    }

    private fun start(intent: Intent) {
        val s = vm.settings
        if (intent.hasExtra(EXTRA_NETWORK)) s.network = intent.getBooleanExtra(EXTRA_NETWORK, true)
        // The settings screen's values, for scripted starts (`--ei memoryMb 1024`).
        intent.getIntExtra(EXTRA_MEMORY, 0).takeIf { it in 512..Settings.MAX_MEMORY_MB }?.let { s.memoryMb = it }
        // The guest grows into the disk: past the phone's free space, /data fills up.
        intent.getIntExtra(EXTRA_DISK, 0).takeIf { it in 4..DiskImage.maxGb(this) }?.let { s.diskGb = it }
        // provision's image size: a smaller disk setting would leave the root no room to grow.
        intent.getIntExtra(EXTRA_MIN_DISK, 0).takeIf { it > s.diskGb && it <= DiskImage.maxGb(this) }?.let { s.diskGb = it }
        if (intent.hasExtra(EXTRA_CPUS)) s.cpus = if (intent.getIntExtra(EXTRA_CPUS, 0) == 1) 1 else 0
        // Debug builds: `--es tcp <host:port>` runs against a desktop agent, `--es tcp ""` back to AVF.
        if (BuildConfig.DEBUG && intent.hasExtra(EXTRA_TCP)) {
            val addr = intent.getStringExtra(EXTRA_TCP).orEmpty()
            s.tcpBackend = addr.isNotEmpty()
            if (addr.isNotEmpty()) s.tcpAddress = addr
        }
        vm.start()
    }

    companion object {
        const val ACTION_START = "dev.nickolaj.pocketnix.action.START"
        const val ACTION_STOP = "dev.nickolaj.pocketnix.action.STOP"
        const val ACTION_TAB = "dev.nickolaj.pocketnix.action.DEBUG_TAB"
        const val ACTION_DUMP = "dev.nickolaj.pocketnix.action.DEBUG_DUMP"
        /** START opens the app afterwards unless false (the Quick Settings tile). */
        const val EXTRA_OPEN = "open"
        const val EXTRA_NETWORK = "network"
        const val EXTRA_TCP = "tcp"
        const val EXTRA_MEMORY = "memoryMb"
        const val EXTRA_DISK = "diskGb"
        const val EXTRA_MIN_DISK = "minDiskGb"
        const val EXTRA_CPUS = "cpus"
        const val EXTRA_PROFILE = "profile"
        const val EXTRA_INPUT = "input"
    }
}
