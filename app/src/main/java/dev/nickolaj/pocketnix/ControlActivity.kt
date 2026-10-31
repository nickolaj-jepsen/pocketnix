package dev.nickolaj.pocketnix

import android.app.Activity
import android.content.Intent
import android.os.Bundle
import android.util.Log
import dev.nickolaj.pocketnix.agent.Plan
import dev.nickolaj.pocketnix.engine.DiskImage
import dev.nickolaj.pocketnix.gui.GuiWindowActivity
import dev.nickolaj.pocketnix.gui.validAppId
import dev.nickolaj.pocketnix.managed.Selections
import kotlinx.coroutines.launch

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

            // Debug builds: the setup screen's button (`--es user alice --es services ssh,gui`).
            ACTION_SETUP -> if (BuildConfig.DEBUG) {
                intent.getStringExtra(EXTRA_RELEASE_URL)?.let { vm.settings.releaseUrl = it }
                intent.getIntExtra(EXTRA_DISK, 0).takeIf { it in Settings.MIN_SETUP_DISK_GB..DiskImage.maxGb(this) }?.let { vm.settings.diskGb = it }
                vm.managed.install(Selections(intent.getStringExtra(EXTRA_USER).orEmpty(), services(intent))) { vm.start() }
            }

            // Debug builds: what Configure and the launcher do; results show in `pocketnix control dump`.
            ACTION_MANAGED -> if (BuildConfig.DEBUG) managed(intent)

            else -> Log.w(TAG, "unknown action ${intent?.action}")
        }
        finish()
    }

    private fun services(intent: Intent): Set<String> = intent.getStringExtra(EXTRA_SERVICES).orEmpty().split(',').filter { it.isNotBlank() }.toSet()

    private fun managed(intent: Intent) {
        val m = vm.managed
        val allow = intent.getBooleanExtra(EXTRA_ALLOW_BUILDS, false)
        val pkg = intent.getStringExtra(EXTRA_PACKAGE).orEmpty()
        val confirm: suspend (Plan) -> Boolean = { allow }
        vm.debugScope.launch {
            val result = when (intent.getStringExtra(EXTRA_OP)) {
                "rebuild" -> {
                    val old = m.store.selections ?: Selections("", emptySet())
                    val user = intent.getStringExtra(EXTRA_USER) ?: old.user
                    val services = if (intent.hasExtra(EXTRA_SERVICES)) services(intent) else old.services
                    m.rebuild(Selections(user, services, intent.getIntExtra(EXTRA_KEEP, old.keepGenerations)), confirm)
                }

                "update" -> m.update(confirm)

                "add" -> m.addPackage(pkg, allow).error

                "remove" -> m.removePackage(pkg).error

                "run" -> m.run(pkg, allow).let { u -> u.error ?: "run ${u.run}" }

                else -> "unknown op"
            }
            Log.i(TAG, "managed ${intent.getStringExtra(EXTRA_OP)} $pkg: ${result ?: "done"}")
        }
    }

    private fun start(intent: Intent) {
        val s = vm.settings
        if (intent.hasExtra(EXTRA_NETWORK)) s.network = intent.getBooleanExtra(EXTRA_NETWORK, true)
        // The settings screen's values, for scripted starts (`--ei memoryMb 1024`).
        intent.getIntExtra(EXTRA_MEMORY, 0).takeIf { it in 512..Settings.MAX_MEMORY_MB }?.let { s.memoryMb = it }
        // The guest grows into the disk: past the phone's free space, /data fills up.
        intent.getIntExtra(EXTRA_DISK, 0).takeIf { it in Settings.MIN_DISK_GB..DiskImage.maxGb(this) }?.let { s.diskGb = it }
        // provision's image size: a smaller disk setting would leave the root no room to grow.
        intent.getIntExtra(EXTRA_MIN_DISK, 0).takeIf { it > s.diskGb && it <= DiskImage.maxGb(this) }?.let { s.diskGb = it }
        if (intent.hasExtra(EXTRA_CPUS)) s.cpus = if (intent.getIntExtra(EXTRA_CPUS, 0) == 1) 1 else 0
        // Debug builds: `--ez audio true` gives the guest a speaker from the next VM start.
        if (BuildConfig.DEBUG && intent.hasExtra(EXTRA_AUDIO)) s.audio = intent.getBooleanExtra(EXTRA_AUDIO, false)
        // Debug builds: `--ez guestRam false` keeps guest RAM out of the app from the next VM start.
        if (BuildConfig.DEBUG && intent.hasExtra(EXTRA_GUEST_RAM)) s.guestRam = intent.getBooleanExtra(EXTRA_GUEST_RAM, true)
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
        const val ACTION_SETUP = "dev.nickolaj.pocketnix.action.DEBUG_SETUP"
        const val ACTION_MANAGED = "dev.nickolaj.pocketnix.action.DEBUG_MANAGED"
        const val EXTRA_USER = "user"

        /** Comma-separated service ids ([dev.nickolaj.pocketnix.managed.SERVICES]). */
        const val EXTRA_SERVICES = "services"
        const val EXTRA_RELEASE_URL = "releaseUrl"

        /** MANAGED's operation: rebuild, update, add, remove or run. */
        const val EXTRA_OP = "op"
        const val EXTRA_PACKAGE = "package"
        const val EXTRA_KEEP = "keepGenerations"

        /** MANAGED: say yes when an operation would compile on the phone. */
        const val EXTRA_ALLOW_BUILDS = "allowBuilds"
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
        const val EXTRA_GUEST_RAM = "guestRam"
    }
}
