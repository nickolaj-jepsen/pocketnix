package dev.nickolaj.pocketnix.terminal

import com.termux.terminal.TerminalColors
import com.termux.terminal.TextStyle
import java.util.Properties

/** The module's terminal colors, applied to Termux's process-wide default palette. */
object TerminalTheme {
    private val key = Regex("foreground|background|cursor|color([0-9]|[1-9][0-9]|1[0-9][0-9]|2[0-4][0-9]|25[0-5])")
    private val value = Regex("#[0-9a-fA-F]{6}")

    /** The entries Termux's scheme accepts; others are dropped rather than failing the whole theme. */
    fun properties(colors: Map<String, String>): Properties =
        Properties().apply {
            for ((k, v) in colors) if (key.matches(k) && value.matches(v)) setProperty(k, v)
        }

    /**
     * Makes [colors] the default palette for new emulators. Existing ones
     * pick it up through their own `mColors.reset()`.
     */
    fun apply(colors: Map<String, String>) {
        TerminalColors.COLOR_SCHEME.updateWith(properties(colors))
    }

    /** The current default background, as an ARGB int. */
    val background: Int get() = TerminalColors.COLOR_SCHEME.mDefaultColors[TextStyle.COLOR_INDEX_BACKGROUND]
    val foreground: Int get() = TerminalColors.COLOR_SCHEME.mDefaultColors[TextStyle.COLOR_INDEX_FOREGROUND]
}
