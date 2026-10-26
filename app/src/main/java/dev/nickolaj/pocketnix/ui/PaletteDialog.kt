package dev.nickolaj.pocketnix.ui

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import dev.nickolaj.pocketnix.Vm
import dev.nickolaj.pocketnix.terminal.Palette
import dev.nickolaj.pocketnix.terminal.PaletteEntry

/**
 * The command palette: the module's snippets and the user's shell history,
 * filtered as you type. Tapping a row types it into the current tab; ⏎ also
 * presses Enter.
 */
@Composable
fun PaletteDialog(vm: Vm, controller: TerminalController, onDismiss: () -> Unit) {
    val config by vm.config.collectAsState()
    var history by remember { mutableStateOf<List<String>>(emptyList()) }
    var query by remember { mutableStateOf("") }
    val focus = remember { FocusRequester() }
    LaunchedEffect(Unit) {
        focus.requestFocus()
        history = vm.history()
    }
    val rows = remember(config, history, query) { Palette.entries(config?.snippets.orEmpty(), history, query) }

    fun pick(e: PaletteEntry, submit: Boolean) {
        controller.paste(e.command, submit)
        onDismiss()
    }

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Commands") },
        text = {
            Column {
                OutlinedTextField(
                    query, { query = it },
                    placeholder = { Text("Filter snippets and history") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth().focusRequester(focus),
                )
                LazyColumn(Modifier.heightIn(max = 420.dp).padding(top = 8.dp)) {
                    items(rows) { e -> PaletteRow(e, onPick = { pick(e, e.submit) }, onRun = { pick(e, true) }) }
                }
                if (rows.isEmpty()) Text("Nothing matches.", Modifier.padding(top = 8.dp))
            }
        },
        confirmButton = {},
        dismissButton = { TextButton(onClick = onDismiss) { Text("Close") } },
    )
}

@Composable
private fun PaletteRow(e: PaletteEntry, onPick: () -> Unit, onRun: () -> Unit) {
    Row(Modifier.fillMaxWidth().clickable(onClick = onPick).padding(vertical = 6.dp), verticalAlignment = Alignment.CenterVertically) {
        Column(Modifier.weight(1f)) {
            if (e.snippet) Text(e.label, style = MaterialTheme.typography.bodyMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
            Text(
                e.command,
                fontFamily = FontFamily.Monospace,
                fontSize = if (e.snippet) 11.sp else 13.sp,
                maxLines = 2,
                overflow = TextOverflow.Ellipsis,
            )
        }
        TextButton(onClick = onRun) { Text("⏎") }
    }
}
