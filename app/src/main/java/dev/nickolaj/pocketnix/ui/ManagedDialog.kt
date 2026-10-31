package dev.nickolaj.pocketnix.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Switch
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
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import dev.nickolaj.pocketnix.Vm
import dev.nickolaj.pocketnix.managed.Package
import dev.nickolaj.pocketnix.managed.PackageIndex
import dev.nickolaj.pocketnix.managed.SERVICES
import dev.nickolaj.pocketnix.managed.Selections
import kotlinx.coroutines.launch

/** A managed guest's configuration: the user, the service switches, kept generations and packages. */
@Composable
fun ManagedDialog(vm: Vm, onDismiss: () -> Unit) {
    val updates by vm.updates.collectAsState()
    val scope = rememberCoroutineScope()
    val ask = remember { PlanAsk() }
    val saved = remember { vm.managed.store.selections ?: Selections("", emptySet()) }
    var user by remember { mutableStateOf(saved.user) }
    var services by remember { mutableStateOf(saved.services) }
    var keep by remember { mutableStateOf(saved.keepGenerations.toString()) }
    var packages by remember { mutableStateOf(vm.managed.store.packages) }
    var message by remember { mutableStateOf<String?>(null) }
    val busy = updates?.busy == true
    val userError = if (Selections.USER_NAME.matches(user)) null else "Lowercase letters, digits, - and _"
    val wanted = Selections(user, services, keep.toIntOrNull() ?: 0)

    PlanAskDialog(ask)
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Configure") },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedTextField(
                    user,
                    { user = it.lowercase().take(32) },
                    label = { Text("User name") },
                    singleLine = true,
                    isError = userError != null,
                    supportingText = { Text(userError ?: "A new name makes a new user; the old one's home stays.") }
                )
                for (s in SERVICES) {
                    Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                        Column(Modifier.weight(1f)) {
                            Text(s.label)
                            Text(s.detail, fontSize = 12.sp)
                        }
                        Switch(s.id in services, { on -> services = if (on) services + s.id else services - s.id })
                    }
                }
                OutlinedTextField(
                    keep,
                    { v -> keep = v.filter(Char::isDigit).take(2) },
                    label = { Text("Older generations to keep") },
                    singleLine = true,
                    supportingText = { Text("Besides the running and the booted one; the rest are deleted after each boot and switch.") },
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number)
                )
                if (busy) updates?.let { Text(updateStatus(it), style = MaterialTheme.typography.bodyMedium) }
                message?.let { Text(it, color = MaterialTheme.colorScheme.error) }
                Button(enabled = !busy && userError == null, onClick = {
                    message = null
                    scope.launch { message = vm.managed.rebuild(wanted, ask::ask) }
                }) { Text(if (wanted == saved) "Rebuild" else "Apply and rebuild") }

                Text("Packages", style = MaterialTheme.typography.titleSmall, modifier = Modifier.padding(top = 8.dp))
                Text("They go to your user's profile, at once, without a rebuild.", fontSize = 12.sp)
                for (p in packages.sorted()) {
                    Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                        Text(p, Modifier.weight(1f))
                        TextButton(enabled = !busy, onClick = {
                            packages = packages - p
                            scope.launch { message = vm.managed.removePackage(p).error }
                        }) { Text("Remove") }
                    }
                }
                PackageSearch(vm, enabled = !busy, have = packages) { pkg ->
                    scope.launch {
                        message = null
                        var u = vm.managed.addPackage(pkg.attr, false)
                        u.blocked?.let { plan -> if (ask.ask(plan)) u = vm.managed.addPackage(pkg.attr, true) }
                        packages = vm.managed.store.packages
                        message = u.error.takeIf { u.blocked == null }
                    }
                }
            }
        },
        confirmButton = { TextButton(onClick = onDismiss) { Text("Close") } }
    )
}

/** A search of the release's package index, offering [onPick] for each result not in [have]. */
@Composable
fun PackageSearch(vm: Vm, enabled: Boolean, have: Set<String>, action: String = "Add", onPick: (Package) -> Unit) {
    var query by remember { mutableStateOf("") }
    var all by remember { mutableStateOf<List<Package>?>(null) }
    var failed by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(query.isNotBlank()) {
        if (query.isNotBlank() && all == null) {
            runCatching { vm.managed.index.packages() }
                .onSuccess { all = it }
                .onFailure { failed = "The package list didn't download: ${it.message}" }
        }
    }
    OutlinedTextField(query, { query = it }, label = { Text("Find a package") }, singleLine = true, modifier = Modifier.fillMaxWidth())
    failed?.let { Text(it, color = MaterialTheme.colorScheme.error) }
    val list = all
    if (query.isNotBlank() && list == null && failed == null) Text("Loading the package list…", fontSize = 12.sp)
    val results = remember(list, query) { list?.let { PackageIndex.search(it, query) }.orEmpty() }
    for (p in results) {
        Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("${p.attr} ${p.version}", maxLines = 1, overflow = TextOverflow.Ellipsis)
                if (p.description.isNotEmpty()) Text(p.description, fontSize = 12.sp, maxLines = 2, overflow = TextOverflow.Ellipsis)
            }
            if (p.attr !in have) TextButton(enabled = enabled, onClick = { onPick(p) }) { Text(action) }
        }
    }
}
