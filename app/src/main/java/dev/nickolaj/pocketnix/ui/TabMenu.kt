package dev.nickolaj.pocketnix.ui

import android.content.Context
import dev.nickolaj.pocketnix.Vm
import dev.nickolaj.pocketnix.agent.ControlChannel
import dev.nickolaj.pocketnix.engine.VmState
import dev.nickolaj.pocketnix.gui.MenuAction
import dev.nickolaj.pocketnix.gui.guard
import dev.nickolaj.pocketnix.terminal.Tab

const val RENAME = "Rename…"
const val NEW_TAB = "New tab"
const val REOPEN = "Reopen"

/**
 * A terminal tab's long-press menu: the console gets only Share transcript,
 * an [ended] tab also Reopen. Every action runs inside [guard]: a throw on
 * the main thread would kill the app and the VM.
 */
fun tabMenu(
    console: Boolean,
    ended: Boolean,
    canOpen: Boolean,
    rename: () -> Unit,
    newTab: () -> Unit,
    shareTranscript: () -> Unit,
    reopen: () -> Unit
): List<MenuAction> = buildList {
    if (ended) add(MenuAction(REOPEN, enabled = canOpen) { guard("reopening the tab") { reopen() } })
    if (!console) {
        add(MenuAction(RENAME) { guard("renaming the tab") { rename() } })
        add(MenuAction(NEW_TAB, enabled = canOpen) { guard("opening a tab") { newTab() } })
    }
    add(MenuAction(SHARE_TRANSCRIPT) { guard("sharing the transcript") { shareTranscript() } })
}

/** [tab]'s long-press menu on [vm]; [rename] asks for the new title. */
fun chipMenu(vm: Vm, tab: Tab, rename: () -> Unit): List<MenuAction> = tabMenu(
    console = tab.kind == Tab.Kind.Console,
    ended = tab.ended,
    canOpen = vm.control.link.value is ControlChannel.Link.Up,
    rename = rename,
    newTab = { (tab.kind as? Tab.Kind.Pty)?.let { vm.openTab(it.profile) } },
    shareTranscript = { vm.terminalActions.shareTranscript(tab) },
    reopen = { vm.reopenTab(tab.id)?.let { vm.tabs.select(it.id) } }
)

/** Starts every ended terminal tab again, each in its place. */
fun Vm.reopenEndedTabs() {
    for (tab in tabs.all().filter { it.ended }) reopenTab(tab.id)
}

/** An entry of the ⋮ menu; [mainMenu] picks which show. */
enum class MainItem(val label: String) {
    APPS("Apps"),
    MOVE_OUT(dev.nickolaj.pocketnix.gui.MOVE_OUT),
    REOPEN_ENDED("Reopen ended tabs"),
    COMMANDS("Commands"),
    UPDATES("System and updates"),
    SETTINGS("Settings"),
    KEEP_SCREEN_ON("Keep screen on"),
    REBOOT("Reboot"),
    RESTART_BOOT_MENU("Restart with boot menu"),
    START_BOOT_MENU("Start with boot menu")
}

/** What the ⋮ menu's entries depend on. */
data class MainMenuState(
    val apps: Boolean,
    val windowShown: Boolean,
    val agentUp: Boolean,
    val endedTabs: Boolean,
    val keepScreenOn: Boolean,
    val running: Boolean,
    val ownsGuest: Boolean,
    val canStart: Boolean
)

/** [vm]'s ⋮ menu state now, with window [shownWindow] on screen (0: the terminal). */
fun mainMenuState(context: Context, vm: Vm, shownWindow: Int) = MainMenuState(
    apps = vm.config.value?.gui?.enable == true || vm.gui.catalog.apps.isNotEmpty(),
    windowShown = shownWindow != 0,
    agentUp = vm.control.link.value is ControlChannel.Link.Up,
    endedTabs = vm.tabs.all().any { it.ended },
    keepScreenOn = vm.settings.keepScreenOn,
    running = vm.state.value == VmState.Running || vm.state.value == VmState.Starting,
    // The debug TCP backend has no console, so no boot menu.
    ownsGuest = vm.engine.value.ownsGuest,
    canStart = missingRequirements(context, vm).isEmpty()
)

/** The ⋮ menu for [s]; choosing an entry calls [run] with it, inside [guard]. */
fun mainMenu(s: MainMenuState, run: (MainItem) -> Unit): List<MenuAction> = buildList {
    fun item(i: MainItem, enabled: Boolean = true, label: String = i.label) = add(MenuAction(label, enabled) { guard("menu: ${i.label}") { run(i) } })
    if (s.apps) item(MainItem.APPS)
    if (s.windowShown) item(MainItem.MOVE_OUT)
    item(MainItem.REOPEN_ENDED, enabled = s.endedTabs && s.agentUp)
    item(MainItem.COMMANDS, enabled = s.agentUp)
    item(MainItem.UPDATES, enabled = s.agentUp)
    item(MainItem.SETTINGS)
    item(MainItem.KEEP_SCREEN_ON, label = if (s.keepScreenOn) "✓ ${MainItem.KEEP_SCREEN_ON.label}" else MainItem.KEEP_SCREEN_ON.label)
    if (s.running) {
        item(MainItem.REBOOT, enabled = s.agentUp)
        if (s.ownsGuest) item(MainItem.RESTART_BOOT_MENU)
    } else if (s.ownsGuest) {
        item(MainItem.START_BOOT_MENU, enabled = s.canStart)
    }
}
