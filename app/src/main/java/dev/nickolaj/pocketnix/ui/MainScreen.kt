package dev.nickolaj.pocketnix.ui

import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.view.View
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.ime
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.layout.systemBars
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.LocalContentColor
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.SideEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.IntOffset
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.lifecycle.compose.LifecycleResumeEffect
import dev.nickolaj.pocketnix.Vm
import dev.nickolaj.pocketnix.VmPhase
import dev.nickolaj.pocketnix.agent.ControlChannel
import dev.nickolaj.pocketnix.engine.DiskImage
import dev.nickolaj.pocketnix.engine.VmState
import dev.nickolaj.pocketnix.gui.LauncherActivity
import dev.nickolaj.pocketnix.gui.MOVE_OUT
import dev.nickolaj.pocketnix.gui.MenuAction
import dev.nickolaj.pocketnix.gui.TabStripItem
import dev.nickolaj.pocketnix.gui.WindowMenu
import dev.nickolaj.pocketnix.gui.WindowSurface
import dev.nickolaj.pocketnix.gui.closingWindows
import dev.nickolaj.pocketnix.gui.guard
import dev.nickolaj.pocketnix.gui.stopQuestion
import dev.nickolaj.pocketnix.gui.tabStrip
import dev.nickolaj.pocketnix.gui.windowMenu
import dev.nickolaj.pocketnix.terminal.Modifiers
import dev.nickolaj.pocketnix.terminal.Tab

