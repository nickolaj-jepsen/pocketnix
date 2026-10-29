package dev.nickolaj.pocketnix.host

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.os.BatteryManager
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.util.Log
import dev.nickolaj.pocketnix.agent.HostState
import java.util.TimeZone
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * The phone's timezone, network and power, for the guest (docs/configuration.md,
 * Phone state). Only state changes count: no battery level, which would
 * wake the guest every percent. Callbacks run on the main thread, where an
 * exception would take the VM down with the app, so each one catches.
 */
class HostMonitor(private val context: Context) {
    private val _state = MutableStateFlow<HostState?>(null)

    /** The current state while started, else null. */
    val state: StateFlow<HostState?> = _state.asStateFlow()

    private val main = Handler(Looper.getMainLooper())
    private val connectivity = context.getSystemService(ConnectivityManager::class.java)
    private val power = context.getSystemService(PowerManager::class.java)
    private var started = false

    private var timeZone = TimeZone.getDefault().id
    private var online = false
    private var metered = false
    private var charging = false
    private var batteryLow = false
    private var powerSave = false

    private val receiver = object : BroadcastReceiver() {
        override fun onReceive(c: Context, intent: Intent) = guard {
            when (intent.action) {
                // The extra: the process's default zone may not have caught up yet.
                Intent.ACTION_TIMEZONE_CHANGED -> timeZone = intent.getStringExtra(Intent.EXTRA_TIMEZONE) ?: TimeZone.getDefault().id

                Intent.ACTION_POWER_CONNECTED -> charging = true

                Intent.ACTION_POWER_DISCONNECTED -> charging = false

                Intent.ACTION_BATTERY_LOW -> batteryLow = true

                Intent.ACTION_BATTERY_OKAY -> batteryLow = false

                PowerManager.ACTION_POWER_SAVE_MODE_CHANGED -> powerSave = power.isPowerSaveMode
            }
            publish()
        }
    }

    private val network = object : ConnectivityManager.NetworkCallback() {
        override fun onCapabilitiesChanged(n: Network, caps: NetworkCapabilities) = guard {
            online = caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
            metered = !caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED)
            publish()
        }

        override fun onLost(n: Network) = guard {
            online = false
            publish()
        }
    }

    /** Starts watching; [state] is current from here on. */
    fun start() = guard {
        if (started) return@guard
        started = true
        timeZone = TimeZone.getDefault().id
        powerSave = power.isPowerSaveMode
        // Sticky: the last battery broadcast, without registering for every level change.
        context.registerReceiver(null, IntentFilter(Intent.ACTION_BATTERY_CHANGED))?.let {
            charging = it.getIntExtra(BatteryManager.EXTRA_PLUGGED, 0) != 0
            batteryLow = it.getBooleanExtra(BatteryManager.EXTRA_BATTERY_LOW, false)
        }
        online = false
        val filter = IntentFilter().apply {
            addAction(Intent.ACTION_TIMEZONE_CHANGED)
            addAction(Intent.ACTION_POWER_CONNECTED)
            addAction(Intent.ACTION_POWER_DISCONNECTED)
            addAction(Intent.ACTION_BATTERY_LOW)
            addAction(Intent.ACTION_BATTERY_OKAY)
            addAction(PowerManager.ACTION_POWER_SAVE_MODE_CHANGED)
        }
        context.registerReceiver(receiver, filter, null, main, Context.RECEIVER_NOT_EXPORTED)
        // Calls back at once with the current default network, if any.
        connectivity.registerDefaultNetworkCallback(network, main)
        publish()
    }

    fun stop() = guard {
        if (!started) return@guard
        started = false
        runCatching { context.unregisterReceiver(receiver) }
        runCatching { connectivity.unregisterNetworkCallback(network) }
        _state.value = null
    }

    private fun publish() {
        if (started) _state.value = HostState(timeZone, online, metered, charging, batteryLow, powerSave)
    }

    private inline fun guard(block: () -> Unit) {
        try {
            block()
        } catch (e: Exception) {
            Log.e(TAG, "host state", e)
        }
    }

    private companion object {
        const val TAG = "HostMonitor"
    }
}
