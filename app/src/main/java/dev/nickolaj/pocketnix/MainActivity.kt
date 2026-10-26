package dev.nickolaj.pocketnix

import android.content.Context
import android.content.pm.PackageManager
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        setContent {
            val context = LocalContext.current
            val colors = if (isSystemInDarkTheme()) dynamicDarkColorScheme(context) else dynamicLightColorScheme(context)
            MaterialTheme(colorScheme = colors) {
                Scaffold { padding ->
                    Requirements(Modifier.padding(padding).padding(16.dp).fillMaxSize())
                }
            }
        }
    }
}

/** One prerequisite of booting a VM, with the fix shown when it's missing. */
private data class Requirement(val name: String, val met: Boolean, val fix: String)

private fun requirements(context: Context): List<Requirement> {
    val pm = context.packageManager
    fun granted(permission: String) = context.checkSelfPermission(permission) == PackageManager.PERMISSION_GRANTED
    val grant = "run `pocketnix provision`, or adb shell pm grant ${context.packageName}"
    return listOf(
        Requirement(
            "Android Virtualization Framework",
            // PackageManager.FEATURE_VIRTUALIZATION_FRAMEWORK is @SystemApi, so spell it out.
            pm.hasSystemFeature("android.software.virtualization_framework"),
            "this device has no AVF",
        ),
        Requirement(
            "MANAGE_VIRTUAL_MACHINE",
            granted("android.permission.MANAGE_VIRTUAL_MACHINE"),
            "$grant android.permission.MANAGE_VIRTUAL_MACHINE",
        ),
        Requirement(
            "USE_CUSTOM_VIRTUAL_MACHINE",
            granted("android.permission.USE_CUSTOM_VIRTUAL_MACHINE"),
            "$grant android.permission.USE_CUSTOM_VIRTUAL_MACHINE",
        ),
    )
}

@Composable
private fun Requirements(modifier: Modifier = Modifier) {
    val context = LocalContext.current
    Column(modifier, verticalArrangement = Arrangement.spacedBy(12.dp)) {
        Text("pocketnix", style = MaterialTheme.typography.headlineMedium)
        for (r in requirements(context)) {
            Text("${if (r.met) "✓" else "✗"} ${r.name}", style = MaterialTheme.typography.titleMedium)
            if (!r.met) Text(r.fix, style = MaterialTheme.typography.bodySmall)
        }
    }
}
