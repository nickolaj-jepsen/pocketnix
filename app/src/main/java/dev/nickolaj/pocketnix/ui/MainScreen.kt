package dev.nickolaj.pocketnix.ui

import android.content.Context
import android.content.pm.PackageManager
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import dev.nickolaj.pocketnix.Vm
import dev.nickolaj.pocketnix.agent.ControlChannel
import dev.nickolaj.pocketnix.agent.ExtraKey
import dev.nickolaj.pocketnix.engine.DiskImage
import dev.nickolaj.pocketnix.engine.VmState
import dev.nickolaj.pocketnix.terminal.Modifiers
import dev.nickolaj.pocketnix.terminal.Tab
import dev.nickolaj.pocketnix.terminal.TerminalTheme

/** Used until the agent sends the module's rows (Termux's defaults). */
private val defaultExtraKeys = listOf(
    listOf(ExtraKey("ESC", null, null), ExtraKey("/", "\\", null), ExtraKey("-", "|", null), ExtraKey("HOME", null, null), ExtraKey("UP", null, null), ExtraKey("END", null, null), ExtraKey("PGUP", null, null)),
    listOf(ExtraKey("TAB", null, null), ExtraKey("CTRL", null, null), ExtraKey("ALT", null, null), ExtraKey("LEFT", null, null), ExtraKey("DOWN", null, null), ExtraKey("RIGHT", null, null), ExtraKey("PGDN", null, null)),
)

@Composable
fun MainScreen(vm: Vm) {
    val context = LocalContext.current
    val state by vm.state.collectAsState()
    val link by vm.control.link.collectAsState()
    val stopping by vm.stopping.collectAsState()
    val config by vm.config.collectAsState()
    val tabs by vm.tabs.tabs.collectAsState()
    val selected by vm.tabs.selected.collectAsState()
    val revision by vm.tabs.revision.collectAsState()
    val typeface by vm.typeface.collectAsState()
    val themeRevision by vm.themeRevision.collectAsState()
    val forwardErrors by vm.forwardErrors.collectAsState()
    val updates by vm.updates.collectAsState()
    val agentLate by vm.agentLate.collectAsState()
    var showSettings by remember { mutableStateOf(false) }
    var showUpdates by remember { mutableStateOf(false) }
    var showPalette by remember { mutableStateOf(false) }
    var composerOpen by rememberSaveable { mutableStateOf(false) }
    var modifierRevision by remember { mutableIntStateOf(0) }
    val controller = remember {
        TerminalController(context, vm.tabs, vm.settings, Modifiers()) { modifierRevision++ }
    }

    val missing = remember(state, showSettings) { missingRequirements(context, vm) }
    // Recomputed on theme changes: the module's background frames the terminal.
    val background = remember(themeRevision) { Color(TerminalTheme.background) }
    val foreground = remember(themeRevision) { Color(TerminalTheme.foreground) }

    LaunchedEffect(selected, tabs) { controller.show(vm.tabs.current()) }
    LaunchedEffect(typeface) { controller.setTypeface(typeface) }
    LaunchedEffect(themeRevision) { controller.redraw() }

    if (showSettings) SettingsDialog(vm) { showSettings = false }
    if (showUpdates) UpdatesDialog(vm) { showUpdates = false }
    if (showPalette) PaletteDialog(vm, controller) { showPalette = false }

    val running = state == VmState.Running || state == VmState.Starting
    val agentUp = link is ControlChannel.Link.Up
    // The debug TCP backend has no console, so no boot menu.
    val ownsGuest = vm.engine.collectAsState().value.ownsGuest
    Column(
        Modifier
            .fillMaxSize()
            .background(background)
            .safeDrawingPadding()
            .imePadding(),
    ) {
        StatusBar(
            status = statusText(state, link, stopping, config?.hostname),
            running = running,
            canStart = missing.isEmpty(),
            foreground = foreground,
            onStart = { vm.start() },
            onStop = vm::stop,
            composerOpen = composerOpen,
            onComposer = { composerOpen = !composerOpen },
            updateAvailable = updates?.available != null,
            onUpdates = { showUpdates = true },
        ) { dismiss ->
            MenuItem("Commands", enabled = agentUp) { dismiss(); showPalette = true }
            MenuItem("System and updates", enabled = agentUp) { dismiss(); showUpdates = true }
            MenuItem("Settings") { dismiss(); showSettings = true }
            if (running) {
                MenuItem("Reboot", enabled = agentUp) { dismiss(); vm.reboot() }
                if (ownsGuest) MenuItem("Restart with boot menu") { dismiss(); vm.restartWithBootMenu() }
            } else if (ownsGuest) {
                MenuItem("Start with boot menu", enabled = missing.isEmpty()) { dismiss(); vm.start(bootMenu = true) }
            }
        }
        for (e in forwardErrors) {
            Text(e, color = foreground.copy(alpha = 0.8f), fontSize = 12.sp, modifier = Modifier.padding(horizontal = 8.dp))
        }
        if (agentLate) {
            Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp), verticalAlignment = Alignment.CenterVertically) {
                Text(
                    "No agent after two minutes. A generation that won't boot? The console tab shows the boot.",
                    color = foreground.copy(alpha = 0.8f), fontSize = 12.sp, modifier = Modifier.weight(1f),
                )
                TextButton(onClick = vm::restartWithBootMenu) { Text("Boot menu") }
            }
        }
        if (missing.isNotEmpty() && state == VmState.Stopped) {
            Requirements(missing, foreground, Modifier.padding(16.dp))
        }
        // Read for its side effect: titles and finished states redraw the row.
        revision.let {}
        TabRow(
            tabs = tabs,
            selected = selected,
            profiles = config?.profiles?.map { it.name }.orEmpty(),
            canOpen = link is ControlChannel.Link.Up,
            foreground = foreground,
            onSelect = vm.tabs::select,
            onClose = vm.tabs::close,
            onOpen = vm::openTab,
        )
        AndroidView(
            factory = { controller.create().also { controller.show(vm.tabs.current()) } },
            modifier = Modifier.weight(1f).fillMaxWidth(),
        )
        modifierRevision.let {}
        ExtraKeysBar(
            rows = config?.extraKeys?.takeIf { it.isNotEmpty() } ?: defaultExtraKeys,
            modifiers = controller.modifiers,
            revision = modifierRevision,
            foreground = foreground,
            onKey = controller::press,
        )
        if (composerOpen) Composer(controller, foreground)
    }
}

