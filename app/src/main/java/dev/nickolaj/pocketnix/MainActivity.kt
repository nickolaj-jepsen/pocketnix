package dev.nickolaj.pocketnix

import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.ui.platform.LocalContext
import dev.nickolaj.pocketnix.ui.MainScreen

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        handle(intent)
        setContent {
            val context = LocalContext.current
            // The terminal is dark by default; keep the chrome around it dark too.
            val colors = when {
                isSystemInDarkTheme() -> dynamicDarkColorScheme(context)
                else -> runCatching { dynamicLightColorScheme(context) }.getOrDefault(darkColorScheme())
            }
            MaterialTheme(colorScheme = colors) {
                MainScreen(vm)
            }
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handle(intent)
    }

    /** Scripted control over adb: `pocketnix provision` starts the VM with ACTION_START. */
    private fun handle(intent: Intent?) {
        when (intent?.action) {
            ACTION_START -> start(intent)
            ACTION_STOP -> vm.stop()
            // Debug builds: drive tabs from adb while the phone stays locked.
            ACTION_TAB -> if (BuildConfig.DEBUG) vm.debugOpenTab(intent.getStringExtra(EXTRA_PROFILE), intent.getStringExtra(EXTRA_INPUT))
            ACTION_DUMP -> if (BuildConfig.DEBUG) vm.debugDumpTabs()
        }
    }

    private fun start(intent: Intent) {
        if (intent.hasExtra(EXTRA_NETWORK)) vm.settings.network = intent.getBooleanExtra(EXTRA_NETWORK, true)
        // The settings screen's values, for scripted starts (`--ei memoryMb 1024`).
        intent.getIntExtra(EXTRA_MEMORY, 0).takeIf { it >= 512 }?.let { vm.settings.memoryMb = it }
        intent.getIntExtra(EXTRA_DISK, 0).takeIf { it >= 4 }?.let { vm.settings.diskGb = it }
        if (intent.hasExtra(EXTRA_CPUS)) vm.settings.cpus = if (intent.getIntExtra(EXTRA_CPUS, 0) == 1) 1 else 0
        // Debug builds: `--es tcp <host:port>` runs against a desktop agent, `--es tcp ""` back to AVF.
        if (BuildConfig.DEBUG && intent.hasExtra(EXTRA_TCP)) {
            val addr = intent.getStringExtra(EXTRA_TCP).orEmpty()
            vm.settings.tcpBackend = addr.isNotEmpty()
            if (addr.isNotEmpty()) vm.settings.tcpAddress = addr
        }
        vm.start()
    }

    companion object {
        const val ACTION_START = "dev.nickolaj.pocketnix.action.START"
        const val ACTION_STOP = "dev.nickolaj.pocketnix.action.STOP"
        const val ACTION_TAB = "dev.nickolaj.pocketnix.action.DEBUG_TAB"
        const val ACTION_DUMP = "dev.nickolaj.pocketnix.action.DEBUG_DUMP"
        const val EXTRA_PROFILE = "profile"
        const val EXTRA_INPUT = "input"
        const val EXTRA_NETWORK = "network"
        const val EXTRA_TCP = "tcp"
        const val EXTRA_MEMORY = "memoryMb"
        const val EXTRA_DISK = "diskGb"
        const val EXTRA_CPUS = "cpus"
    }
}
