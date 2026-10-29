package dev.nickolaj.pocketnix.gui

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import kotlin.math.roundToInt

class WindowFitTest {
    // The bench emulator: 1080 px wide at 420 dpi.
    private val density = 2.625f

    private fun fit(factor: Float, width: Int, height: Int, minWidth: Float, minHeight: Float = 0f) =
        WindowFit.scale(density, factor, width, height, minWidth, minHeight)

    @Test
    fun aWindowThatFitsGetsItsPreset() {
        assertEquals(density, fit(1f, 1080, 2000, 300f, 400f), 0f)
        assertEquals(density * 0.75f, fit(0.75f, 1080, 2000, 300f), 0f)
    }

    @Test
    fun aMinimumTooWideLowersTheScaleUntilItFits() {
        val s = fit(1f, 1080, 2000, 600f)
        assertEquals(1.8f, s, 1e-6f)
        // The compositor configures round(width / scale) logical pixels: exactly the minimum.
        assertEquals(600, (1080 / s).roundToInt())
    }

    @Test
    fun theTighterAxisDecides() {
        assertEquals(1000f / 500f, fit(1f, 1080, 1000, 400f, 500f), 1e-6f)
    }

    @Test
    fun theScaleNeverGoesBelowTheDesktopPresetsFactor() {
        assertEquals(density * SizePreset.DESKTOP.factor, fit(1f, 1080, 2000, 1200f), 0f)
    }

    @Test
    fun aPresetBelowTheFloorIsKept() {
        // Only the presets set the floor; none is below Desktop today, but the fit never raises a scale.
        assertEquals(density * 0.4f, fit(0.4f, 1080, 2000, 100f), 0f)
    }

    @Test
    fun noSizeOrNoMinimumMeansThePreset() {
        assertEquals(density, fit(1f, 0, 0, 900f, 900f), 0f)
        assertEquals(density, fit(1f, 1080, 2000, 0f, 0f), 0f)
    }

    @Test
    fun theFitGoesBackUpWhenTheWindowGrows() {
        val narrow = fit(1f, 800, 2000, 400f)
        val wide = fit(1f, 2000, 2000, 400f)
        assertEquals(2f, narrow, 1e-6f)
        assertEquals(density, wide, 0f)
    }

    @Test
    fun theLogicalMinimumNeverComesOutBelowTheClients() {
        // Sizes on the wire are round(logical × scale): undoing that must give back at least the logical size.
        for (scale in listOf(0.5f, 1f, 1.3125f, 1.96875f, 2.625f, 3.5f)) {
            for (logical in 1..900) {
                val got = WindowFit.logical((logical * scale).roundToInt(), scale)
                assertTrue("$logical at $scale came back as $got", got >= logical && got <= logical + 1)
            }
        }
    }

    @Test
    fun noMinimumIsZero() {
        assertEquals(0f, WindowFit.logical(0, 2f), 0f)
        assertEquals(0f, WindowFit.logical(500, 0f), 0f)
    }
}
