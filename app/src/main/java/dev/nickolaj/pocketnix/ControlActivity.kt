package dev.nickolaj.pocketnix

import android.app.Activity
import android.content.Intent
import android.os.Bundle
import dev.nickolaj.pocketnix.engine.DiskImage
import dev.nickolaj.pocketnix.gui.GuiWindowActivity
import dev.nickolaj.pocketnix.gui.validAppId

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
            // Debug builds: what a launcher tap or pin does, for an app-list entry id.
            ACTION_LAUNCH -> if (BuildConfig.DEBUG) {
                val app = intent.getStringExtra(EXTRA_APP).orEmpty()
                if (validAppId(app)) {
                    startActivity(GuiWindowActivity.appIntent(this, app, intent.getStringExtra(EXTRA_ACTION).orEmpty(), intent.getBooleanExtra(EXTRA_NEW, false)))
                }
            }
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
        // Debug builds: `--es gpu <name>` picks the guest's GPU (names in AvfEngine.gpuSpec; "default" clears it).
        if (BuildConfig.DEBUG && intent.hasExtra(EXTRA_GPU)) {
            s.gpu = intent.getStringExtra(EXTRA_GPU).orEmpty().takeUnless { it == "default" }.orEmpty()
        }
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
        const val ACTION_START = "dev.nickolaj.pocketnix.action.START"
        const val ACTION_STOP = "dev.nickolaj.pocketnix.action.STOP"
        const val ACTION_TAB = "dev.nickolaj.pocketnix.action.DEBUG_TAB"
        const val ACTION_DUMP = "dev.nickolaj.pocketnix.action.DEBUG_DUMP"
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
        const val EXTRA_GPU = "gpu"
        const val EXTRA_GUI_HASHES = "guiHashes"
        const val EXTRA_PROFILE = "profile"
        const val EXTRA_INPUT = "input"
    }
}
