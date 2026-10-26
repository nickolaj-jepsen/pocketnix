package dev.nickolaj.pocketnix

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.ui.platform.LocalContext
import dev.nickolaj.pocketnix.ui.MainScreen

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        setContent {
            val context = LocalContext.current
            // The terminal is dark by default; keep the chrome around it dark too.
            val colors = when {
                isSystemInDarkTheme() -> dynamicDarkColorScheme(context)
                else -> runCatching { dynamicLightColorScheme(context) }.getOrDefault(darkColorScheme())
            }
            MaterialTheme(colorScheme = colors) {
                MainScreen(vm)
            }
        }
    }
}
