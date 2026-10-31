package dev.nickolaj.pocketnix.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.LocalContentColor
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import dev.nickolaj.pocketnix.Settings
import dev.nickolaj.pocketnix.Vm
import dev.nickolaj.pocketnix.engine.DiskImage
import dev.nickolaj.pocketnix.managed.ManagedGuest
import dev.nickolaj.pocketnix.managed.SERVICES
import dev.nickolaj.pocketnix.managed.Selections

/**
 * First run without a disk: who the user is and what the guest runs, then
 * the release's base image becomes the disk and the VM boots, if by then
 * nothing else is missing. The guest's first rebuild applies the rest.
 */
@Composable
fun SetupCard(vm: Vm, modifier: Modifier = Modifier) {
    val context = LocalContext.current
    val setup by vm.managed.setup.collectAsState()
    var user by rememberSaveable { mutableStateOf("") }
    var services by rememberSaveable { mutableStateOf(setOf("ssh")) }
    var disk by rememberSaveable { mutableStateOf(vm.settings.diskGb.toString()) }
    val maxDisk = remember { DiskImage.maxGb(context) }
    val userError = if (user.isEmpty() || Selections.USER_NAME.matches(user)) null else "Lowercase letters, digits, - and _"
    val diskSize = checkDiskSize(disk, Settings.MIN_SETUP_DISK_GB, maxDisk)
    val diskError = when (diskSize) {
        DiskSize.NoRoom -> "Not enough free space: needs ${Settings.MIN_SETUP_DISK_GB} GB, the phone has $maxDisk GB"
        DiskSize.OutOfRange -> "${Settings.MIN_SETUP_DISK_GB}–$maxDisk GB"
        is DiskSize.Ok -> null
    }
    val working = setup is ManagedGuest.Setup.Working
    Column(modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text("Set up NixOS", style = MaterialTheme.typography.titleMedium)
        Text(
            "pocketnix downloads a NixOS image, then the guest configures itself. You can change all of it later under Configure.",
            color = LocalContentColor.current.copy(alpha = 0.8f),
            fontSize = 13.sp
        )
        OutlinedTextField(
            user,
            { user = it.lowercase().take(32) },
            label = { Text("User name") },
            singleLine = true,
            enabled = !working,
            isError = userError != null,
            supportingText = userError?.let { { Text(it) } },
            modifier = Modifier.fillMaxWidth()
        )
        for (s in SERVICES) {
            Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                Column(Modifier.weight(1f)) {
                    Text(s.label)
                    Text(s.detail, color = LocalContentColor.current.copy(alpha = 0.7f), fontSize = 12.sp)
                }
                Switch(s.id in services, { on -> services = if (on) services + s.id else services - s.id }, enabled = !working)
            }
        }
        OutlinedTextField(
            disk,
            { v -> disk = v.filter(Char::isDigit).take(5) },
            label = { Text("Disk (GB)") },
            singleLine = true,
            enabled = !working && diskSize != DiskSize.NoRoom,
            isError = diskError != null,
            supportingText = diskError?.let { { Text(it) } },
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number)
        )
        when (val s = setup) {
            is ManagedGuest.Setup.Working -> {
                val what = if (s.unpacking) "Unpacking" else "Downloading"
                Text(if (s.total > 0) "$what: ${megabytes(s.done)} of ${megabytes(s.total)}" else "$what…")
                if (s.total > 0) LinearProgressIndicator(progress = { s.done.toFloat() / s.total }, modifier = Modifier.fillMaxWidth())
            }

            is ManagedGuest.Setup.Failed -> Text("Failed: ${s.message}", color = MaterialTheme.colorScheme.error)

            ManagedGuest.Setup.Idle -> {}
        }
        Button(
            enabled = !working && user.isNotEmpty() && userError == null && diskSize is DiskSize.Ok,
            onClick = {
                vm.settings.diskGb = (diskSize as DiskSize.Ok).gb
                // Asked when the download ends: the permissions may have come meanwhile.
                vm.managed.install(Selections(user, services)) { if (missingRequirements(context, vm).isEmpty()) vm.start() }
            }
        ) { Text(if (setup is ManagedGuest.Setup.Failed) "Try again" else "Download and set up") }
        Text(
            "Or bring your own NixOS flake from a computer: `pocketnix provision --image …` (see the docs).",
            color = LocalContentColor.current.copy(alpha = 0.6f),
            fontSize = 12.sp
        )
    }
}
