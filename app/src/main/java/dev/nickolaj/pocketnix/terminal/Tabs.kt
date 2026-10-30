package dev.nickolaj.pocketnix.terminal

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.util.Log
import com.termux.terminal.TerminalBackend
import com.termux.terminal.TerminalSession
import com.termux.terminal.TerminalSessionClient
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update

class Tab(
    val id: Int,
    val kind: Kind,
    val session: TerminalSession,
    private val agentTab: () -> String = { "" }
) {
    sealed interface Kind {
        /** The hvc0 console: always there, never closed, the way in when the agent isn't. */
        data object Console : Kind
        data class Pty(val profile: String) : Kind
    }

    /** The title the user gave it, "" for none; see [tabTitle]. Not kept across app restarts. */
    @Volatile var userTitle = ""
        internal set

    val title: String
        get() = when (kind) {
            Kind.Console -> "console"
            is Kind.Pty -> tabTitle(userTitle, session.title, kind.profile)
        }

    /** A terminal tab whose session is over; the console never ends. */
    val ended: Boolean get() = kind != Kind.Console && !session.isRunning

    /** The agent's id for its session (`POCKETNIX_TAB`), "" for the console and until the agent answers. */
    val agentId: String get() = agentTab()

    /** Rang or notified while nobody watched it; cleared when it's shown. */
    @Volatile var attention = false
        internal set
}

/** A terminal tab's title: the [user]'s, else the shell's [osc] title, else the [profile]'s name; blank counts as unset. */
fun tabTitle(user: String, osc: String?, profile: String): String = user.ifBlank { null } ?: osc?.takeIf { it.isNotBlank() } ?: profile

/**
 * The open terminal tabs, and which tab the terminal screen shows: a
 * terminal, or a guest window in a tab ([selectedWindow]). The console tab
 * comes first and stays.
 */
class Tabs(private val context: Context, consoleBackend: TerminalBackend, private val terminalVersion: String) {
    /** The UI showing a session; set while one is attached. */
    interface View {
        fun onTextChanged(session: TerminalSession)
        fun onColorsChanged()
        fun onCursorStateChange(visible: Boolean)
    }

    @Volatile var view: View? = null

    /** Told when a tab asks for attention, and when that's been seen. */
    interface Alerts {
        /** A bell ([title] and [body] null) or an OSC 9/777 notification; [watched] if it was in view. */
        fun alert(tab: Tab, watched: Boolean, appVisible: Boolean, title: String?, body: String?)
        fun seen(tab: Tab)
    }

    @Volatile var alerts: Alerts? = null

    /** Whether the app is in the foreground, so the selected tab is in view. */
    var visible = false
        set(v) {
            field = v
            if (v && _selectedWindow.value == 0) {
                seen(current())
                // Output that arrived while hidden wasn't drawn.
                view?.onTextChanged(current().session)
            }
        }

    private var nextId = 0
    private val client = Client()
    val console = Tab(nextId++, Tab.Kind.Console, session(consoleBackend))

    // Started at a default size rather than at the view's first layout, which a
    // hidden activity never gets; attaching to the view resizes it.
    private fun session(backend: TerminalBackend) = TerminalSession(backend, TRANSCRIPT_ROWS, client).apply { updateSize(80, 24, 1, 1) }

    private val _tabs = MutableStateFlow(listOf(console))
    val tabs: StateFlow<List<Tab>> = _tabs.asStateFlow()

    private val _selected = MutableStateFlow(console.id)
    val selected: StateFlow<Int> = _selected.asStateFlow()

    /** Bumped when a title or a session's running state changes, for the tab row to redraw. */
    private val _revision = MutableStateFlow(0)
    val revision: StateFlow<Int> = _revision.asStateFlow()

    private val _selectedWindow = MutableStateFlow(0)

    /** The guest window shown in place of the selected terminal, or 0. */
    val selectedWindow: StateFlow<Int> = _selectedWindow.asStateFlow()

    /** [agentTab]: the agent's id for the session once it has one. */
    fun open(profile: String, backend: TerminalBackend, agentTab: () -> String = { "" }): Tab {
        val tab = Tab(nextId++, Tab.Kind.Pty(profile), session(backend), agentTab)
        _tabs.update { it + tab }
        select(tab.id)
        return tab
    }

    /**
     * Replaces ended tab [id] with a new one on [backend] in its place, with
     * its id, kind and user title; the old transcript goes. A new [Tab]
     * rather than a changed one: tabs are compared by identity, and an equal
     * list wouldn't reach [tabs]' collectors. Null if [id] isn't an ended tab.
     */
    fun reopen(id: Int, backend: TerminalBackend, agentTab: () -> String = { "" }): Tab? {
        val old = _tabs.value.firstOrNull { it.id == id }?.takeIf { it.ended } ?: return null
        val tab = Tab(id, old.kind, session(backend), agentTab).apply { userTitle = old.userTitle }
        _tabs.update { list -> list.map { if (it === old) tab else it } }
        alerts?.seen(old)
        return tab
    }

