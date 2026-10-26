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

    /** `pocketnix provision` starts the VM with this, optionally without networking. */
    private fun handle(intent: Intent?) {
        if (intent?.action != ACTION_START) return
        if (intent.hasExtra(EXTRA_NETWORK)) vm.settings.network = intent.getBooleanExtra(EXTRA_NETWORK, true)
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
        const val EXTRA_NETWORK = "network"
        const val EXTRA_TCP = "tcp"
    }
}
