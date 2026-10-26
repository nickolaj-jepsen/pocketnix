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
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.compose.ui.platform.LocalContext
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
    var tcp by remember { mutableStateOf(s.tcpBackend) }
    var tcpAddress by remember { mutableStateOf(s.tcpAddress) }
    val stopped = vm.state.value == VmState.Stopped || vm.state.value is VmState.Failed

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("VM settings") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                if (!stopped) Text("Changes apply when the VM next starts or the guest reboots.")
                NumberField("Memory (MB)", memory) { memory = it }
                NumberField("Disk (GB, only grows)", disk) { disk = it }
                Toggle("One vCPU per host core", allCpus) { allCpus = it }
                Toggle("Network", network) { network = it }
                Toggle("Start when the phone boots", onBoot) { onBoot = it }
                if (BuildConfig.DEBUG) {
                    Toggle("TCP agent backend (debug)", tcp) { tcp = it }
                    if (tcp) {
                        OutlinedTextField(tcpAddress, { tcpAddress = it }, label = { Text("Agent host:port") }, singleLine = true)
                    }
                }
            }
        },
        confirmButton = {
            TextButton(onClick = {
                memory.toIntOrNull()?.takeIf { it in 512..Settings.MAX_MEMORY_MB }?.let { s.memoryMb = it }
                disk.toIntOrNull()?.takeIf { it in 4..DiskImage.maxGb(context) }?.let { s.diskGb = it }
                s.cpus = if (allCpus) 0 else 1
                s.network = network
                s.startOnBoot = onBoot
                s.tcpBackend = tcp
                s.tcpAddress = tcpAddress.trim()
                vm.reloadEngine()
                onDismiss()
            }) { Text("Save") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun NumberField(label: String, value: String, onChange: (String) -> Unit) {
    OutlinedTextField(
        value, { v -> onChange(v.filter(Char::isDigit)) },
        label = { Text(label) },
        singleLine = true,
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
    )
}

@Composable
private fun Toggle(label: String, value: Boolean, onChange: (Boolean) -> Unit) {
    Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
        Text(label, Modifier.weight(1f))
        Switch(value, onChange)
    }
}
