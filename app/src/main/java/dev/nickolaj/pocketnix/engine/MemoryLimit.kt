package dev.nickolaj.pocketnix.engine

import android.app.ActivityManager
import android.content.Context
import java.io.File

/**
 * Android's per-app memory limit out of front, as this phone's vendor config
 * gives it (app/AGENTS.md, MemoryLimiter). Apps have no API for it.
 */
data class MemoryLimit(val notVisibleMib: Int, val notVisibleSwapMib: Int) {
    /** What to tell someone giving the VM [vmMib], or null when that fits the limit. */
    fun warning(vmMib: Int): String? {
        if (vmMib + OVERHEAD_MIB <= notVisibleMib) return null
        return "Android limits the app to $notVisibleMib MB while it isn't in front. A guest that uses " +
            "more can freeze the phone until the app comes back, and past ${notVisibleMib + notVisibleSwapMib} MB " +
            "Android closes the app, which powers off the VM."
    }

    companion object {
        /** Roughly what the app and crosvm hold beside guest RAM. Go's `limiterOverheadMiB` is the same. */
        const val OVERHEAD_MIB = 200

        private const val CONFIG = "/vendor/etc/memory-limiter-config.xml"
        private val COMMENT = Regex("<!--.*?-->", RegexOption.DOT_MATCHES_ALL)
        private val LIMIT_SET = Regex("<limitSet>(.*?)</limitSet>", RegexOption.DOT_MATCHES_ALL)

        private fun tag(set: String, name: String): Int? = Regex("<$name>\\s*(\\d+)\\s*</$name>").find(set)?.groupValues?.get(1)?.toIntOrNull()

        /**
         * The limit of the set with the largest `minimumRequiredMemTotal` that
         * [phoneMib] reaches, as MemoryLimiter picks it; null when none does or
         * [xml] names none, where the phone has no limit.
         */
        fun parse(xml: String, phoneMib: Long): MemoryLimit? = LIMIT_SET.findAll(COMMENT.replace(xml, ""))
            .map { it.groupValues[1] }
            .mapNotNull { set ->
                val min = tag(set, "minimumRequiredMemTotal") ?: return@mapNotNull null
                val mem = tag(set, "memNotVisible") ?: return@mapNotNull null
                val swap = tag(set, "swapNotVisible") ?: return@mapNotNull null
                if (min > phoneMib) null else min to MemoryLimit(mem, swap)
            }
            .maxByOrNull { it.first }
            ?.second

        /** This phone's limit, or null when it has none or the config can't be read. */
        fun read(context: Context): MemoryLimit? = runCatching {
            val info = ActivityManager.MemoryInfo().also { context.getSystemService(ActivityManager::class.java).getMemoryInfo(it) }
            parse(File(CONFIG).readText(), info.totalMem shr 20)
        }.getOrNull()
    }
}
