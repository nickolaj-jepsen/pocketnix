package dev.nickolaj.pocketnix

import android.app.Activity
import android.content.Intent
import android.os.Bundle
import android.util.Log
import dev.nickolaj.pocketnix.engine.DiskImage
import dev.nickolaj.pocketnix.gui.GuiWindowActivity
import dev.nickolaj.pocketnix.gui.validAppId

/**
 * Scripted control over adb (`am start -n <pkg>/dev.nickolaj.pocketnix.ControlActivity -a …`).
 * Invisible and without history, so the system never replays its intent: restoring
 * a task relaunches it from its base intent, which would start the VM again.
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
            // Debug builds: what a launcher tap or pin does, for an app-list entry id.
            ACTION_LAUNCH -> if (BuildConfig.DEBUG) {
                val app = intent.getStringExtra(EXTRA_APP).orEmpty()
                if (validAppId(app)) {
                    startActivity(GuiWindowActivity.appIntent(this, app, intent.getStringExtra(EXTRA_ACTION).orEmpty(), intent.getBooleanExtra(EXTRA_NEW, false)))
                }
            }
            else -> Log.w(TAG, "unknown action ${intent?.action}")
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
        // Debug builds: `--ez audio true` gives the guest a speaker from the next VM start.
        if (BuildConfig.DEBUG && intent.hasExtra(EXTRA_AUDIO)) s.audio = intent.getBooleanExtra(EXTRA_AUDIO, false)
        // Debug builds: `--ez guiHashes true` checks every GUI commit against the compositor's content hash.
        if (BuildConfig.DEBUG && intent.hasExtra(EXTRA_GUI_HASHES)) s.guiHashes = intent.getBooleanExtra(EXTRA_GUI_HASHES, false)
        // Debug builds: `--es tcp <host:port>` runs against a desktop agent, `--es tcp ""` back to AVF.
        if (BuildConfig.DEBUG && intent.hasExtra(EXTRA_TCP)) {
            val addr = intent.getStringExtra(EXTRA_TCP).orEmpty()
            s.tcpBackend = addr.isNotEmpty()
            if (addr.isNotEmpty()) s.tcpAddress = addr
        }
        vm.start()
    }

    companion object {
        private const val TAG = "ControlActivity"
        const val ACTION_START = "dev.nickolaj.pocketnix.action.START"
        const val ACTION_STOP = "dev.nickolaj.pocketnix.action.STOP"
        const val ACTION_LAUNCH = "dev.nickolaj.pocketnix.action.DEBUG_LAUNCH"
        const val EXTRA_APP = "app"
        const val EXTRA_ACTION = "action"
        const val EXTRA_NEW = "newWindow"
        /** START opens the app afterwards unless false (the Quick Settings tile). */
        const val EXTRA_OPEN = "open"
        const val EXTRA_NETWORK = "network"
        const val EXTRA_TCP = "tcp"
        const val EXTRA_MEMORY = "memoryMb"
        const val EXTRA_DISK = "diskGb"
        const val EXTRA_MIN_DISK = "minDiskGb"
        const val EXTRA_CPUS = "cpus"
        const val EXTRA_GUI_HASHES = "guiHashes"
        const val EXTRA_AUDIO = "audio"
    }
}