private fun statusText(state: VmState, link: ControlChannel.Link, stopping: Boolean, hostname: String?): String = when {
    stopping -> "Stopping…"
    state == VmState.Stopped -> "Stopped"
    state is VmState.Failed -> "Failed: ${state.message}"
    state == VmState.Starting -> "Starting…"
    link is ControlChannel.Link.Up -> "Running · ${hostname ?: "guest"}"
    link is ControlChannel.Link.Refused -> "Agent refused: ${link.reason}"
    else -> "Booting (waiting for the agent)"
}

@Composable
private fun MenuItem(text: String, enabled: Boolean = true, onClick: () -> Unit) {
    DropdownMenuItem(text = { Text(text) }, enabled = enabled, onClick = onClick)
}

@Composable
private fun StatusBar(
    status: String,
    running: Boolean,
    canStart: Boolean,
    foreground: Color,
    onStart: () -> Unit,
    onStop: () -> Unit,
    composerOpen: Boolean,
    onComposer: () -> Unit,
    updateAvailable: Boolean,
    onUpdates: () -> Unit,
    menuItems: @Composable (dismiss: () -> Unit) -> Unit,
) {
    var menu by remember { mutableStateOf(false) }
    Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        Text(status, color = foreground, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f))
        if (updateAvailable) TextButton(onClick = onUpdates) { Text("Update") }
        TextButton(onClick = onComposer) { Text(if (composerOpen) "Keys" else "Compose") }
        Box {
            TextButton(onClick = { menu = true }) { Text("⋮", color = foreground) }
            DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) { menuItems { menu = false } }
        }
        if (running) {
            // The module's background may be dark under a light Material scheme.
            OutlinedButton(onClick = onStop, colors = ButtonDefaults.outlinedButtonColors(contentColor = foreground)) { Text("Stop") }
        } else {
            Button(onClick = onStart, enabled = canStart) { Text("Start") }
        }
    }
}

