package dev.nickolaj.pocketnix.gui

import android.content.Intent
import android.graphics.BitmapFactory
import android.os.Bundle
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.Image
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.GridItemSpan
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.produceState
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import dev.nickolaj.pocketnix.MainActivity
import dev.nickolaj.pocketnix.R
import dev.nickolaj.pocketnix.Vm
import dev.nickolaj.pocketnix.VmPhase
import dev.nickolaj.pocketnix.managed.Package
import dev.nickolaj.pocketnix.managed.PackageIndex
import dev.nickolaj.pocketnix.ui.PlanAsk
import dev.nickolaj.pocketnix.ui.PlanAskDialog
import dev.nickolaj.pocketnix.ui.PocketnixTheme
import dev.nickolaj.pocketnix.vm
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.filterNotNull
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull

/**
 * The guest's apps, from the cached list, so it works with the VM off: a
 * tap launches one on a splash (booting the VM if it's off), a long press
 * offers a new window, the entry's actions and a home-screen pin. Its
 * drawer icon is an activity-alias that shows while the guest has GUI on.
 */
class LauncherActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        setContent {
            PocketnixTheme {
                Surface(Modifier.fillMaxSize()) {
                    Launcher(vm, ::open, ::pin, ::runInTab)
                }
            }
        }
    }

    /** A fetched program without a window: it runs in a new tab of the main screen. */
    private fun runInTab(command: String) = guard("launcher: run $command") {
        vm.openTab(command = command) ?: return@guard
        startActivity(Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_REORDER_TO_FRONT))
    }

    private fun open(app: AppEntry, action: String, newWindow: Boolean) = guard("launcher: open ${app.id}") {
        startActivity(GuiWindowActivity.appIntent(this, app.id, action, newWindow))
    }

    private fun pin(app: AppEntry) = guard("launcher: pin ${app.id}") {
        when (vm.gui.shortcuts.pin(app)) {
            AppShortcuts.Pin.ALREADY -> Toast.makeText(this, "${app.label} is already on the home screen", Toast.LENGTH_SHORT).show()
            AppShortcuts.Pin.UNSUPPORTED -> Toast.makeText(this, "This launcher can't add shortcuts", Toast.LENGTH_SHORT).show()
            AppShortcuts.Pin.ASKED -> {}
        }
    }
}

@Composable
private fun Launcher(vm: Vm, open: (AppEntry, String, Boolean) -> Unit, pin: (AppEntry) -> Unit, runInTab: (String) -> Unit) {
    val revision by vm.gui.catalog.revision.collectAsState()
    val phase by vm.phase.collectAsState()
    val config by vm.config.collectAsState()
    var query by rememberSaveable { mutableStateOf("") }
    val apps = remember(revision, query) {
        vm.gui.catalog.apps.filter { !it.noDisplay && it.matches(query) }.sortedBy { it.label.lowercase() }
    }
    val iconSize = remember { vm.gui.iconSize(large = false) }
    Column(Modifier.fillMaxSize().safeDrawingPadding().padding(horizontal = 12.dp)) {
        Text(
            launcherStatus(phase, config?.gui?.enable),
            style = MaterialTheme.typography.bodySmall,
            modifier = Modifier.padding(vertical = 8.dp)
        )
        OutlinedTextField(
            value = query,
            onValueChange = { query = it },
            placeholder = { Text("Search apps") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth()
        )
        if (vm.gui.catalog.apps.isEmpty()) {
            Text(
                "No apps yet. With pocketnix.guest.gui.enable set, start the VM once: pocketnix keeps the guest's " +
                    "app list for when it's off.",
                modifier = Modifier.padding(vertical = 24.dp)
            )
        }
        val managed = config?.managedFlake != null && phase is VmPhase.Running
        val runner = remember { NixpkgsRunner(vm, open, runInTab) }
        PlanAskDialog(runner.ask)
        LazyVerticalGrid(
            columns = GridCells.Adaptive(88.dp),
            modifier = Modifier.fillMaxSize().padding(top = 8.dp),
            verticalArrangement = Arrangement.spacedBy(4.dp)
        ) {
            items(apps, key = { it.id }) { app -> AppCell(vm, app, iconSize, revision, open, pin) }
            if (managed && query.isNotBlank()) {
                item(span = { GridItemSpan(maxLineSpan) }) { NixpkgsResults(vm, runner, query) }
            }
        }
    }
}

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun AppCell(vm: Vm, app: AppEntry, iconSize: Int, revision: Int, open: (AppEntry, String, Boolean) -> Unit, pin: (AppEntry) -> Unit) {
    var menu by remember { mutableStateOf(false) }
    val icon by produceState<ImageBitmap?>(null, app.icon, revision) {
        value = withContext(Dispatchers.IO) {
            vm.gui.catalog.icon(app.icon, iconSize)?.let { runCatching { BitmapFactory.decodeFile(it.path)?.asImageBitmap() }.getOrNull() }
        }
    }
    Box {
        Column(
            Modifier
                .fillMaxWidth()
                .combinedClickable(onClick = { open(app, "", false) }, onLongClick = { menu = true })
                .padding(vertical = 8.dp),
            horizontalAlignment = Alignment.CenterHorizontally
        ) {
            val bitmap = icon
            if (bitmap != null) {
                Image(bitmap, contentDescription = null, modifier = Modifier.size(48.dp))
            } else {
                Image(painterResource(R.drawable.ic_guest_app), contentDescription = null, modifier = Modifier.size(48.dp))
            }
            Text(
                app.label,
                style = MaterialTheme.typography.bodySmall,
                textAlign = TextAlign.Center,
                maxLines = 2,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.padding(top = 4.dp, start = 2.dp, end = 2.dp)
            )
        }
        DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
            DropdownMenuItem(text = { Text("New window") }, onClick = {
                menu = false
                open(app, "", true)
            })
            for (a in app.actions) {
                DropdownMenuItem(text = { Text(a.name.ifEmpty { a.id }) }, onClick = {
                    menu = false
                    open(app, a.id, false)
                })
            }
            DropdownMenuItem(text = { Text("Add to home screen") }, onClick = {
                menu = false
                pin(app)
            })
        }
    }
}

