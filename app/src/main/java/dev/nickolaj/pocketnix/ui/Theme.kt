package dev.nickolaj.pocketnix.ui

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.ColorScheme
import androidx.compose.material3.LocalContentColor
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.luminance
import androidx.compose.ui.platform.LocalContext

/** The Material theme of the launcher and GUI windows: the system's dynamic colours. */
@Composable
fun PocketnixTheme(content: @Composable () -> Unit) {
    val context = LocalContext.current
    val colors = if (isSystemInDarkTheme()) dynamicDarkColorScheme(context) else dynamicLightColorScheme(context)
    MaterialTheme(colorScheme = colors, content = content)
}

/**
 * The main screen's theme: [terminalColorScheme] over the system's dynamic
 * schemes, with [foreground] as the content colour outside any `Surface`.
 */
@Composable
fun TerminalChromeTheme(background: Int, foreground: Int, content: @Composable () -> Unit) {
    val context = LocalContext.current
    val colors = terminalColorScheme(dynamicLightColorScheme(context), dynamicDarkColorScheme(context), background, foreground)
    MaterialTheme(colorScheme = colors) {
        CompositionLocalProvider(LocalContentColor provides colors.onBackground, content = content)
    }
}

/** Whether light text reads better than dark on [background] (ARGB). */
fun isDarkBackground(background: Int): Boolean = Color(background).luminance() < CONTRAST_CROSSOVER

/**
 * [dark] or [light], whichever suits the terminal's ARGB [background], with
 * its surfaces and their text taken from the terminal: surfaces are
 * [background], containers are it lifted toward [foreground], and text on
 * them is [foreground]. Accents (primary, error, outline…) stay the base
 * scheme's.
 */
fun terminalColorScheme(light: ColorScheme, dark: ColorScheme, background: Int, foreground: Int): ColorScheme {
    val bg = Color(background)
    val fg = Color(foreground)

    // Mixed in sRGB: Compose's lerp mixes in Oklab, where a tenth of the way up from black is still black.
    fun lift(f: Float) = Color(bg.red + (fg.red - bg.red) * f, bg.green + (fg.green - bg.green) * f, bg.blue + (fg.blue - bg.blue) * f)
    return (if (isDarkBackground(background)) dark else light).copy(
        background = bg,
        onBackground = fg,
        surface = bg,
        onSurface = fg,
        surfaceVariant = lift(0.18f),
        onSurfaceVariant = fg,
        surfaceContainerLowest = bg,
        surfaceContainerLow = lift(0.04f),
        surfaceContainer = lift(0.10f),
        surfaceContainerHigh = lift(0.14f),
        surfaceContainerHighest = lift(0.18f)
    )
}

// The relative luminance at which white and black text contrast equally (WCAG): sqrt(1.05 * 0.05) - 0.05.
private const val CONTRAST_CROSSOVER = 0.179f