@Composable
fun MainScreen(vm: Vm) {
    val context = LocalContext.current
    val state by vm.state.collectAsState()
    val link by vm.control.link.collectAsState()
    val phase by vm.phase.collectAsState()
    val config by vm.config.collectAsState()
    val syncError by vm.managed.syncError.collectAsState()
    val tabs by vm.tabs.tabs.collectAsState()
    val selected by vm.tabs.selected.collectAsState()
    val revision by vm.tabs.revision.collectAsState()
    val typeface by vm.typeface.collectAsState()
    val themeRevision by vm.themeRevision.collectAsState()
    val forwardErrors by vm.forwardErrors.collectAsState()
    val updates by vm.updates.collectAsState()
    val serviceRefused by vm.serviceRefused.collectAsState()
    val windowTabs by vm.gui.tabWindows.collectAsState()
    val selectedWindow by vm.tabs.selectedWindow.collectAsState()
    // A window that just left its tab may still be selected for a moment.
    val shownWindow = selectedWindow.takeIf { id -> windowTabs.any { it.id == id } } ?: 0
    var showSettings by remember { mutableStateOf(false) }
    var showUpdates by remember { mutableStateOf(false) }
    var showManaged by remember { mutableStateOf(false) }
    var showPalette by remember { mutableStateOf(false) }
    var composerOpen by rememberSaveable { mutableStateOf(false) }
    var modifierRevision by remember { mutableIntStateOf(0) }
    // Where the terminal's context menu opens, in the view's pixels; null while it's closed.
    var terminalMenuAt by remember { mutableStateOf<IntOffset?>(null) }
    val controller = remember {
        TerminalController(context, vm.tabs, vm.settings, Modifiers()) { modifierRevision++ }.apply {
            onOpenLink = vm.terminalActions::open
            onContextMenu = { x, y -> terminalMenuAt = IntOffset(x.toInt(), y.toInt()) }
        }
    }

    DisposableEffect(controller) { onDispose { controller.release() } }

    // The window tab's own keyboard, which the extra keys type into while it shows; bound once its view is.
    var windowSurface by remember { mutableStateOf<WindowSurface?>(null) }
    DisposableEffect(shownWindow) {
        val s = vm.gui.surfaceOf(shownWindow)
        s?.ime?.onModifiers = { modifierRevision++ }
        windowSurface = s
        onDispose { windowSurface = null }
    }
    val density = LocalDensity.current
    val imeBottom = WindowInsets.ime.getBottom(density)
    // The fit measures a tab's window as if the keyboard weren't there.
    val keyboardHeight = (imeBottom - WindowInsets.systemBars.getBottom(density)).coerceAtLeast(0)
    SideEffect { windowSurface?.keyboardHeight = keyboardHeight }
    LaunchedEffect(imeBottom > 0, windowSurface) { if (imeBottom == 0) windowSurface?.ime?.keyboardHidden() }

    // Resumes count: `pocketnix provision` grants and pushes while the app is open.
    var resumes by remember { mutableIntStateOf(0) }
    LifecycleResumeEffect(Unit) {
        resumes++
        onPauseOrDispose {}
    }
    val missing = remember(state, showSettings, resumes) { missingRequirements(context, vm) }
    LaunchedEffect(selected, tabs, shownWindow) { if (shownWindow == 0) controller.show(vm.tabs.current()) }
    LaunchedEffect(typeface) { controller.setTypeface(typeface) }
    LaunchedEffect(themeRevision) { controller.redraw() }

    // A destructive tap asks first: the question, and what a yes does.
    var confirm by remember { mutableStateOf<Pair<String, () -> Unit>?>(null) }
    confirm?.let { (question, action) ->
        AlertDialog(
            onDismissRequest = { confirm = null },
            text = { Text(question) },
            confirmButton = {
                TextButton(onClick = {
                    confirm = null
                    action()
                }) { Text("Yes") }
            },
            dismissButton = { TextButton(onClick = { confirm = null }) { Text("Cancel") } }
        )
    }

    // The terminal tab being renamed, from its long-press menu.
    var renaming by remember { mutableStateOf<Tab?>(null) }
    renaming?.let { tab -> RenameDialog(tab.title, onDismiss = { renaming = null }) { vm.tabs.rename(tab.id, it) } }

    if (showSettings) SettingsDialog(vm) { showSettings = false }
    if (showUpdates) UpdatesDialog(vm) { showUpdates = false }
    if (showManaged) ManagedDialog(vm) { showManaged = false }
    if (showPalette) PaletteDialog(vm, controller) { showPalette = false }

    val running = state == VmState.Running || state == VmState.Starting
    Column(
        Modifier
            .fillMaxSize()
            // The theme's background is the terminal's, so it frames the terminal.
            .background(MaterialTheme.colorScheme.background)
            .safeDrawingPadding()
            .imePadding()
    ) {
        StatusBar(
            status = statusText(phase),
            running = running,
            canStart = missing.isEmpty(),
            onStart = { vm.start() },
            onStop = {
                confirm = stopQuestion(vm.gui.windowTitles()) to vm::stop
            },
            composerOpen = composerOpen,
            onComposer = { composerOpen = !composerOpen },
            updateAvailable = updates?.available != null,
            onUpdates = { showUpdates = true }
        ) {
            // Built during composition: a throw here would kill the app and the VM.
            guard("building the ⋮ menu", emptyList()) {
                mainMenu(mainMenuState(context, vm, shownWindow)) { item ->
                    when (item) {
                        MainItem.APPS -> context.startActivity(Intent(context, LauncherActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
                        MainItem.MOVE_OUT -> vm.gui.move(shownWindow, "")
                        MainItem.REOPEN_ENDED -> vm.reopenEndedTabs()
                        MainItem.COMMANDS -> showPalette = true
                        MainItem.CONFIGURE -> showManaged = true
                        MainItem.UPDATES -> showUpdates = true
                        MainItem.SETTINGS -> showSettings = true
                        MainItem.KEEP_SCREEN_ON -> controller.setKeepScreenOn(!vm.settings.keepScreenOn)
                        MainItem.REBOOT -> confirm = "Reboot the guest? Every tab ends. ${closingWindows(vm.gui.windowTitles())}".trim() to vm::reboot
                        MainItem.RESTART_BOOT_MENU -> confirm = "Restart into the boot menu? This stops the guest at once, without shutting it down." to vm::restartWithBootMenu
                        MainItem.START_BOOT_MENU -> vm.start(bootMenu = true)
                    }
                }
            }
        }
        for (e in forwardErrors) {
            Text(e, color = LocalContentColor.current.copy(alpha = 0.8f), fontSize = 12.sp, modifier = Modifier.padding(horizontal = 8.dp))
        }
        if (serviceRefused && running) {
            Text(
                "Android refused the foreground service, so it may kill the VM with the app. Reopening the app retries.",
                color = LocalContentColor.current.copy(alpha = 0.8f),
                fontSize = 12.sp,
                modifier = Modifier.padding(horizontal = 8.dp)
            )
        }
        if (phase == VmPhase.AgentLate) {
            Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp), verticalAlignment = Alignment.CenterVertically) {
                Text(
                    "The agent hasn't answered. A generation that won't boot? The console tab shows the boot.",
                    color = LocalContentColor.current.copy(alpha = 0.8f),
                    fontSize = 12.sp,
                    modifier = Modifier.weight(1f)
                )
                TextButton(onClick = vm::restartWithBootMenu) { Text("Boot menu") }
            }
        }
        // A guest the phone is working on (its first rebuild, say): what it does, and the way to the details.
        val u = updates
        val failed = syncError?.takeIf { u != null && !u.busy && u.blocked == null }
        if (config?.managedFlake != null && u != null && (u.busy || u.blocked != null || failed != null)) {
            Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp), verticalAlignment = Alignment.CenterVertically) {
                Text(
                    when {
                        u.busy -> "${updateStatus(u)} ${u.log.lastOrNull().orEmpty()}"
                        failed != null -> "Your settings aren't applied yet: $failed"
                        else -> "An update waits for your yes to compile on the phone."
                    },
                    color = LocalContentColor.current.copy(alpha = 0.8f),
                    fontSize = 12.sp,
                    maxLines = 2,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f)
                )
                if (failed != null) TextButton(onClick = vm.managed::retrySync) { Text("Retry") }
                TextButton(onClick = { showUpdates = true }) { Text("Details") }
            }
        }
        if (missing.isNotEmpty() && state == VmState.Stopped) {
            val needDisk = missing.any { it.name == DISK_REQUIREMENT }
            val others = missing.filter { it.name != DISK_REQUIREMENT }
            Column(Modifier.weight(1f, fill = false).padding(16.dp)) {
                if (others.isNotEmpty()) Requirements(others)
                if (needDisk) SetupCard(vm, canStart = others.isEmpty())
            }
        }
        // Read for its side effect: titles and finished states redraw the row.
        revision.let {}
        TabRow(
            strip = tabStrip(tabs, { it.agentId }, windowTabs),
            selected = selected,
            selectedWindow = shownWindow,
            profiles = config?.profiles?.map { it.name }.orEmpty(),
            canOpen = link is ControlChannel.Link.Up,
            onSelect = vm.tabs::select,
            onClose = { id ->
                val tab = tabs.firstOrNull { it.id == id }
                if (tab != null && tab.session.isRunning) confirm = "Close ${tab.title}? What runs in it gets a hangup." to { vm.tabs.close(id) } else vm.tabs.close(id)
            },
            onSelectWindow = vm.tabs::selectWindow,
            // The client decides: it may ask about unsaved work first.
            onCloseWindow = vm.gui::requestClose,
            tabMenu = { tab -> chipMenu(vm, tab) { renaming = tab } },
            windowMenu = { id ->
                windowMenu(
                    vm.gui,
                    id,
                    listOf(
                        MenuAction("Keyboard") { vm.gui.surfaceOf(id)?.ime?.toggleKeyboard() },
                        MenuAction(MOVE_OUT) { vm.gui.move(id, "") }
                    )
                )
            },
            onOpen = vm::openTab
        )
        Box(Modifier.weight(1f).fillMaxWidth()) {
            AndroidView(
                factory = { controller.create().also { controller.show(vm.tabs.current()) } },
                // Hidden rather than removed: the controller keeps its one view.
                update = { it.visibility = if (shownWindow != 0) View.INVISIBLE else View.VISIBLE },
                modifier = Modifier.fillMaxSize()
            )
            // A zero-size anchor at the touch point: the menu drops from it.
            Box(Modifier.offset { terminalMenuAt ?: IntOffset.Zero }) {
                WindowMenu(
                    expanded = terminalMenuAt != null,
                    onDismiss = {
                        terminalMenuAt = null
                        controller.clearStoredSelection()
                    }
                ) { vm.terminalActions.menu(vm.tabs.current(), controller.storedSelection()) }
            }
            if (shownWindow != 0) {
                key(shownWindow) {
                    AndroidView(
                        factory = { ctx -> vm.gui.tabView(ctx, shownWindow) },
                        onRelease = vm.gui::release,
                        modifier = Modifier.fillMaxSize()
                    )
                }
            }
        }
        // The extra keys and the composer type into the terminal, or into the window tab that shows.
        val ime = windowSurface?.ime
        if (shownWindow != 0 && ime != null) {
            modifierRevision.let {}
            ExtraKeysBar(
                rows = extraKeyRows(config),
                modifiers = ime.modifiers,
                revision = modifierRevision,
                onKey = ime::extraKey
            )
        }
        if (shownWindow == 0) {
            modifierRevision.let {}
            ExtraKeysBar(
                rows = extraKeyRows(config),
                modifiers = controller.modifiers,
                revision = modifierRevision,
                onKey = controller::press
            )
            if (composerOpen) Composer(controller)
        }
    }
}

