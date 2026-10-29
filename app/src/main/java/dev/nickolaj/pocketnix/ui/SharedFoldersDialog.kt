package dev.nickolaj.pocketnix.ui

import android.net.Uri
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.MaterialTheme
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
import androidx.compose.ui.unit.dp
import dev.nickolaj.pocketnix.Vm
import dev.nickolaj.pocketnix.hostfs.SharedFolder
import dev.nickolaj.pocketnix.hostfs.SharedFolders

/**
 * The Android folders mounted in the guest, under
 * `pocketnix.guest.hostFolders.mountPoint`. Changes reach a running guest at once.
 */
@Composable
fun SharedFoldersDialog(vm: Vm, onDismiss: () -> Unit) {
    val store = vm.sharedFolders
    val folders by store.folders.collectAsState()
    val agentError by vm.hostFolders.error.collectAsState()
    var picked by remember { mutableStateOf<Uri?>(null) }
    var renaming by remember { mutableStateOf<SharedFolder?>(null) }
    var failure by remember { mutableStateOf<String?>(null) }
    val pick = rememberLauncherForActivityResult(ActivityResultContracts.OpenDocumentTree()) { uri -> picked = uri }

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Android folders in the guest") },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                if (folders.isEmpty()) Text("None yet. Each appears in the guest as ~/Android/<name> (the module's hostFolders.mountPoint), for the session user only.")
                for (f in folders) {
                    Column {
                        Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                            Column(Modifier.weight(1f)) {
                                Text(f.name)
                                Text(SharedFolders.location(f.uri), style = MaterialTheme.typography.bodySmall)
                            }
                            Text("Writable", style = MaterialTheme.typography.bodySmall)
                            Switch(f.writable, { store.update(f, writable = it) })
                        }
                        Row {
                            TextButton(onClick = { renaming = f }) { Text("Rename") }
                            TextButton(onClick = { store.remove(f) }) { Text("Stop sharing") }
                        }
                    }
                }
                (failure ?: agentError)?.let { Text(it, color = MaterialTheme.colorScheme.error) }
            }
        },
        confirmButton = { TextButton(onClick = { pick.launch(null) }) { Text("Share a folder") } },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Done") } },
    )

    picked?.let { uri ->
        NameDialog(store, SharedFolders.defaultName(uri), null, onDismiss = { picked = null }) { name ->
            failure = runCatching { store.add(uri, name) }.exceptionOrNull()?.let { "Couldn't keep access to that folder: ${it.message}" }
            picked = null
        }
    }
    renaming?.let { f ->
        NameDialog(store, f.name, f.name, onDismiss = { renaming = null }) { name ->
            store.update(f, name = name)
            renaming = null
        }
    }
}

@Composable
private fun NameDialog(store: SharedFolders, initial: String, except: String?, onDismiss: () -> Unit, onDone: (String) -> Unit) {
    var name by remember { mutableStateOf(initial) }
    val problem = store.problem(name.trim(), except)
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Name in the guest") },
        text = {
            OutlinedTextField(
                name, { name = it },
                singleLine = true,
                isError = problem != null,
                supportingText = problem?.let { { Text(it) } },
            )
        },
        confirmButton = { TextButton(enabled = problem == null, onClick = { onDone(name.trim()) }) { Text("OK") } },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}
