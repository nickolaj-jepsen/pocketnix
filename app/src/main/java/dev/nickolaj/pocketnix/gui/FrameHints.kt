package dev.nickolaj.pocketnix.gui

import android.content.Context
import android.os.PerformanceHintManager
import android.os.Process
import android.util.Log

/**
 * Tells Android's performance hint service (ADPF) how long each frame's
 * work took on the calling thread, against the display's frame interval:
 * the power HAL moves a thread that runs over to faster cores and clocks,
 * and lowers them for one well under. Does nothing where the device has no
 * hint support. Used from one thread at a time.
 */
internal class FrameHints(context: Context) {
    private val manager = context.getSystemService(PerformanceHintManager::class.java)
    private var session: PerformanceHintManager.Session? = null
    // The thread and target the session was made for; a failed attempt counts, so it isn't retried every frame.
    private var tid = 0
    private var targetNs = 0L

    /** One frame's work, [workNs] long, on the calling thread, for a display showing a frame every [frameNs]. */
    fun report(workNs: Long, frameNs: Long) {
        if (manager == null || workNs <= 0 || frameNs <= 0) return
        val me = Process.myTid()
        if (me != tid) {
            close()
            tid = me
            targetNs = frameNs
            session = runCatching { manager.createHintSession(intArrayOf(me), frameNs) }
                .onFailure { Log.w(TAG, "createHintSession", it) }.getOrNull()
            Log.i(TAG, if (session != null) "hint session for $me at ${frameNs / 1000} µs a frame" else "no hint session")
        } else if (frameNs != targetNs) {
            targetNs = frameNs
            session?.updateTargetWorkDuration(frameNs)
        }
        session?.reportActualWorkDuration(workNs)
    }

    /** Ends the session; the next [report] starts another. */
    fun close() {
        session?.close()
        session = null
        tid = 0
    }

    private companion object {
        const val TAG = "FrameHints"
    }
}
