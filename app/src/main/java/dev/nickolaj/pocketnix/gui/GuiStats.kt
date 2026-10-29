package dev.nickolaj.pocketnix.gui

import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicLong
import kotlin.math.ceil
import kotlin.math.ln
import kotlin.math.pow
import org.json.JSONArray
import org.json.JSONObject

/**
 * Counters and latency histograms of the GUI link's work, for benchmarks
 * (`pocketnix control stats`). Every number only grows: a reader takes two
 * snapshots and subtracts them. The snapshot's shape is the compositor's
 * stats socket's (compositor/src/stats.rs), histogram buckets included.
 */
object GuiStats {
    private val counters = ConcurrentHashMap<String, AtomicLong>()
    private val histograms = ConcurrentHashMap<String, Histogram>()

    // Per window, when the first input frame since its last shown group went out.
    private val inputAt = ConcurrentHashMap<Int, Long>()

    fun add(name: String, n: Long = 1) {
        counters.getOrPut(name) { AtomicLong() }.addAndGet(n)
    }

    fun record(name: String, ns: Long) {
        histograms.getOrPut(name) { Histogram() }.record(ns)
    }

    /** An input frame for [window] went to the compositor. */
    fun input(window: Int) {
        inputAt.putIfAbsent(window, System.nanoTime())
    }

    /**
     * A group for [window], received at [receivedNs], is on screen: if an
     * input came before it, times input → shown.
     */
    fun shown(window: Int, receivedNs: Long) {
        val at = inputAt[window] ?: return
        if (at < receivedNs && inputAt.remove(window, at)) record("inputToCommitted", System.nanoTime() - at)
    }

    fun json(): JSONObject = JSONObject()
        .put("source", "app")
        .put("counters", JSONObject().also { o -> for ((k, v) in counters) o.put(k, v.get()) })
        .put("histograms", JSONObject().also { o -> for ((k, h) in histograms) o.put(k, h.json()) })

    private class Histogram {
        private val buckets = LongArray(BUCKETS)
        private var count = 0L
        private var sumUs = 0L

        @Synchronized fun record(ns: Long) {
            val us = (ns / 1000).coerceAtLeast(0)
            val i = if (us <= 1) 0 else (ln(us.toDouble()) / LN2 * PER_OCTAVE).toInt()
            buckets[i.coerceAtMost(BUCKETS - 1)]++
            count++
            sumUs += us
        }

        @Synchronized fun json(): JSONObject {
            val b = JSONArray()
            for (i in buckets.indices) if (buckets[i] > 0) b.put(JSONArray().put(bound(i)).put(buckets[i]))
            return JSONObject().put("count", count).put("sumUs", sumUs).put("buckets", b)
        }

        private fun bound(i: Int) = ceil(2.0.pow((i + 1) / PER_OCTAVE)).toLong()
    }

    // As the compositor's: buckets per power of two, and how many.
    private const val PER_OCTAVE = 4.0
    private const val BUCKETS = 112
    private val LN2 = ln(2.0)
}