/** The status bar's line for [phase]. */
internal fun statusText(phase: VmPhase): String = when (phase) {
    VmPhase.Stopping -> "Stopping…"
    VmPhase.Stopped -> "Stopped"
    is VmPhase.Failed -> "Failed: ${phase.message}"
    VmPhase.Starting -> "Starting…"
    is VmPhase.Running -> "Running · ${phase.host ?: "guest"}"
    is VmPhase.Refused -> "Agent refused: ${phase.reason}"
    VmPhase.AgentLate -> "Booting (no agent yet)"
    VmPhase.Booting -> "Booting (waiting for the agent)"
}

/** Asks for a tab's new title, starting from [current]; an empty one clears the user's. */
@Composable
private fun RenameDialog(current: String, onDismiss: () -> Unit, onRename: (String) -> Unit) {
    var title by remember { mutableStateOf(current) }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Rename tab") },
        text = { OutlinedTextField(title, { title = it }, singleLine = true, modifier = Modifier.fillMaxWidth()) },
        confirmButton = {
            TextButton(onClick = {
                onDismiss()
                guard("renaming the tab") { onRename(title) }
            }) { Text("Rename") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } }
    )
}

@Composable
private fun StatusBar(
    status: String,
    running: Boolean,
    canStart: Boolean,
    onStart: () -> Unit,
    onStop: () -> Unit,
    composerOpen: Boolean,
    onComposer: () -> Unit,
    updateAvailable: Boolean,
    onUpdates: () -> Unit,
    /** The ⋮ menu, read when it opens. */
    menu: () -> List<MenuAction>
) {
    var menuOpen by remember { mutableStateOf(false) }
    Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        Text(status, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f))
        if (updateAvailable) TextButton(onClick = onUpdates) { Text("Update") }
        TextButton(onClick = onComposer) { Text(if (composerOpen) "Keys" else "Compose") }
        Box {
            // A TextButton tints its text with the accent; the menu glyph stays the text colour.
            TextButton(onClick = { menuOpen = true }) { Text("⋮", color = MaterialTheme.colorScheme.onBackground) }
            WindowMenu(menuOpen, onDismiss = { menuOpen = false }, items = menu)
        }
        if (running) {
            OutlinedButton(onClick = onStop) { Text("Stop") }
        } else {
            Button(onClick = onStart, enabled = canStart) { Text("Start") }
        }
    }
}

