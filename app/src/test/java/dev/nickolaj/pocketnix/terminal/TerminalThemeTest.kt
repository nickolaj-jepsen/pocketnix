package dev.nickolaj.pocketnix.terminal

import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Test

class TerminalThemeTest {
    @After
    fun reset() = TerminalTheme.apply(emptyMap())

    @Test
    fun dropsWhatTermuxWouldReject() {
        val p = TerminalTheme.properties(
            mapOf(
                "background" to "#1e1e2e",
                "color15" to "#a6adc8",
                "color255" to "#000000",
                "color256" to "#000000",
                "colour1" to "#ffffff",
                "foreground" to "white",
            ),
        )
        assertEquals(setOf("background", "color15", "color255"), p.stringPropertyNames())
    }

    @Test
    fun appliesTheBackground() {
        TerminalTheme.apply(mapOf("background" to "#1e1e2e", "foreground" to "#cdd6f4"))
        assertEquals(0xff1e1e2e.toInt(), TerminalTheme.background)
        assertEquals(0xffcdd6f4.toInt(), TerminalTheme.foreground)
    }
}
