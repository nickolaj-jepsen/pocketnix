package dev.nickolaj.pocketnix.ui

import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.luminance
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class TerminalColorSchemeTest {
    private val light = lightColorScheme()
    private val dark = darkColorScheme()
    private val black = 0xff000000.toInt()
    private val white = 0xffffffff.toInt()
    private val solarizedBase3 = 0xfffdf6e3.toInt()
    private val solarizedBase01 = 0xff586e75.toInt()

    @Test
    fun aBlackTerminalPicksTheDarkBase() {
        assertEquals(dark.primary, terminalColorScheme(light, dark, black, white).primary)
    }

    @Test
    fun aLightTerminalPicksTheLightBase() {
        assertEquals(light.primary, terminalColorScheme(light, dark, solarizedBase3, solarizedBase01).primary)
    }

    @Test
    fun surfacesAreTheTerminalBackground() {
        val s = terminalColorScheme(light, dark, solarizedBase3, solarizedBase01)
        assertEquals(Color(0xfffdf6e3), s.background)
        assertEquals(Color(0xfffdf6e3), s.surface)
    }

    @Test
    fun textIsTheTerminalForeground() {
        val s = terminalColorScheme(light, dark, solarizedBase3, solarizedBase01)
        assertEquals(Color(0xff586e75), s.onSurface)
        assertEquals(Color(0xff586e75), s.onBackground)
    }

    @Test
    fun dialogContainersAreTheBackgroundLiftedTowardTheForeground() {
        val onLight = terminalColorScheme(light, dark, solarizedBase3, solarizedBase01).surfaceContainerHigh
        assertTrue(onLight.luminance() < Color(solarizedBase3).luminance())
        assertTrue(onLight.luminance() > (Color(solarizedBase3).luminance() + Color(solarizedBase01).luminance()) / 2)

        val onBlack = terminalColorScheme(light, dark, black, white).surfaceContainerHigh
        assertTrue(onBlack.luminance() > 0f)
        assertTrue(onBlack.luminance() < 0.5f)
    }
}