    /** Gives terminal tab [id] the user title [title]; blank clears it, so the shell's title shows again. */
    fun rename(id: Int, title: String) {
        val tab = _tabs.value.firstOrNull { it.id == id }?.takeIf { it.kind != Tab.Kind.Console } ?: return
        tab.userTitle = title.trim()
        _revision.update { it + 1 }
    }

    fun select(id: Int) {
        val tab = _tabs.value.firstOrNull { it.id == id } ?: return
        _selected.value = id
        _selectedWindow.value = 0
        if (visible) seen(tab)
    }

    /** Shows guest window [id] in its tab. */
    fun selectWindow(id: Int) {
        _selectedWindow.value = id
    }

    /** The guest windows in tabs are now [ids]: a selected one that left gives way to its terminal. */
    fun windowsChanged(ids: Collection<Int>) {
        val w = _selectedWindow.value
        if (w == 0 || w in ids) return
        _selectedWindow.value = 0
        if (visible) seen(current())
    }

    /** The running terminal tabs a guest window can follow: those the agent gave an id. */
    fun windowTargets(): List<Tab> = _tabs.value.filter { it.agentId.isNotEmpty() && it.session.isRunning }

    fun close(id: Int) {
        val tab = _tabs.value.firstOrNull { it.id == id } ?: return
        if (tab.kind == Tab.Kind.Console) return
        tab.session.finishIfRunning()
        val remaining = _tabs.value - tab
        _tabs.value = remaining
        alerts?.seen(tab)
        if (_selected.value == id) select(remaining.last().id)
    }

    private fun seen(tab: Tab) {
        if (tab.attention) {
            tab.attention = false
            _revision.update { it + 1 }
        }
        alerts?.seen(tab)
    }

    private fun attention(session: TerminalSession, title: String?, body: String?) {
        val tab = _tabs.value.firstOrNull { it.session === session } ?: return
        val watched = visible && tab.id == _selected.value && _selectedWindow.value == 0
        if (!watched && !tab.attention) {
            tab.attention = true
            _revision.update { it + 1 }
        }
        alerts?.alert(tab, watched, visible, title, body)
    }

    fun current(): Tab = _tabs.value.firstOrNull { it.id == _selected.value } ?: console

    fun all(): List<Tab> = _tabs.value

    private inner class Client : TerminalSessionClient {
        override fun onTextChanged(changedSession: TerminalSession) {
            // Hidden or locked, a redraw per output batch is battery for nothing.
            if (visible) view?.onTextChanged(changedSession)
        }

        override fun onTitleChanged(changedSession: TerminalSession) {
            _revision.update { it + 1 }
        }

        override fun onSessionFinished(finishedSession: TerminalSession) {
            _revision.update { it + 1 }
        }

        override fun onCopyTextToClipboard(session: TerminalSession, text: String?) {
            val cm = context.getSystemService(ClipboardManager::class.java) ?: return
            cm.setPrimaryClip(ClipData.newPlainText("terminal", text ?: return))
        }

        override fun onPasteTextFromClipboard(session: TerminalSession?) {
            val cm = context.getSystemService(ClipboardManager::class.java) ?: return
            val text = cm.primaryClip?.takeIf { it.itemCount > 0 }?.getItemAt(0)?.coerceToText(context)?.toString() ?: return
            val emulator = session?.emulator ?: return
            session.write(Paste.encode(text, emulator.isBracketedPasteMode, submit = false))
        }

        override fun onBell(session: TerminalSession) = attention(session, null, null)

        override fun onNotify(session: TerminalSession, title: String?, body: String) = attention(session, title, body)

        override fun onColorsChanged(session: TerminalSession) {
            view?.onColorsChanged()
        }

        override fun onTerminalCursorStateChange(state: Boolean) {
            view?.onCursorStateChange(state)
        }

        override fun getTerminalCursorStyle(): Int? = null

        override fun getTerminalVersionString(): String = terminalVersion

        override fun logError(tag: String?, message: String?) = Log.e(tag, message ?: "").let {}
        override fun logWarn(tag: String?, message: String?) = Log.w(tag, message ?: "").let {}
        override fun logInfo(tag: String?, message: String?) = Log.i(tag, message ?: "").let {}
        override fun logDebug(tag: String?, message: String?) = Log.d(tag, message ?: "").let {}
        override fun logVerbose(tag: String?, message: String?) = Log.v(tag, message ?: "").let {}
        override fun logStackTraceWithMessage(tag: String?, message: String?, e: Exception?) = Log.e(tag, message, e).let {}
        override fun logStackTrace(tag: String?, e: Exception?) = Log.e(tag, "", e).let {}
    }

    private companion object {
        const val TRANSCRIPT_ROWS = 5000
    }
}
