package dev.nickolaj.pocketnix

import android.Manifest
import android.content.ClipboardManager
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import dev.nickolaj.pocketnix.ui.MainScreen
import dev.nickolaj.pocketnix.ui.PocketnixTheme

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        openTab(intent)
        guestNotificationOpened(intent)
        // Tab notifications; `pocketnix provision` grants it, this asks otherwise. Asked once.
        if (savedInstanceState == null && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 0)
        }
        setContent {
            PocketnixTheme {
                MainScreen(vm)
            }
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        openTab(intent)
        guestNotificationOpened(intent)
    }

    override fun onStart() {
        super.onStart()
        vm.tabs.visible = true
        // A VM started from the background (adb, a locked phone) may run without its service.
        if (vm.serviceRefused.value) vm.startService()
    }

    override fun onStop() {
        vm.tabs.visible = false
        super.onStop()
    }

    // Android lets an app read the clipboard only while it has focus.
    override fun onWindowFocusChanged(hasFocus: Boolean) {
        super.onWindowFocusChanged(hasFocus)
        if (!hasFocus) return
        val clip = getSystemService(ClipboardManager::class.java)?.primaryClip?.takeIf { it.itemCount > 0 } ?: return
        vm.syncClipboard(clip)
    }

    /** A tab notification opens its tab. */
    private fun openTab(intent: Intent?) {
        val id = intent?.getIntExtra(EXTRA_TAB, -1) ?: -1
        if (id >= 0) vm.tabs.select(id)
    }

    /** A tapped guest notification without a default action is gone, which its program may wait for. */
    private fun guestNotificationOpened(intent: Intent?) {
        val id = intent?.getLongExtra(EXTRA_GUEST_NOTIFICATION, 0L) ?: 0L
        // Recents replays the launching intent: that tap was answered already.
        if (id != 0L && intent!!.flags and Intent.FLAG_ACTIVITY_LAUNCHED_FROM_HISTORY == 0) vm.answerNotification(id, null)
    }

    companion object {
        const val EXTRA_TAB = "dev.nickolaj.pocketnix.extra.TAB"
        const val EXTRA_GUEST_NOTIFICATION = "dev.nickolaj.pocketnix.extra.GUEST_NOTIFICATION"
    }
}
