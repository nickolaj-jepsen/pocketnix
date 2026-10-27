package dev.nickolaj.pocketnix.gui

import android.graphics.Rect
import android.graphics.Region
import android.hardware.HardwareBuffer
import android.hardware.SyncFence
import android.util.Log
import android.view.SurfaceControl
import java.time.Duration
import java.util.function.Consumer

/**
 * One window on screen: a root layer under the window's SurfaceView, and a
 * child layer per guest surface, fed from a few HardwareBuffers each.
 * Exists while the window's surface does. Every method runs under the
 * session's lock; [onReleased] reaches it from SurfaceFlinger's threads.
 */
internal class WindowRenderer(
    val parent: SurfaceControl,
    private val window: Int,
    private val lock: Any,
    /** The window's commits up to a seq are on screen. */
    private val presented: (window: Int, seq: Int) -> Unit,
    /** A buffer came free while [deferred]: render again. */
    private val onReleased: (WindowRenderer) -> Unit,
) {
    private val root = SurfaceControl.Builder().setName("gui-window-${window.u}").setParent(parent).build()
    private val layers = HashMap<Int, Layer>()
    private var destroyed = false

    /** A surface had no free buffer: the seq to acknowledge once a buffer frees and it's drawn. */
    var deferred = 0
        private set

    private class Layer(val sc: SurfaceControl) {
        val slots = ArrayList<Slot>()

        /** The content version its current buffer shows; 0 for none. */
        var shown = 0L
    }

    private class Slot(val buffer: HardwareBuffer) {
        /** The content version it holds; 0 for none. */
        var serial = 0L
        var busy = false
        var fence: SyncFence? = null
        var stale = false
    }

    /** Sizes the root to the window's [width] × [height]. */
    fun resize(t: SurfaceControl.Transaction, width: Int, height: Int) {
        t.setCrop(root, Rect(0, 0, width, height))
        t.setVisibility(root, true)
    }

    /**
     * Puts [surfaces] (every surface of the window, or those a group
     * touched) into [t], and acknowledges [seq] once SurfaceFlinger has
     * committed it; 0 acknowledges nothing.
     */
    fun render(t: SurfaceControl.Transaction, surfaces: Collection<GuiState.Surface>, seq: Int) {
        if (destroyed) return
        var short = false
        for (s in surfaces) if (!update(t, s)) short = true
        if (short) {
            if (seq != 0) deferred = seq
            return
        }
        val ack = if (seq != 0) seq else deferred
        deferred = 0
        if (ack == 0) return
        // A change of its own, so the transaction is never empty and always reports its commit.
        t.setVisibility(root, true)
        t.addTransactionCommittedListener(Runnable::run) { presented(window, ack) }
    }

    /** Drops a surface's layer. */
    fun remove(t: SurfaceControl.Transaction, surface: Int) {
        val l = layers.remove(surface) ?: return
        t.reparent(l.sc, null)
        drop(l)
    }

    fun destroy() {
        if (destroyed) return
        destroyed = true
        SurfaceControl.Transaction().reparent(root, null).apply()
        for (l in layers.values) drop(l)
        layers.clear()
        root.release()
    }

    /** False when it couldn't show the surface's newest content for want of a buffer. */
    private fun update(t: SurfaceControl.Transaction, s: GuiState.Surface): Boolean {
        val pixels = s.content.pixels
        if (pixels == null) {
            layers[s.id]?.let { t.setVisibility(it.sc, false) }
            return true
        }
        val l = layers.getOrPut(s.id) { Layer(SurfaceControl.Builder().setName("gui-surface-${s.id.u}").setParent(root).build()) }
        val current = s.damage.serial
        if (l.shown != current) {
            val slot = freeSlot(l, s.content) ?: return false
            val copy = s.damage.since(slot.serial)
            slot.fence?.let {
                // Released means SurfaceFlinger is done with it once this signals.
                if (!it.await(Duration.ofMillis(FENCE_WAIT_MS))) Log.w(TAG, "surface ${s.id.u}: release fence still pending")
                it.close()
            }
            slot.fence = null
            val err = GuiNative.copyRects(slot.buffer, pixels, s.content.width, copy, (copy?.size ?: 0) / 4)
            if (err != 0) {
                Log.w(TAG, "surface ${s.id.u}: copy into its buffer failed ($err)")
                return true
            }
            slot.serial = current
            slot.busy = true
            t.setBuffer(l.sc, slot.buffer, null, Consumer { fence -> released(slot, fence) })
            t.setDamageRegion(l.sc, region(s.damage.since(l.shown), s.content.width, s.content.height))
            t.setOpaque(l.sc, s.content.format == Gui.FORMAT_RGBX8888)
            l.shown = current
        }
        val srcW = if (s.srcWidth > 0) s.srcWidth else s.content.width
        val srcH = if (s.srcHeight > 0) s.srcHeight else s.content.height
        val sx = if (s.dstWidth > 0) s.dstWidth.toFloat() / srcW else 1f
        val sy = if (s.dstHeight > 0) s.dstHeight.toFloat() / srcH else 1f
        t.setLayer(l.sc, s.z)
        t.setBufferTransform(l.sc, bufferTransform(s.transform))
        t.setCrop(l.sc, Rect(s.srcX, s.srcY, s.srcX + srcW, s.srcY + srcH))
        t.setScale(l.sc, sx, sy)
        // The crop doesn't move the layer: place the buffer so the crop's corner lands at (x, y).
        t.setPosition(l.sc, s.x - s.srcX * sx, s.y - s.srcY * sy)
        t.setVisibility(l.sc, true)
        return true
    }

    /** A free buffer of the content's size and format, the one needing the least copying; null when all are with SurfaceFlinger. */
    private fun freeSlot(l: Layer, c: SurfaceContent): Slot? {
        val it = l.slots.iterator()
        while (it.hasNext()) {
            val s = it.next()
            if (s.buffer.width != c.width || s.buffer.height != c.height || s.buffer.format != c.format) {
                it.remove()
                if (s.busy) s.stale = true else s.buffer.close()
            }
        }
        l.slots.filter { !it.busy }.maxByOrNull { it.serial }?.let { return it }
        if (l.slots.size >= MAX_SLOTS) return null
        val b = HardwareBuffer.create(c.width, c.height, c.format, 1, USAGE)
        return Slot(b).also { l.slots += it }
    }

    private fun released(slot: Slot, fence: SyncFence) {
        val retry = synchronized(lock) {
            if (slot.stale || destroyed) {
                fence.close()
                slot.buffer.close()
                return
            }
            slot.fence?.close()
            slot.fence = fence
            slot.busy = false
            deferred != 0
        }
        if (retry) onReleased(this)
    }

    private fun drop(l: Layer) {
        l.sc.release()
        for (s in l.slots) if (s.busy) s.stale = true else s.buffer.close()
        l.slots.clear()
    }

    private companion object {
        const val TAG = "GuiRenderer"
        const val MAX_SLOTS = 3
        const val FENCE_WAIT_MS = 100L
        const val USAGE = HardwareBuffer.USAGE_CPU_WRITE_OFTEN or HardwareBuffer.USAGE_GPU_SAMPLED_IMAGE or HardwareBuffer.USAGE_COMPOSER_OVERLAY

        fun region(rects: IntArray?, width: Int, height: Int): Region {
            if (rects == null) return Region(0, 0, width, height)
            val r = Region()
            for (i in 0 until rects.size / 4) {
                r.op(rects[i * 4], rects[i * 4 + 1], rects[i * 4] + rects[i * 4 + 2], rects[i * 4 + 1] + rects[i * 4 + 3], Region.Op.UNION)
            }
            return r
        }

        /**
         * wl_output.transform (what the client applied) as the transform
         * SurfaceFlinger applies to show it: the inverse rotation.
         */
        fun bufferTransform(wl: Int): Int = when (wl) {
            1 -> SurfaceControl.BUFFER_TRANSFORM_ROTATE_90
            2 -> SurfaceControl.BUFFER_TRANSFORM_ROTATE_180
            3 -> SurfaceControl.BUFFER_TRANSFORM_ROTATE_270
            4 -> SurfaceControl.BUFFER_TRANSFORM_MIRROR_HORIZONTAL
            5 -> SurfaceControl.BUFFER_TRANSFORM_MIRROR_VERTICAL or SurfaceControl.BUFFER_TRANSFORM_ROTATE_90
            6 -> SurfaceControl.BUFFER_TRANSFORM_MIRROR_VERTICAL
            7 -> SurfaceControl.BUFFER_TRANSFORM_MIRROR_HORIZONTAL or SurfaceControl.BUFFER_TRANSFORM_ROTATE_90
            else -> SurfaceControl.BUFFER_TRANSFORM_IDENTITY
        }
    }
}