/** Runs nixpkgs packages ad hoc: fetched into the guest, then launched or opened in a tab. */
private class NixpkgsRunner(val vm: Vm, val open: (AppEntry, String, Boolean) -> Unit, val runInTab: (String) -> Unit) {
    val ask = PlanAsk()
    var status by mutableStateOf<String?>(null)
    var running by mutableStateOf(false)

    suspend fun run(pkg: Package) {
        running = true
        status = "Fetching ${pkg.attr}…"
        try {
            var u = vm.managed.run(pkg.attr, false)
            u.blocked?.let { plan -> if (ask.ask(plan)) u = vm.managed.run(pkg.attr, true) else return }
            val r = u.run?.takeIf { it.pkg == pkg.attr && u.error == null }
            if (r == null) {
                status = u.error ?: "${pkg.attr} didn't fetch"
                return
            }
            val command = r.command
            if (r.apps.isEmpty() && command != null) {
                status = null
                runInTab(command)
                return
            }
            // The compositor rescans its app list when the ad-hoc profile changes.
            val entry = withTimeoutOrNull(APP_LIST_WAIT_MS) {
                vm.gui.catalog.revision.map { vm.gui.catalog.apps.firstOrNull { it.id in r.apps } }.filterNotNull().first()
            }
            status = if (entry == null) "${pkg.attr} is here, but its app didn't show up" else null
            entry?.let { open(it, "", false) }
        } catch (e: Exception) {
            status = e.message ?: e.toString()
        } finally {
            running = false
        }
    }

    suspend fun keep(pkg: Package) {
        running = true
        try {
            var u = vm.managed.addPackage(pkg.attr, false)
            u.blocked?.let { plan -> if (ask.ask(plan)) u = vm.managed.addPackage(pkg.attr, true) else return }
            status = u.error ?: "${pkg.attr} is kept"
        } finally {
            running = false
        }
    }

    companion object {
        const val APP_LIST_WAIT_MS = 30_000L
    }
}

@Composable
private fun NixpkgsResults(vm: Vm, runner: NixpkgsRunner, query: String) {
    val scope = rememberCoroutineScope()
    var all by remember { mutableStateOf<List<Package>?>(null) }
    var failed by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(Unit) {
        runCatching { vm.managed.index.packages() }.onSuccess { all = it }.onFailure { failed = "The package list didn't download: ${it.message}" }
    }
    val results = remember(all, query) { all?.let { PackageIndex.search(it, query, limit = 20) }.orEmpty() }
    val kept = vm.managed.store.packages
    Column(Modifier.fillMaxWidth().padding(top = 16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Text("From nixpkgs", style = MaterialTheme.typography.titleSmall)
        Text("Run fetches one for now; Keep installs it for good.", style = MaterialTheme.typography.bodySmall)
        runner.status?.let { Text(it, style = MaterialTheme.typography.bodySmall) }
        failed?.let { Text(it, color = MaterialTheme.colorScheme.error) }
        if (all == null && failed == null) Text("Loading the package list…", style = MaterialTheme.typography.bodySmall)
        for (p in results) {
            Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                Column(Modifier.weight(1f)) {
                    Text("${p.attr} ${p.version}", maxLines = 1, overflow = TextOverflow.Ellipsis)
                    if (p.description.isNotEmpty()) Text(p.description, style = MaterialTheme.typography.bodySmall, maxLines = 2, overflow = TextOverflow.Ellipsis)
                }
                TextButton(enabled = !runner.running, onClick = { scope.launch { runner.run(p) } }) { Text("Run") }
                if (p.attr !in kept) TextButton(enabled = !runner.running, onClick = { scope.launch { runner.keep(p) } }) { Text("Keep") }
            }
        }
    }
}

/** The launcher's line above the apps; [guiEnabled] is the guest config's `gui.enable`, null before one arrived. */
internal fun launcherStatus(phase: VmPhase, guiEnabled: Boolean?): String = when (phase) {
    VmPhase.Stopped -> "The VM is off. Tapping an app starts it."

    is VmPhase.Failed -> "The VM failed: ${phase.message}. Tapping an app starts it again."

    VmPhase.Stopping -> "The VM is stopping."

    VmPhase.Starting, VmPhase.Booting -> "The VM is starting."

    VmPhase.AgentLate -> "The guest's agent hasn't answered. pocketnix's console tab shows the boot."

    is VmPhase.Refused -> "The guest's agent refused pocketnix: ${phase.reason}"

    is VmPhase.Running -> if (guiEnabled == false) {
        "GUI apps are off in this guest (pocketnix.guest.gui.enable)."
    } else {
        "Apps of ${phase.host ?: "the guest"}"
    }
}
