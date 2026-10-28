package dev.nickolaj.pocketnix.gui

import android.content.Context
import android.graphics.Bitmap
import android.graphics.Color
import android.view.Gravity
import android.widget.Button
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.ProgressBar
import android.widget.TextView
import dev.nickolaj.pocketnix.R

/**
 * A launch's splash: the app's icon and name, what's happening, and the
 * ways out. Black like the window theme's blank system splash, so the two
 * show no seam.
 */
internal class SplashView(context: Context, onCancel: () -> Unit, onRetry: () -> Unit, onClose: () -> Unit) : LinearLayout(context) {
    private val icon = ImageView(context)
    private val name = TextView(context)
    private val status = TextView(context)
    private val progress = ProgressBar(context)
    private val cancel = button(context.getString(android.R.string.cancel), onCancel)
    private val retry = button("Retry", onRetry)
    private val close = button("Close", onClose)

    init {
        orientation = VERTICAL
        gravity = Gravity.CENTER
        setBackgroundColor(Color.BLACK)
        val pad = dp(24)
        setPadding(pad, pad, pad, pad)
        val side = dp(96)
        addView(icon, LayoutParams(side, side))
        name.setTextColor(Color.WHITE)
        name.textSize = 22f
        name.gravity = Gravity.CENTER
        addView(name, LayoutParams(LayoutParams.WRAP_CONTENT, LayoutParams.WRAP_CONTENT).apply { topMargin = dp(16) })
        status.setTextColor(Color.argb(0xb0, 0xff, 0xff, 0xff))
        status.textSize = 15f
        status.gravity = Gravity.CENTER
        addView(status, LayoutParams(LayoutParams.WRAP_CONTENT, LayoutParams.WRAP_CONTENT).apply { topMargin = dp(8) })
        addView(progress, LayoutParams(dp(32), dp(32)).apply { topMargin = dp(24) })
        val buttons = LinearLayout(context).apply {
            gravity = Gravity.CENTER
            addView(cancel)
            addView(retry)
            addView(close)
        }
        addView(buttons, LayoutParams(LayoutParams.WRAP_CONTENT, LayoutParams.WRAP_CONTENT).apply { topMargin = dp(16) })
        show(SplashStatus(""))
    }

    fun app(label: String, bitmap: Bitmap?) {
        name.text = label
        if (bitmap != null) icon.setImageBitmap(bitmap) else icon.setImageResource(R.drawable.ic_guest_app)
    }

    fun show(s: SplashStatus) {
        status.text = s.text
        progress.visibility = if (s.failed) GONE else VISIBLE
        cancel.visibility = if (s.failed) GONE else VISIBLE
        retry.visibility = if (s.failed && s.canRetry) VISIBLE else GONE
        close.visibility = if (s.failed) VISIBLE else GONE
    }

    private fun button(text: String, onClick: () -> Unit) = Button(context, null, android.R.attr.borderlessButtonStyle).apply {
        this.text = text
        setTextColor(Color.WHITE)
        setOnClickListener { guard("splash: $text") { onClick() } }
    }

    private fun dp(v: Int) = (v * resources.displayMetrics.density).toInt()
}
