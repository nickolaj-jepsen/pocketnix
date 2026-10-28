package dev.nickolaj.pocketnix.ui

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.platform.LocalContext

/** The Material theme of every pocketnix activity: the system's dynamic colours. */
@Composable
fun PocketnixTheme(content: @Composable () -> Unit) {
    val context = LocalContext.current
    // The terminal is dark by default; keep the chrome around it dark too.
    val colors = when {
        isSystemInDarkTheme() -> dynamicDarkColorScheme(context)
        else -> runCatching { dynamicLightColorScheme(context) }.getOrDefault(darkColorScheme())
    }
    MaterialTheme(colorScheme = colors, content = content)
}