@Composable
private fun TabRow(
    strip: List<TabStripItem<Tab>>,
    selected: Int,
    selectedWindow: Int,
    profiles: List<String>,
    canOpen: Boolean,
    onSelect: (Int) -> Unit,
    onClose: (Int) -> Unit,
    onSelectWindow: (Int) -> Unit,
    onCloseWindow: (Int) -> Unit,
    /** A terminal tab's long-press menu, read when it opens. */
    tabMenu: (Tab) -> List<MenuAction>,
    /** A window tab's menu, read when it opens. */
    windowMenu: (Int) -> List<MenuAction>,
    onOpen: (String?) -> Unit
) {
    var menu by remember { mutableStateOf(false) }
    val foreground = LocalContentColor.current
    Row(
        Modifier.fillMaxWidth().horizontalScroll(rememberScrollState()).padding(horizontal = 4.dp),
        horizontalArrangement = Arrangement.spacedBy(4.dp),
        verticalAlignment = Alignment.CenterVertically
    ) {
        for (item in strip) {
            when (item) {
                is TabStripItem.Terminal -> {
                    val tab = item.tab
                    var tabMenuOpen by remember(tab.id) { mutableStateOf(false) }
                    Box {
                        TabChip(
                            mark = if (tab.attention) "● " else "",
                            title = tab.title + if (tab.ended) " (ended)" else "",
                            selected = selectedWindow == 0 && tab.id == selected,
                            onSelect = { onSelect(tab.id) },
                            onLongPress = { tabMenuOpen = true },
                            onClose = if (tab.kind != Tab.Kind.Console) ({ onClose(tab.id) }) else null
                        )
                        WindowMenu(tabMenuOpen, onDismiss = { tabMenuOpen = false }) { tabMenu(tab) }
                    }
                }

                is TabStripItem.Window -> {
                    val w = item.window
                    var windowMenuOpen by remember(w.id) { mutableStateOf(false) }
                    Box {
                        TabChip(
                            mark = "▣ ",
                            title = w.title,
                            selected = w.id == selectedWindow,
                            onSelect = { onSelectWindow(w.id) },
                            onLongPress = { windowMenuOpen = true },
                            onClose = { onCloseWindow(w.id) }
                        )
                        WindowMenu(windowMenuOpen, onDismiss = { windowMenuOpen = false }) { windowMenu(w.id) }
                    }
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
                    .padding(horizontal = 8.dp, vertical = 2.dp)
            )
            DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                for (p in profiles) {
                    DropdownMenuItem(text = { Text(p) }, onClick = {
                        menu = false
                        onOpen(p)
                    })
                }
            }
        }
    }
}

