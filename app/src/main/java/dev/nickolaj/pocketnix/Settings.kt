package dev.nickolaj.pocketnix

import android.content.Context
import androidx.core.content.edit
import dev.nickolaj.pocketnix.engine.VmSettings
import org.json.JSONObject

/** What the app UI owns: set-once hardware, and in debug builds the backend. */
class Settings(context: Context) {
    private val prefs = context.getSharedPreferences("settings", Context.MODE_PRIVATE)

    var memoryMb: Int
        get() = prefs.getInt("memoryMb", 2048)
        set(v) = prefs.edit { putInt("memoryMb", v) }

    /** 1, or 0 for one vCPU per host core. */
    var cpus: Int
        get() = prefs.getInt("cpus", 0)
        set(v) = prefs.edit { putInt("cpus", v) }

    var diskGb: Int
        get() = prefs.getInt("diskGb", 16)
        set(v) = prefs.edit { putInt("diskGb", v) }

    /** Only one networked VM can run on a device at a time. */
    var network: Boolean
        get() = prefs.getBoolean("network", true)
        set(v) = prefs.edit { putBoolean("network", v) }

    /** Debug builds: give the guest a speaker. */
    var audio: Boolean
        get() = BuildConfig.DEBUG && prefs.getBoolean("audio", false)
        set(v) = prefs.edit { putBoolean("audio", v) }

    /** Debug builds: run against an agent over TCP instead of AVF. */
    var tcpBackend: Boolean
        get() = BuildConfig.DEBUG && prefs.getBoolean("tcpBackend", false)
        set(v) = prefs.edit { putBoolean("tcpBackend", v) }

    /** Debug builds: where the release's base image and package index are, instead of GitHub. */
    var releaseUrl: String
        get() = if (BuildConfig.DEBUG) prefs.getString("releaseUrl", "")!! else ""
        set(v) = prefs.edit { putString("releaseUrl", v) }

    /** host:port of the TCP agent; 10.0.2.2 is the emulator's host. */
    var tcpAddress: String
        get() = prefs.getString("tcpAddress", "10.0.2.2:9100")!!
        set(v) = prefs.edit { putString("tcpAddress", v) }

    /** Debug builds: the compositor sends content hashes, and the app checks every commit against them. */
    var guiHashes: Boolean
        get() = BuildConfig.DEBUG && prefs.getBoolean("guiHashes", false)
        set(v) = prefs.edit { putBoolean("guiHashes", v) }

    var startOnBoot: Boolean
        get() = prefs.getBoolean("startOnBoot", false)
        set(v) = prefs.edit { putBoolean("startOnBoot", v) }

    /** Off by default: the screen is the phone's biggest battery draw. */
    var keepScreenOn: Boolean
        get() = prefs.getBoolean("keepScreenOn", false)
        set(v) = prefs.edit { putBoolean("keepScreenOn", v) }

    var textSize: Int
        get() = prefs.getInt("textSize", 0)
        set(v) = prefs.edit { putInt("textSize", v) }

    /** The guest's sleep inhibitors may hold a wake lock; off overrides the module. */
    var wakeLocks: Boolean
        get() = prefs.getBoolean("wakeLocks", true)
        set(v) = prefs.edit { putBoolean("wakeLocks", v) }

    /** The module's last terminal colours, so a cold start draws them before the agent sends them; null before the first. */
    var terminalColors: Map<String, String>?
        get() = prefs.getString("terminalColors", null)?.let { s ->
            runCatching { JSONObject(s).let { o -> o.keys().asSequence().associateWith { o.getString(it) } } }.getOrNull()
        }
        set(v) = prefs.edit { if (v == null) remove("terminalColors") else putString("terminalColors", JSONObject(v).toString()) }

    fun vm() = VmSettings(memoryMb = memoryMb, cpus = cpus, diskGb = diskGb, network = network, audio = audio)

    companion object {
        const val MAX_MEMORY_MB = 65_536
    }
}