@Composable
private fun TabRow(
    tabs: List<Tab>,
    selected: Int,
    profiles: List<String>,
    canOpen: Boolean,
    foreground: Color,
    onSelect: (Int) -> Unit,
    onClose: (Int) -> Unit,
    onOpen: (String?) -> Unit,
) {
    var menu by remember { mutableStateOf(false) }
    Row(
        Modifier.fillMaxWidth().horizontalScroll(rememberScrollState()).padding(horizontal = 4.dp),
        horizontalArrangement = Arrangement.spacedBy(4.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        for (tab in tabs) {
            val isSelected = tab.id == selected
            Row(
                Modifier
                    .background(if (isSelected) foreground.copy(alpha = 0.18f) else Color.Transparent, RoundedCornerShape(6.dp))
                    .clickable { onSelect(tab.id) }
                    .padding(horizontal = 10.dp, vertical = 6.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                val ended = !tab.session.isRunning && tab.kind != Tab.Kind.Console
                // A bell or notification arrived while nobody watched the tab.
                if (tab.attention) Text("● ", color = foreground, fontSize = 10.sp)
                Text(
                    tab.title + if (ended) " (ended)" else "",
                    color = foreground.copy(alpha = if (isSelected) 1f else 0.7f),
                    fontSize = 13.sp,
                    maxLines = 1,
                )
                if (tab.kind != Tab.Kind.Console) {
                    Text("  ×", color = foreground.copy(alpha = 0.7f), fontSize = 13.sp, modifier = Modifier.clickable { onClose(tab.id) })
                }
            }
        }
        Box {
            Text(
                " + ",
                color = foreground.copy(alpha = if (canOpen) 1f else 0.3f),
                fontSize = 18.sp,
                modifier = Modifier
                    .combinedClickable(enabled = canOpen, onLongClick = { menu = profiles.size > 1 }, onClick = { onOpen(null) })
                    .padding(horizontal = 8.dp, vertical = 2.dp),
            )
            DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                for (p in profiles) DropdownMenuItem(text = { Text(p) }, onClick = { menu = false; onOpen(p) })
            }
        }
    }
}

/** One prerequisite of booting a VM, with the fix shown while it's missing. */
data class Requirement(val name: String, val fix: String)

fun missingRequirements(context: Context, vm: Vm): List<Requirement> {
    if (vm.settings.tcpBackend) return emptyList()
    fun granted(p: String) = context.checkSelfPermission(p) == PackageManager.PERMISSION_GRANTED
    val grant = "run `pocketnix provision`, or adb shell pm grant ${context.packageName}"
    return buildList {
        // PackageManager.FEATURE_VIRTUALIZATION_FRAMEWORK is @SystemApi, so spell it out.
        if (!context.packageManager.hasSystemFeature("android.software.virtualization_framework")) {
            add(Requirement("Android Virtualization Framework", "this device has no AVF"))
        }
        for (p in listOf("MANAGE_VIRTUAL_MACHINE", "USE_CUSTOM_VIRTUAL_MACHINE")) {
            if (!granted("android.permission.$p")) add(Requirement(p, "$grant android.permission.$p"))
        }
        if (!DiskImage.file(context).exists()) add(Requirement("Disk image", "run `pocketnix provision --image …`"))
    }
}

@Composable
private fun Requirements(missing: List<Requirement>, foreground: Color, modifier: Modifier = Modifier) {
    Column(modifier, verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text("Before the VM can start:", color = foreground, style = MaterialTheme.typography.titleMedium)
        for (r in missing) {
            Text("✗ ${r.name}", color = foreground)
            Text(r.fix, color = foreground.copy(alpha = 0.7f), style = MaterialTheme.typography.bodySmall)
        }
        Spacer(Modifier.padding(4.dp))
    }
}
