package dev.nickolaj.pocketnix.ui

import androidx.compose.foundation.background
import androidx.compose.foundation.gestures.awaitEachGesture
import androidx.compose.foundation.gestures.awaitFirstDown
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.Button
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import dev.nickolaj.pocketnix.agent.ExtraKey
import dev.nickolaj.pocketnix.terminal.ExtraKeys
import dev.nickolaj.pocketnix.terminal.KeyAction
import dev.nickolaj.pocketnix.terminal.Modifiers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/** The module's extra-keys rows: tap a key, swipe up for its popup, hold to repeat. */
@Composable
fun ExtraKeysBar(rows: List<List<ExtraKey>>, modifiers: Modifiers, revision: Int, foreground: Color, onKey: (String) -> Unit) {
    Column(Modifier.fillMaxWidth().padding(horizontal = 2.dp, vertical = 2.dp)) {
        for (row in rows) {
            Row(Modifier.fillMaxWidth().height(40.dp)) {
                for (key in row) {
                    ExtraKeyButton(key, modifiers, revision, foreground, onKey, Modifier.weight(1f).fillMaxHeight())
                }
            }
        }
    }
}

@Composable
private fun ExtraKeyButton(key: ExtraKey, modifiers: Modifiers, revision: Int, foreground: Color, onKey: (String) -> Unit, modifier: Modifier) {
    val scope = rememberCoroutineScope()
    val action = remember(key.key) { ExtraKeys.action(key.key) }
    val modState = (action as? KeyAction.Toggle)?.let { revision.let { _ -> modifiers.state(it.modifier) } }
    val fill = when (modState) {
        Modifiers.State.ONCE -> foreground.copy(alpha = 0.25f)
        Modifiers.State.LOCKED -> foreground.copy(alpha = 0.5f)
        else -> Color.Transparent
    }
    Box(
        modifier
            .padding(1.dp)
            .background(fill, RoundedCornerShape(4.dp))
            .pointerInput(key) {
                val swipe = 24.dp.toPx()
                awaitEachGesture {
                    val down = awaitFirstDown()
                    var swiped = false
                    var repeated = false
                    var repeat: Job? = null
                    // Holding a key that isn't a modifier repeats it, as a hardware key would.
                    if (action !is KeyAction.Toggle) {
                        repeat = scope.launch {
                            delay(400)
                            repeated = true
                            while (true) {
                                onKey(key.key)
                                delay(50)
                            }
                        }
                    }
                    while (true) {
                        val event = awaitPointerEvent()
                        val change = event.changes.firstOrNull { it.id == down.id } ?: break
                        if (down.position.y - change.position.y > swipe && key.popup != null) {
                            swiped = true
                            repeat?.cancel()
                        }
                        if (!change.pressed) break
                    }
                    repeat?.cancel()
                    when {
                        swiped -> onKey(key.popup!!)
                        !repeated -> onKey(key.key)
                    }
                }
            },
        contentAlignment = Alignment.Center,
    ) {
        Text(key.label ?: ExtraKeys.label(key.key), color = foreground, fontSize = 14.sp, maxLines = 1)
        key.popup?.let {
            Text(
                ExtraKeys.label(it),
                color = foreground.copy(alpha = 0.5f),
                fontSize = 9.sp,
                modifier = Modifier.align(Alignment.TopEnd).padding(end = 3.dp),
            )
        }
    }
}

/**
 * A native text field above the keys, for the IME's autocorrect and
 * dictation; its text goes to the terminal as a bracketed paste.
 */
@Composable
fun Composer(controller: TerminalController, foreground: Color) {
    var text by rememberSaveable { mutableStateOf("") }
    fun send(submit: Boolean) {
        controller.paste(text, submit)
        text = ""
    }
    Row(Modifier.fillMaxWidth().padding(4.dp), verticalAlignment = Alignment.CenterVertically) {
        OutlinedTextField(
            value = text,
            onValueChange = { text = it },
            modifier = Modifier.weight(1f),
            maxLines = 4,
            placeholder = { Text("Compose", color = foreground.copy(alpha = 0.5f)) },
            keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.None, autoCorrectEnabled = true),
        )
        TextButton(onClick = { send(submit = false) }) { Text("Paste") }
        Button(onClick = { send(submit = true) }) { Text("Send") }
    }
}
