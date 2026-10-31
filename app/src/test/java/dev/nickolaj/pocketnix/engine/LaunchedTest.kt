package dev.nickolaj.pocketnix.engine

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class LaunchedTest {
    @Test
    fun theEngineLineSaysWhatTheLaunchGot() {
        val line = engineLine(Launched(gpu = "2d", gpuAsked = true, gpuRefused = true, guestRam = true, virtioSnd = false))
        assertTrue(line.startsWith("engine "))
        for (token in listOf("gpu=2d", "gpuAsked=true", "gpuRefused=true", "guestRam=true", "virtioSnd=false")) {
            assertTrue(token, line.split(' ').contains(token))
        }
    }

    @Test
    fun noLaunchIsNotAGpuOrGuestRam() {
        val line = engineLine(null)
        assertTrue(line.startsWith("engine "))
        assertFalse(line.contains("gpu="))
        assertFalse(line.contains("guestRam="))
    }
}
