package dev.nickolaj.pocketnix.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import dev.nickolaj.pocketnix.Vm
import dev.nickolaj.pocketnix.agent.Generation
import dev.nickolaj.pocketnix.agent.Updates
import dev.nickolaj.pocketnix.managed.Release
import java.text.DateFormat
import java.util.Date
import kotlinx.coroutines.launch

/** The guest's updates and generations (docs/updates.md). */
@Composable
fun UpdatesDialog(vm: Vm, onDismiss: () -> Unit) {
    val updates by vm.updates.collectAsState()
    val scope = rememberCoroutineScope()
    var refused by remember { mutableStateOf<String?>(null) }
    val host by vm.host.state.collectAsState()
    var confirmingMetered by remember { mutableStateOf(false) }
    val config by vm.config.collectAsState()
    val managed = config?.managedFlake != null
    val ask = remember { PlanAsk() }
    PlanAskDialog(ask)
    // A desktop's `switch` changes generations without telling the agent.
    LaunchedEffect(Unit) { vm.control.refreshUpdates() }

    fun send(type: String, generation: Int? = null) {
        scope.launch { refused = vm.updateRequest(type, generation) }
    }

    // A managed guest checks against this app's release, and asks before compiling on the phone.
    fun update() {
        scope.launch { refused = if (managed) vm.managed.update(ask::ask) else vm.updateRequest("update-apply") }
    }

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("System") },
        text = {
            val u = updates
            if (u == null) {
                Text("The agent isn't connected.")
            } else {
                UpdatesBody(u, refused, onSwitch = { send("switch-generation", it) }, onReboot = vm::reboot)
            }
        },
        confirmButton = {
            val u = updates
            if (u != null && u.sources.isNotEmpty()) {
                Row {
                    // A check or download can hang on a build host that went away; a switch must finish.
                    if (u.busy && u.state != "switching") TextButton(onClick = { send("update-cancel") }) { Text("Cancel") }
                    TextButton(enabled = !u.busy, onClick = { scope.launch { refused = vm.updateRequest("update-check", pin = if (managed) Release.pin else null) } }) {
                        Text("Check")
                    }
                    // A system's closure can be gigabytes: on a metered network the first tap only asks.
                    val metered = host?.metered == true
                    Button(enabled = !u.busy, onClick = {
                        if (metered && !confirmingMetered) {
                            confirmingMetered = true
                        } else {
                            confirmingMetered = false
                            update()
                        }
                    }) { Text(if (metered && confirmingMetered) "Metered: update?" else "Update") }
                }
            }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Close") } }
    )
}

@Composable
private fun UpdatesBody(u: Updates, refused: String?, onSwitch: (Int) -> Unit, onReboot: () -> Unit) {
    Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text(updateStatus(u), style = MaterialTheme.typography.bodyMedium)
        refused?.let { Text(it, color = MaterialTheme.colorScheme.error) }
        u.error?.takeIf { !u.busy && u.blocked == null }?.let { Text("Failed: $it", color = MaterialTheme.colorScheme.error) }
        u.blocked?.takeIf { !u.busy }?.let { Text("Stopped before compiling on the phone: ${it.builds.joinToString(", ")}. Update asks again.") }
        u.plan?.takeIf { u.available != null && !u.busy }?.let { p ->
            Text(
                listOfNotNull(
                    "Downloads ${megabytes(p.download)}.".takeIf { p.download > 0 },
                    "Compiles on the phone: ${p.builds.joinToString(", ")}.".takeIf { p.builds.isNotEmpty() }
                ).joinToString(" ").ifEmpty { "Nothing to download or compile." },
                fontSize = 13.sp
            )
        }
        if (u.rebootPending) {
            // A reboot ends every tab: the first tap only asks.
            var confirmingReboot by remember { mutableStateOf(false) }
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    if (confirmingReboot) "Reboot now? Every tab ends." else "Running a generation it didn't boot; a reboot boots it fully.",
                    Modifier.weight(1f),
                    fontSize = 13.sp
                )
                TextButton(onClick = { if (confirmingReboot) onReboot() else confirmingReboot = true }) {
                    Text(if (confirmingReboot) "Confirm" else "Reboot")
                }
            }
        }
        if (u.log.isNotEmpty() && (u.busy || u.error != null)) {
            Text(
                u.log.takeLast(12).joinToString("\n"),
                fontFamily = FontFamily.Monospace,
                fontSize = 10.sp,
                lineHeight = 12.sp,
                modifier = Modifier
                    .fillMaxWidth()
                    .background(MaterialTheme.colorScheme.surfaceVariant, RoundedCornerShape(4.dp))
                    .padding(6.dp)
            )
        }
        Text("Generations", style = MaterialTheme.typography.titleSmall)
        // A switch is a rollback: the first tap only asks.
        var confirming by remember { mutableStateOf<Int?>(null) }
        for (g in u.generations) {
            GenerationRow(g, enabled = !u.busy && !g.current, confirming = confirming == g.number) {
                if (confirming == g.number) {
                    confirming = null
                    onSwitch(g.number)
                } else {
                    confirming = g.number
                }
            }
        }
    }
}

@Composable
private fun GenerationRow(g: Generation, enabled: Boolean, confirming: Boolean, onSwitch: () -> Unit) {
    Row(Modifier.fillMaxWidth().padding(vertical = 2.dp), verticalAlignment = Alignment.CenterVertically) {
        Column(Modifier.weight(1f)) {
            val marks = listOfNotNull("current".takeIf { g.current }, "booted".takeIf { g.booted }).joinToString(", ")
            Text("${g.number}" + if (marks.isEmpty()) "" else "  ($marks)", style = MaterialTheme.typography.bodyMedium)
            Text(
                listOfNotNull(g.label, DateFormat.getDateTimeInstance(DateFormat.SHORT, DateFormat.SHORT).format(Date(g.date * 1000))).joinToString(" · "),
                fontSize = 12.sp,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis
            )
        }
        if (!g.current) OutlinedButton(enabled = enabled, onClick = onSwitch) { Text(if (confirming) "Confirm" else "Switch") }
    }
}

fun updateStatus(u: Updates): String = when {
    u.state == "checking" -> "Checking for an update…"
    u.state == "fetching" -> "Downloading ${u.available?.let(::storeName) ?: "the update"}…"
    u.state == "switching" -> "Switching…"
    u.state == "installing" -> "Installing packages…"
    u.state == "cleaning" -> "Deleting old generations…"
    u.sources.isEmpty() -> "No update source is configured (pocketnix.guest.updates)."
    u.available != null -> "Update available from ${sourceName(u.source)}: ${storeName(u.available)}"
    u.checkedAt > 0 -> "Up to date (checked ${DateFormat.getTimeInstance(DateFormat.SHORT).format(Date(u.checkedAt * 1000))})."
    else -> "Not checked yet."
}

private fun sourceName(s: String?) = when (s) {
    "buildHost" -> "the build host"
    "published" -> "the published path"
    "local" -> "this guest's flake"
    else -> s ?: "?"
}

/** /nix/store/<hash>-nixos-system-host-26.05… → nixos-system-host-26.05… */
private fun storeName(path: String) = path.substringAfterLast('/').substringAfter('-')
