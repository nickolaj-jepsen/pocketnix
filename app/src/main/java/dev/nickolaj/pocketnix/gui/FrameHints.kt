package dev.nickolaj.pocketnix.gui

import android.content.Context
import android.os.PerformanceHintManager
import android.os.Process
import android.util.Log

/**
 * Tells Android's performance hint service (ADPF) how long each frame's
 * work took on the calling thread and the helpers it waits on, against the
 * display's frame interval: the power HAL moves threads that run over to
 * faster cores and clocks, and lowers them for work well under. Does
 * nothing where the device has no hint support. Used from one thread at a
 * time.
 */
internal class FrameHints(context: Context) {
    private val manager = context.getSystemService(PerformanceHintManager::class.java)
    private var session: PerformanceHintManager.Session? = null

    // The thread and target the session was made for; a failed attempt counts, so it isn't retried every frame.
    private var tid = 0
    private var targetNs = 0L
    private var helpers = IntArray(0)

    /**
     * One frame's work, [workNs] long, on the calling thread and the
     * [helpers] it waited on, for a display showing a frame every [frameNs].
     */
    fun report(workNs: Long, frameNs: Long, helpers: IntArray) {
        if (manager == null || workNs <= 0 || frameNs <= 0) return
        val me = Process.myTid()
        if (me != tid) {
            close()
            tid = me
            targetNs = frameNs
            this.helpers = helpers
            session = runCatching { manager.createHintSession(intArrayOf(me) + helpers, frameNs) }
                .onFailure { Log.w(TAG, "createHintSession", it) }.getOrNull()
            Log.i(TAG, if (session != null) "hint session for $me and ${helpers.size} helpers at ${frameNs / 1000} µs a frame" else "no hint session")
        } else {
            if (frameNs != targetNs) {
                targetNs = frameNs
                session?.updateTargetWorkDuration(frameNs)
            }
            if (!helpers.contentEquals(this.helpers)) {
                this.helpers = helpers
                session?.let { s -> runCatching { s.setThreads(intArrayOf(me) + helpers) }.onFailure { Log.w(TAG, "setThreads", it) } }
            }
        }
        session?.reportActualWorkDuration(workNs)
    }

    /** Ends the session; the next [report] starts another. */
    fun close() {
        session?.close()
        session = null
        tid = 0
        helpers = IntArray(0)
    }

    private companion object {
        const val TAG = "FrameHints"
    }
}