@Composable
private fun TabChip(
    mark: String,
    title: String,
    selected: Boolean,
    onSelect: () -> Unit,
    onClose: (() -> Unit)?,
    onLongPress: (() -> Unit)? = null
) {
    val foreground = LocalContentColor.current
    Row(
        Modifier
            .background(if (selected) foreground.copy(alpha = 0.18f) else Color.Transparent, RoundedCornerShape(6.dp))
            .combinedClickable(onLongClick = onLongPress, onClick = onSelect)
            .padding(horizontal = 10.dp, vertical = 6.dp),
        verticalAlignment = Alignment.CenterVertically
    ) {
        if (mark.isNotEmpty()) Text(mark, color = foreground, fontSize = 10.sp)
        Text(title, color = foreground.copy(alpha = if (selected) 1f else 0.7f), fontSize = 13.sp, maxLines = 1)
        if (onClose != null) {
            // A finger-sized target, not a 13sp glyph.
            Text("×", color = foreground.copy(alpha = 0.7f), fontSize = 18.sp, modifier = Modifier.clickable(onClick = onClose).padding(start = 10.dp, end = 4.dp))
        }
    }
}

private const val DISK_REQUIREMENT = "Disk image"

/** One prerequisite of booting a VM, with the fix shown while it's missing. */
data class Requirement(val name: String, val fix: String)

fun missingRequirements(context: Context, vm: Vm): List<Requirement> {
    if (vm.settings.tcpBackend) return emptyList()
    fun granted(p: String) = context.checkSelfPermission(p) == PackageManager.PERMISSION_GRANTED
    val grant = "from a computer with adb: adb shell pm grant ${context.packageName}"
    return buildList {
        // PackageManager.FEATURE_VIRTUALIZATION_FRAMEWORK is @SystemApi, so spell it out.
        if (!context.packageManager.hasSystemFeature("android.software.virtualization_framework")) {
            add(Requirement("Android Virtualization Framework", "this device has no AVF"))
        }
        for (p in listOf("MANAGE_VIRTUAL_MACHINE", "USE_CUSTOM_VIRTUAL_MACHINE")) {
            if (!granted("android.permission.$p")) add(Requirement(p, "$grant android.permission.$p"))
        }
        if (!DiskImage.file(context).exists()) add(Requirement(DISK_REQUIREMENT, "set it up below, or run `pocketnix provision --image …`"))
    }
}

@Composable
private fun Requirements(missing: List<Requirement>, modifier: Modifier = Modifier) {
    Column(modifier, verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text("Before the VM can start:", style = MaterialTheme.typography.titleMedium)
        for (r in missing) {
            Text("✗ ${r.name}")
            Text(r.fix, color = LocalContentColor.current.copy(alpha = 0.7f), style = MaterialTheme.typography.bodySmall)
        }
        Spacer(Modifier.padding(4.dp))
    }
}
