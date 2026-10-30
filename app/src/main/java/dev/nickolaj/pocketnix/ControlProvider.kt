package dev.nickolaj.pocketnix

import android.content.ContentProvider
import android.content.ContentValues
import android.database.Cursor
import android.net.Uri
import android.os.Bundle
import android.os.ParcelFileDescriptor
import android.util.Log
import dev.nickolaj.pocketnix.gui.MenuAction
import dev.nickolaj.pocketnix.provision.requireAdbCaller
import dev.nickolaj.pocketnix.terminal.Tab
import dev.nickolaj.pocketnix.ui.MainItem
import dev.nickolaj.pocketnix.ui.RENAME
import dev.nickolaj.pocketnix.ui.chipMenu
import dev.nickolaj.pocketnix.ui.mainMenu
import dev.nickolaj.pocketnix.ui.mainMenuState
import dev.nickolaj.pocketnix.ui.reopenEndedTabs
import java.io.FileNotFoundException
import java.net.URLEncoder
import kotlin.concurrent.thread
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull

/**
 * Debug builds' scripted checks over adb, answered to the caller rather
 * than in logcat, so they work with the phone locked too. `pocketnix
 * control` wraps them.
 *
 * - `content read --uri content://<package>.control/dump[?tab=<id>&lines=<n>]`:
 *   the VM's state, the last `lines` ([DUMP_LINES]) of every tab's transcript (or one
 *   tab's) and the GUI windows, as text; `error: <message>` when it can't
 * - `content call --uri content://<package>.control --method tab [--arg <profile>]
 *   [--extra input:s:<text>] [--extra enter:b:true] [--extra timeoutMs:l:<ms>]`:
 *   opens a terminal tab once the agent is up (`timeoutMs`, [TAB_TIMEOUT_MS]),
 *   types `input` and, with `enter`, Enter into it, and answers its id as `tab`
 * - `--method type --arg <tab id> --extra input:s:<text> [--extra enter:b:true]`:
 *   types into an open tab
 * - `--method menu --arg <tab id> [--extra label0:s:<label> [--extra label1:s:…]]`:
 *   the tab's context menu (`TerminalActions.menu`, with no selection), or the
 *   page the labels lead to, as `entries`: `<label>\t<enabled|disabled>`
 *   lines; a path ending at an action runs it
 * - `--method chip-menu --arg <tab id> [--extra label0:s:<label> …]`: the
 *   same for the tab's long-press menu (`chipMenu`); Rename… asks on the
 *   screen, so it answers an error here
 * - `--method main-menu [--extra label0:s:<label> …]`: the same for the ⋮
 *   menu (`mainMenu`); entries that open a dialog or ask first answer an error
 * - `--method links --arg <tab id>`: the tab's Open link… list as `links`,
 *   one URL per line, newest first
 * - `--method rename --arg <tab id> --extra title:s:<title>`: gives a terminal
 *   tab its user title; an empty one clears it
 * - `--method reopen --arg <tab id>`: starts an ended terminal tab again
 * - `content read --uri content://<package>.control/stats`: the GUI link's
 *   counters and histograms as one line of JSON (`GuiSession.stats`)
 *
 * `content` takes a backslash in a value as escaping the next character, and
 * splits at unescaped colons: send `\\` for each backslash in `input` and
 * `\:` for each colon.
 * A call's errors come back as an `error` key. A listing comes back URL-encoded
 * (`URLEncoder`), since `content` prints a bundle without quoting.
 */
class ControlProvider : ContentProvider() {
    override fun onCreate() = true

    // Binder threads, never the main one: blocking on it here can't deadlock.
    override fun call(method: String, arg: String?, extras: Bundle?): Bundle {
        requireAdbCaller("control the app")
        val out = Bundle()
        try {
            if (!BuildConfig.DEBUG) error("debug builds only")
            val vm = context!!.vm
            val input = extras?.getString("input").orEmpty()
            val enter = extras?.getBoolean("enter", false) ?: false
            when (method) {
                "tab" -> {
                    val timeout = extras?.getLong("timeoutMs", TAB_TIMEOUT_MS) ?: TAB_TIMEOUT_MS
                    val id = runBlocking { withTimeoutOrNull(timeout) { vm.debugOpenTab(arg, input, enter) } }
                        ?: error("the agent wasn't up within $timeout ms")
                    out.putInt("tab", id)
                }

                "type" -> onMain { vm.debugType(tabId(arg ?: error("type needs a tab id")), input, enter) }

                "menu" -> {
                    val id = tabId(arg ?: error("menu needs a tab id"))
                    menu(out, extras) { vm.terminalActions.menu(tab(vm, id), null) }
                }

                "chip-menu" -> {
                    val id = tabId(arg ?: error("chip-menu needs a tab id"))
                    var asked = false
                    menu(out, extras) { chipMenu(vm, tab(vm, id)) { asked = true } }
                    if (asked) error("$RENAME asks on the screen: use rename")
                }

                "main-menu" -> {
                    var screenOnly: MainItem? = null
                    menu(out, extras) {
                        mainMenu(mainMenuState(context!!, vm, vm.tabs.selectedWindow.value)) { item ->
                            when (item) {
                                MainItem.REOPEN_ENDED -> vm.reopenEndedTabs()
                                MainItem.MOVE_OUT -> vm.gui.move(vm.tabs.selectedWindow.value, "")
                                else -> screenOnly = item
                            }
                        }
                    }
                    screenOnly?.let { error("${it.label} needs the screen") }
                }

                "rename" -> {
                    val t = onMain { tab(vm, tabId(arg ?: error("rename needs a tab id"))) }
                    if (t.kind == Tab.Kind.Console) error("the console can't be renamed")
                    onMain { vm.tabs.rename(t.id, extras?.getString("title").orEmpty()) }
                }

                "reopen" -> {
                    val t = onMain { tab(vm, tabId(arg ?: error("reopen needs a tab id"))) }
                    if (!t.ended) error("tab ${t.id} hasn't ended")
                    onMain { vm.reopenTab(t.id) } ?: error("the agent can't be reached")
                }

                "links" -> {
                    val id = tabId(arg ?: error("links needs a tab id"))
                    out.putString("links", encode(onMain { vm.terminalActions.links(tab(vm, id)) }))
                }

                else -> error("unknown method $method")
            }
            out.putBoolean("ok", true)
        } catch (e: Exception) {
            out.putString("error", e.message ?: e.javaClass.simpleName)
        }
        return out
    }

