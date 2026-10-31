package dev.nickolaj.pocketnix.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import dev.nickolaj.pocketnix.agent.Plan
import kotlinx.coroutines.CompletableDeferred

/** Asks whether to go on with a [Plan] that compiles on the phone; [PlanAskDialog] shows the question. */
class PlanAsk {
    var pending by mutableStateOf<Pair<Plan, CompletableDeferred<Boolean>>?>(null)
        private set

    suspend fun ask(plan: Plan): Boolean {
        val answer = CompletableDeferred<Boolean>()
        pending = plan to answer
        return try {
            answer.await()
        } finally {
            pending = null
        }
    }
}

@Composable
fun PlanAskDialog(ask: PlanAsk) {
    val (plan, answer) = ask.pending ?: return
    AlertDialog(
        onDismissRequest = { answer.complete(false) },
        title = { Text("Compile on the phone?") },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("These aren't in the binary caches, so the phone would build them from source. That can take a long while, on a share of the phone's CPU.")
                Text(plan.builds.joinToString("\n"))
                if (plan.download > 0) Text("It also downloads ${megabytes(plan.download)}.")
            }
        },
        confirmButton = { TextButton(onClick = { answer.complete(true) }) { Text("Compile") } },
        dismissButton = { TextButton(onClick = { answer.complete(false) }) { Text("Cancel") } }
    )
}

fun megabytes(bytes: Long): String = when {
    bytes >= 1L shl 30 -> "%.1f GB".format(bytes / (1L shl 30).toDouble())
    else -> "%.0f MB".format(bytes / (1L shl 20).toDouble())
}
