package dev.nickolaj.pocketnix.gui

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class ZoomTest {
    private fun zoom(view: Int = 1000, content: Int = view) = Zoom().apply { layout(view, view, content, content) }

    @Test
    fun unzoomedViewAndWindowCoincide() {
        val z = zoom()
        assertFalse(z.zoomed)
        assertEquals(123f, z.windowX(123f), 0f)
        assertEquals(456f, z.windowY(456f), 0f)
    }

    @Test
    fun theFocusStaysOnTheSameWindowPoint() {
        val z = zoom()
        val before = z.windowX(300f) to z.windowY(700f)
        z.zoomBy(2f, 300f, 700f)
        assertTrue(z.zoomed)
        assertEquals(before.first, z.windowX(300f), 1e-3f)
        assertEquals(before.second, z.windowY(700f), 1e-3f)
        // Everything else spreads around it.
        assertEquals(300f + 100f / 2, z.windowX(400f), 1e-3f)
    }

    @Test
    fun aZoomedTapMapsBackThroughTheTransform() {
        val z = zoom()
        z.zoomBy(2f, 0f, 0f)
        z.panBy(-100f, -100f)
        assertEquals(220f, z.windowX(340f), 1e-3f)
        assertEquals(220f, z.windowY(340f), 1e-3f)
    }

    @Test
    fun panningStopsAtTheWindowsEdges() {
        val z = zoom()
        z.zoomBy(2f, 0f, 0f)
        z.panBy(500f, 500f)
        assertEquals("the window's left edge stays at the view's", 0f, z.windowX(0f), 1e-3f)
        z.panBy(-5000f, 0f)
        assertEquals("its right edge stays at the view's", 1000f, z.windowX(1000f), 1e-3f)
    }

    @Test
    fun zoomStaysWithinItsLimits() {
        val z = zoom()
        z.zoomBy(100f, 0f, 0f)
        assertEquals(Zoom.MAX_SCALE, z.scale, 0f)
        z.zoomBy(0.001f, 0f, 0f)
        assertEquals("a window the view holds never shows smaller than 1:1", 1f, z.scale, 0f)
    }

    @Test
    fun aClippedWindowPansAt1to1AndZoomsOutToShowWhole() {
        val z = zoom(view = 1000, content = 2000)
        assertTrue(z.panBy(-600f, -600f))
        assertEquals(600f, z.windowX(0f), 1e-3f)
        z.zoomBy(0.01f, 500f, 500f)
        assertEquals(0.5f, z.scale, 0f)
        assertEquals(0f, z.windowX(0f), 1e-3f)
        assertEquals(2000f, z.windowX(1000f), 1e-3f)
    }

    @Test
    fun contentSmallerThanTheViewIsCentred() {
        val z = Zoom()
        z.layout(1000, 500, 2000, 500)
        z.zoomBy(0.5f, 0f, 0f)
        assertEquals("half the window's height fills half the view, centred", 0f, z.windowY(125f), 1e-3f)
    }

    @Test
    fun aSmallerViewKeepsTheZoomInsideIt() {
        val z = zoom()
        z.zoomBy(2f, 1000f, 1000f)
        assertTrue("the window shrank under the zoomed corner", z.layout(500, 500, 500, 500))
        assertEquals(500f, z.windowX(500f), 1e-3f)
    }

    @Test
    fun resetGoesBackTo1to1() {
        val z = zoom()
        z.zoomBy(3f, 500f, 500f)
        assertTrue(z.reset())
        assertFalse(z.zoomed)
        assertFalse("nothing left to reset", z.reset())
    }

    @Test
    fun aMoveThatChangesNothingSaysSo() {
        val z = zoom()
        assertFalse(z.panBy(10f, 10f))
        assertFalse(z.zoomBy(1f, 0f, 0f))
        assertFalse(z.zoomBy(Float.NaN, 0f, 0f))
    }
}
