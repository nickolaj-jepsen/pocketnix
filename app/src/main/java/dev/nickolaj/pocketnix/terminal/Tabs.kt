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

class Tab(val id: Int, val kind: Kind, val session: TerminalSession) {
    sealed interface Kind {
        /** The hvc0 console: always there, never closed, the way in when the agent isn't. */
        data object Console : Kind
        data class Pty(val profile: String) : Kind
    }

    val title: String
        get() = when (kind) {
            Kind.Console -> "console"
            is Kind.Pty -> session.title?.takeIf { it.isNotBlank() } ?: kind.profile
        }
}

/** The open terminal tabs. The console tab comes first and stays. */
class Tabs(private val context: Context, consoleBackend: TerminalBackend, private val terminalVersion: String) {
    /** The UI showing a session; set while one is attached. */
    interface View {
        fun onTextChanged(session: TerminalSession)
        fun onColorsChanged()
        fun onCursorStateChange(visible: Boolean)
    }

    @Volatile var view: View? = null

    private var nextId = 0
    private val client = Client()
    val console = Tab(nextId++, Tab.Kind.Console, TerminalSession(consoleBackend, TRANSCRIPT_ROWS, client))

    private val _tabs = MutableStateFlow(listOf(console))
    val tabs: StateFlow<List<Tab>> = _tabs.asStateFlow()

    private val _selected = MutableStateFlow(console.id)
    val selected: StateFlow<Int> = _selected.asStateFlow()

    /** Bumped when a title or a session's running state changes, for the tab row to redraw. */
    private val _revision = MutableStateFlow(0)
    val revision: StateFlow<Int> = _revision.asStateFlow()

    fun open(profile: String, backend: TerminalBackend): Tab {
        val tab = Tab(nextId++, Tab.Kind.Pty(profile), TerminalSession(backend, TRANSCRIPT_ROWS, client))
        _tabs.update { it + tab }
        _selected.value = tab.id
        return tab
    }

    fun select(id: Int) {
        if (_tabs.value.any { it.id == id }) _selected.value = id
    }

    fun close(id: Int) {
        val tab = _tabs.value.firstOrNull { it.id == id } ?: return
        if (tab.kind == Tab.Kind.Console) return
        tab.session.finishIfRunning()
        val remaining = _tabs.value - tab
        _tabs.value = remaining
        if (_selected.value == id) _selected.value = remaining.last().id
    }

    fun current(): Tab = _tabs.value.firstOrNull { it.id == _selected.value } ?: console

    fun all(): List<Tab> = _tabs.value

    private inner class Client : TerminalSessionClient {
        override fun onTextChanged(changedSession: TerminalSession) {
            view?.onTextChanged(changedSession)
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

        override fun onBell(session: TerminalSession) {}

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
