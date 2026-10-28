package dev.nickolaj.pocketnix.gui

import android.content.Context
import android.graphics.Canvas
import android.graphics.Paint
import android.graphics.Path
import android.view.View

/**
 * Trackpad mode's cursor, which Android doesn't draw for a touchscreen:
 * an arrow, an I-beam for text, or nothing for `hidden` (gui-protocol.md,
 * Pointer and Trackpad modes). Place its hotspot with [moveTo].
 */
internal class TrackpadCursor(context: Context) : View(context) {
    private val dp = context.resources.displayMetrics.density
    private val fill = Paint(Paint.ANTI_ALIAS_FLAG).apply { color = 0xffffffff.toInt(); style = Paint.Style.FILL }
    private val stroke = Paint(Paint.ANTI_ALIAS_FLAG).apply {
        color = 0xff000000.toInt()
        style = Paint.Style.STROKE
        strokeWidth = 1.5f * dp
        strokeJoin = Paint.Join.ROUND
    }
    private val arrow = Path().apply {
        moveTo(0f, 0f)
        lineTo(0f, 17f * dp)
        lineTo(4f * dp, 13f * dp)
        lineTo(7f * dp, 20f * dp)
        lineTo(9.5f * dp, 19f * dp)
        lineTo(6.5f * dp, 12f * dp)
        lineTo(12f * dp, 12f * dp)
        close()
    }
    private val beam = Path().apply {
        moveTo(-3f * dp, -9f * dp)
        lineTo(3f * dp, -9f * dp)
        moveTo(0f, -9f * dp)
        lineTo(0f, 9f * dp)
        moveTo(-3f * dp, 9f * dp)
        lineTo(3f * dp, 9f * dp)
    }

    /** The `cursor` message's name. */
    var shape = "default"
        set(v) {
            if (field == v) return
            field = v
            invalidate()
        }

    // The hotspot's offset in the view, which is big enough for either shape around it.
    private val hotspot = 12f * dp

    init {
        val size = (hotspot * 3).toInt()
        layoutParams = android.widget.FrameLayout.LayoutParams(size, size)
        isClickable = false
        isFocusable = false
        importantForAccessibility = IMPORTANT_FOR_ACCESSIBILITY_NO
    }

    /** Puts the hotspot at ([x], [y]) in the parent. */
    fun moveTo(x: Float, y: Float) {
        translationX = x - hotspot
        translationY = y - hotspot
    }

    override fun onDraw(canvas: Canvas) {
        canvas.translate(hotspot, hotspot)
        when (shape) {
            "hidden" -> {}
            "text", "vertical-text" -> {
                val white = Paint(stroke).apply { color = 0xffffffff.toInt(); strokeWidth = 3.5f * dp }
                canvas.drawPath(beam, white)
                canvas.drawPath(beam, stroke)
            }
            else -> {
                canvas.drawPath(arrow, fill)
                canvas.drawPath(arrow, stroke)
            }
        }
    }
}