    override fun openFile(uri: Uri, mode: String): ParcelFileDescriptor {
        requireAdbCaller("control the app")
        if (uri.path != "/dump" && uri.path != "/stats") throw FileNotFoundException(uri.toString())
        val text = try {
            if (!BuildConfig.DEBUG) error("debug builds only")
            if (uri.path == "/stats") {
                context!!.vm.debugStats()
            } else {
                val tab = uri.getQueryParameter("tab")?.let(::tabId)
                val lines = uri.getQueryParameter("lines")?.toIntOrNull() ?: DUMP_LINES
                onMain { context!!.vm.debugDump(tab, lines) }.joinToString("\n")
            }
        } catch (e: Exception) {
            "error: ${e.message ?: e.javaClass.simpleName}"
        }
        val (read, write) = ParcelFileDescriptor.createPipe()
        // A pipe holds 64 KiB: the reader drains it while this writes.
        thread(name = "control-dump", isDaemon = true) {
            runCatching { ParcelFileDescriptor.AutoCloseOutputStream(write).use { it.write("$text\n".toByteArray()) } }
                .onFailure { Log.w(TAG, "dump: ${it.message}") }
        }
        return read
    }

    private fun <T> onMain(block: () -> T): T = runBlocking { withContext(Dispatchers.Main) { block() } }

    private fun tabId(arg: String) = arg.toIntOrNull() ?: error("not a tab id: $arg")

    /**
     * Answers [build]'s menu, walked by the `label<n>` extras: the page reached
     * as `entries`, or nothing once an action ran.
     */
    private fun menu(out: Bundle, extras: Bundle?, build: () -> List<MenuAction>) {
        val labels = generateSequence(0) { it + 1 }.map { extras?.getString("label$it") }.takeWhile { it != null }.filterNotNull().toList()
        val entries = onMain { walk(build(), labels) }
        if (entries != null) out.putString("entries", encode(entries.map { "${it.label}\t${if (it.enabled) "enabled" else "disabled"}" }))
    }

    private fun tab(vm: Vm, id: Int) = vm.tabs.all().firstOrNull { it.id == id } ?: error("no tab $id")

    /**
     * Follows [labels] through [items] as the menu's pages do: returns the
     * entries of the page reached, or runs the action reached and returns null.
     */
    private fun walk(items: List<MenuAction>, labels: List<String>): List<MenuAction>? {
        var page = items
        for ((i, label) in labels.withIndex()) {
            val a = page.firstOrNull { it.label == label } ?: error("no entry '$label'")
            if (!a.enabled) error("'$label' is disabled")
            if (a.children.isEmpty()) {
                if (i != labels.lastIndex) error("'$label' is an action, not a page")
                a.run()
                return null
            }
            page = a.children
        }
        return page
    }

    private fun encode(lines: List<String>) = URLEncoder.encode(lines.joinToString("\n"), "UTF-8")

    override fun query(uri: Uri, projection: Array<out String>?, selection: String?, selectionArgs: Array<out String>?, sortOrder: String?): Cursor? = null
    override fun getType(uri: Uri): String? = null
    override fun insert(uri: Uri, values: ContentValues?): Uri? = null
    override fun delete(uri: Uri, selection: String?, selectionArgs: Array<out String>?): Int = 0
    override fun update(uri: Uri, values: ContentValues?, selection: String?, selectionArgs: Array<out String>?): Int = 0

    private companion object {
        const val TAG = "ControlProvider"
        const val DUMP_LINES = 40
        const val TAB_TIMEOUT_MS = 60_000L
    }
}
