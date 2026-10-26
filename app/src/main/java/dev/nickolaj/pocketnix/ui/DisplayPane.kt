package dev.nickolaj.pocketnix.ui

import android.graphics.Bitmap
import android.graphics.Paint
import android.graphics.PixelFormat
import android.graphics.Rect
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import android.view.SurfaceView
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import dev.nickolaj.pocketnix.Vm
import dev.nickolaj.pocketnix.agent.DisplayStream
import java.nio.ByteBuffer

/**
 * The guest compositor on screen: one display stream, blitted into a
 * SurfaceView. The agent sends only changed bands, so the renderer keeps the
 * last full frame and redraws that.
 */
@Composable
fun DisplayPane(vm: Vm, modifier: Modifier = Modifier) {
    val context = LocalContext.current
    val view = remember {
        SurfaceView(context).apply {
            holder.setFormat(PixelFormat.RGBA_8888)
            // A frame arriving after the surface goes away must not draw into nothing.
            setZOrderOnTop(false)
        }
    }
    val renderer = remember(view) { DisplayRenderer(view) }
    var status by remember { mutableStateOf("waiting for the guest's screen") }
    var size by remember { mutableStateOf("") }

    DisposableEffect(Unit) {
        renderer.status = { status = it }
        renderer.size = { size = it }
        val stream = vm.startDisplay(renderer)
        stream?.start()
        onDispose {
            stream?.close()
            renderer.release()
        }
    }

    Box(modifier.background(Color.Black)) {
        AndroidView(factory = { view }, modifier = Modifier.fillMaxSize())
        Row(
            Modifier.fillMaxWidth().padding(4.dp),
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(if (size.isEmpty()) status else "$status · $size", color = Color.White.copy(alpha = 0.7f), fontSize = 12.sp)
            TextButton(onClick = { vm.setDisplayVisible(false) }) { Text("Close", color = Color.White) }
        }
    }
}

/**
 * Draws frames into the SurfaceView on the main thread. Bands arrive on the
 * reader thread, so they are copied into a frame buffer there into a posted
 * draw: at most one copy and one blit a frame, whatever the band count.
 */
private class DisplayRenderer(private val view: SurfaceView) : DisplayStream.Sink {
    var status: (String) -> Unit = {}
    var size: (String) -> Unit = {}

    private val handler = Handler(Looper.getMainLooper())
    private val paint = Paint(Paint.FILTER_BITMAP_FLAG)
    // Bands are written on the reader thread while the main thread copies the
    // frame into the bitmap, so both sides hold this.
    private val lock = Any()
    private var frame: ByteArray? = null
    private var bitmap: Bitmap? = null
    private var width = 0
    private var height = 0
    private var released = false

    private var drawPending = false
    private var lastDraw = 0L
    private var frames = 0
    private var lastStats = 0L

    private val draw = Runnable {
        drawPending = false
        drawFrame()
    }

    override fun onGeometry(width: Int, height: Int) {
        handler.post {
            if (released || (this.width == width && this.height == height)) return@post
            this.width = width
            this.height = height
            frame = ByteArray(width * height * 4)
            bitmap?.recycle()
            bitmap = Bitmap.createBitmap(width, height, Bitmap.Config.ARGB_8888)
            status("")
            size("${width}x$height")
            lastStats = SystemClock.uptimeMillis()
            frames = 0
            scheduleDraw()
        }
    }

    override fun onBand(y: Int, height: Int, stride: Int, pixels: ByteArray) {
        synchronized(lock) {
            val buf = frame ?: return
            if (stride != width * 4 || y + height > this.height) return
            for (row in 0 until height) System.arraycopy(pixels, row * stride, buf, (y + row) * stride, stride)
        }
        handler.post { if (!released) scheduleDraw() }
    }

    override fun onRect(x: Int, y: Int, width: Int, height: Int, stride: Int, pixels: ByteArray) {
        synchronized(lock) {
            val buf = frame ?: return
            if (stride != width * 4 || x + width > this.width || y + height > this.height) return
            val rowBytes = width * 4
            for (row in 0 until height) {
                System.arraycopy(pixels, row * rowBytes, buf, ((y + row) * this.width + x) * 4, rowBytes)
            }
        }
        handler.post { if (!released) scheduleDraw() }
    }

    private fun scheduleDraw() {
        if (released || drawPending) return
        drawPending = true
        val since = SystemClock.uptimeMillis() - lastDraw
        handler.postDelayed(draw, (MIN_FRAME_MS - since).coerceAtLeast(0))
    }

    private fun drawFrame() {
        val canvas = runCatching { view.holder.lockCanvas() }.getOrNull() ?: return
        try {
            synchronized(lock) {
                val bmp = bitmap ?: return
                bmp.copyPixelsFromBuffer(ByteBuffer.wrap(frame ?: return))
                val dst = fit(canvas.width, canvas.height)
                if (dst != null) canvas.drawBitmap(bmp, null, dst, paint)
            }
        } finally {
            view.holder.unlockCanvasAndPost(canvas)
        }
        lastDraw = SystemClock.uptimeMillis()
        frames++
        val elapsed = lastDraw - lastStats
        if (elapsed >= 1000) {
            size("${width}x$height · ${"%.1f".format(frames * 1000f / elapsed)} fps")
            frames = 0
            lastStats = lastDraw
        }
    }

    /** Where the frame goes: the whole view, aspect kept. */
    private fun fit(viewWidth: Int, viewHeight: Int): Rect? {
        if (viewWidth <= 0 || viewHeight <= 0 || width == 0 || height == 0) return null
        val scale = minOf(viewWidth.toFloat() / width, viewHeight.toFloat() / height)
        val w = (width * scale).toInt()
        val h = (height * scale).toInt()
        val left = (viewWidth - w) / 2
        val top = (viewHeight - h) / 2
        return Rect(left, top, left + w, top + h)
    }

    override fun onEnd(reason: String?) {
        if (released) return
        handler.post { if (!released) status(reason ?: "closed") }
    }

    fun release() {
        released = true
        handler.removeCallbacks(draw)
        synchronized(lock) {
            bitmap?.recycle()
            bitmap = null
            frame = null
        }
    }

    private companion object {
        const val MIN_FRAME_MS = 16L
    }
}
