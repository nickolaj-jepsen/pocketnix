package dev.nickolaj.pocketnix.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import dev.nickolaj.pocketnix.BuildConfig
import dev.nickolaj.pocketnix.Settings
import dev.nickolaj.pocketnix.Vm
import dev.nickolaj.pocketnix.engine.DiskImage
import dev.nickolaj.pocketnix.engine.VmState

/** The set-once hardware; changes apply at the next start. */
@Composable
fun SettingsDialog(vm: Vm, onDismiss: () -> Unit) {
    val s = vm.settings
    val context = LocalContext.current
    var memory by remember { mutableStateOf(s.memoryMb.toString()) }
    var disk by remember { mutableStateOf(s.diskGb.toString()) }
    var allCpus by remember { mutableStateOf(s.cpus != 1) }
    var network by remember { mutableStateOf(s.network) }
    var onBoot by remember { mutableStateOf(s.startOnBoot) }
    var wakeLocks by remember { mutableStateOf(s.wakeLocks) }
    var tcp by remember { mutableStateOf(s.tcpBackend) }
    var tcpAddress by remember { mutableStateOf(s.tcpAddress) }
    var releaseUrl by remember { mutableStateOf(s.releaseUrl) }
    var sharedFolders by remember { mutableStateOf(false) }
    val state by vm.state.collectAsState()
    val stopped = state == VmState.Stopped || state is VmState.Failed
    val maxDisk = remember { DiskImage.maxGb(context) }
    val memoryError = memory.toIntOrNull().let { if (it == null || it !in MIN_MEMORY_MB..Settings.MAX_MEMORY_MB) "$MIN_MEMORY_MB–${Settings.MAX_MEMORY_MB} MB" else null }
    // The saved value stays valid even when free space has shrunk below it since.
    val diskError = disk.toIntOrNull().let {
        when {
            it == null || it < MIN_DISK_GB -> "At least $MIN_DISK_GB GB"
            it > maxDisk && it != s.diskGb -> "At most $maxDisk GB: that's the free space"
            else -> null
        }
    }

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("VM settings") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                if (!stopped) Text("Changes apply when the VM next starts or the guest reboots.")
                NumberField("Memory (MB)", memory, memoryError) { memory = it }
                NumberField("Disk (GB, only grows)", disk, diskError) { disk = it }
                Toggle("One vCPU per host core", allCpus) { allCpus = it }
                Toggle("Network", network) { network = it }
                Toggle("Start when the phone boots", onBoot) { onBoot = it }
                Toggle("Stay awake while the guest inhibits sleep", wakeLocks) { wakeLocks = it }
                TextButton(onClick = { sharedFolders = true }) { Text("Android folders in the guest…") }
                if (BuildConfig.DEBUG) {
                    Toggle("TCP agent backend (debug)", tcp) { tcp = it }
                    if (tcp) {
                        OutlinedTextField(tcpAddress, { tcpAddress = it }, label = { Text("Agent host:port") }, singleLine = true)
                    }
                    OutlinedTextField(releaseUrl, { releaseUrl = it }, label = { Text("Release files URL (debug)") }, singleLine = true)
                }
            }
        },
        confirmButton = {
            TextButton(enabled = memoryError == null && diskError == null, onClick = {
                s.memoryMb = memory.toInt()
                s.diskGb = disk.toInt()
                s.cpus = if (allCpus) 0 else 1
                s.network = network
                s.startOnBoot = onBoot
                vm.setWakeLocks(wakeLocks)
                s.tcpBackend = tcp
                s.tcpAddress = tcpAddress.trim()
                s.releaseUrl = releaseUrl.trim()
                vm.reloadEngine()
                onDismiss()
            }) { Text("Save") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } }
    )
    if (sharedFolders) SharedFoldersDialog(vm) { sharedFolders = false }
}

private const val MIN_MEMORY_MB = 512
private const val MIN_DISK_GB = 4

@Composable
private fun NumberField(label: String, value: String, error: String?, onChange: (String) -> Unit) {
    OutlinedTextField(
        value,
        { v -> onChange(v.filter(Char::isDigit).take(9)) },
        label = { Text(label) },
        singleLine = true,
        isError = error != null,
        supportingText = error?.let { { Text(it) } },
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number)
    )
}

@Composable
private fun Toggle(label: String, value: Boolean, onChange: (Boolean) -> Unit) {
    Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
        Text(label, Modifier.weight(1f))
        Switch(value, onChange)
    }
}
