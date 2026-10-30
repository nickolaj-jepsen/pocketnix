package dev.nickolaj.pocketnix.ui

import android.content.Context
import android.graphics.Typeface
import android.util.Log
import android.view.KeyEvent
import android.view.MotionEvent
import android.view.inputmethod.InputMethodManager
import com.termux.terminal.KeyHandler
import com.termux.terminal.TerminalSession
import com.termux.view.TerminalView
import com.termux.view.TerminalViewClient
import dev.nickolaj.pocketnix.Settings
import dev.nickolaj.pocketnix.gui.guard
import dev.nickolaj.pocketnix.terminal.ExtraKeys
import dev.nickolaj.pocketnix.terminal.FnKeys
import dev.nickolaj.pocketnix.terminal.KeyAction
import dev.nickolaj.pocketnix.terminal.Modifier
import dev.nickolaj.pocketnix.terminal.Modifiers
import dev.nickolaj.pocketnix.terminal.Paste
import dev.nickolaj.pocketnix.terminal.Tab
import dev.nickolaj.pocketnix.terminal.Tabs

/**
 * Wires one TerminalView to the tabs: shows the selected session, feeds it
 * extra keys, and holds the modifier state the view reads.
 */
class TerminalController(
    private val context: Context,
    private val tabs: Tabs,
    private val settings: Settings,
    val modifiers: Modifiers,
    private val onModifiersChanged: () -> Unit
) {
    var view: TerminalView? = null
        private set

    private var textSize = settings.textSize.takeIf { it > 0 }
        ?: (DEFAULT_TEXT_DP * context.resources.displayMetrics.density).toInt()

    private var tabsView: Tabs.View? = null

    /** Opens a link the user tapped. */
    var onOpenLink: (String) -> Unit = {}

    /** The terminal's context menu is wanted at view-relative pixels: More… on a selection, or a right-click. */
    var onContextMenu: (x: Float, y: Float) -> Unit = { _, _ -> }

    /** The text selected when More… was chosen, until [clearStoredSelection]. */
    fun storedSelection(): String? = view?.storedSelectedText

    fun clearStoredSelection() {
        view?.unsetStoredSelectedText()
    }

    fun create(): TerminalView = TerminalView(context, null).also { v ->
        view = v
        v.setTerminalViewClient(Client())
        v.setTextSize(textSize)
        v.isFocusable = true
        v.isFocusableInTouchMode = true
        v.keepScreenOn = settings.keepScreenOn
        // A held volume key's key-up goes wherever focus went (a dialog, the lock screen).
        v.setOnFocusChangeListener { _, focused -> if (!focused) releaseHeld() }
        v.viewTreeObserver.addOnWindowFocusChangeListener { focused -> if (!focused) releaseHeld() }
        val tv = object : Tabs.View {
            override fun onTextChanged(session: TerminalSession) {
                if (v.currentSession === session) v.onScreenUpdated()
            }

            override fun onColorsChanged() = v.invalidate()
            override fun onCursorStateChange(visible: Boolean) = v.setTerminalCursorBlinkerState(visible, true).let {}
        }
        tabsView = tv
        tabs.view = tv
    }

    /** Detaches the view, which [Tabs] outlives along with the activity it belongs to. */
    fun release() {
        if (tabs.view === tabsView) tabs.view = null
        tabsView = null
        view?.removeCallbacks(applyTextSize)
        if (textSizePending) settings.textSize = textSize
        textSizePending = false
        view = null
    }

    fun setKeepScreenOn(on: Boolean) {
        settings.keepScreenOn = on
        view?.keepScreenOn = on
    }

    private fun releaseHeld() {
        modifiers.releaseHeld()
        onModifiersChanged()
    }

    // A size change reflows the whole scrollback: apply a pinch's at most every [TEXT_SIZE_DELAY_MS].
    private var textSizePending = false
    private val applyTextSize = Runnable {
        textSizePending = false
        settings.textSize = textSize
        view?.setTextSize(textSize)
    }

    fun show(tab: Tab) {
        val v = view ?: return
        if (v.currentSession !== tab.session) v.attachSession(tab.session)
        v.onScreenUpdated()
        // Otherwise a hardware keyboard types into whichever button was last focused.
        v.requestFocus()
    }

    fun setTypeface(t: Typeface?) {
        view?.setTypeface(t ?: Typeface.MONOSPACE)
    }

    fun redraw() {
        view?.invalidate()
    }

    /** Sends one extra key (or its swipe-up popup) as if typed. */
    fun press(key: String) {
        val v = view ?: return
        when (val a = ExtraKeys.action(key)) {
            is KeyAction.Toggle -> {
                modifiers.tap(a.modifier)
                onModifiersChanged()
                return
            }

            is KeyAction.Code -> v.onKeyDown(a.keyCode, KeyEvent(KeyEvent.ACTION_DOWN, a.keyCode))

            is KeyAction.Text -> a.text.codePoints().forEach {
                v.inputCodePoint(TerminalView.KEY_EVENT_SOURCE_VIRTUAL_KEYBOARD, it, false, false)
            }
        }
        onModifiersChanged()
    }

    /** Writes text straight to the session, bypassing key handling. */
    fun write(text: String) {
        view?.currentSession?.write(text)
    }

    /**
     * Types [text] into the current tab as a paste (bracketed when the app
     * asked); [submit] presses Enter. False when no running session took it.
     */
    fun paste(text: String, submit: Boolean): Boolean {
        val session = view?.currentSession ?: return false
        val emulator = session.emulator ?: return false
        if (!session.isRunning) return false
        if (text.isEmpty() && !submit) return true
        session.write(Paste.encode(text, emulator.isBracketedPasteMode, submit))
        return true
    }

    private inner class Client : TerminalViewClient {
        override fun onScale(scale: Float): Float {
            if (scale < 0.9f || scale > 1.1f) {
                val px = (textSize * scale).toInt().coerceIn(MIN_TEXT_PX, MAX_TEXT_PX)
                if (px != textSize) {
                    textSize = px
                    val v = view
                    if (v != null && !textSizePending) {
                        textSizePending = true
                        v.postDelayed(applyTextSize, TEXT_SIZE_DELAY_MS)
                    }
                }
                return 1.0f
            }
            return scale
        }

        override fun onSingleTapUp(e: MotionEvent?) = guard("terminal tap") {
            val v = view ?: return@guard
            val link = e?.let { v.getHyperlinkAt(it.x, it.y) }
            when (val action = tapAction(link, v.currentSession?.emulator?.isMouseTrackingActive == true)) {
                is TapAction.Open -> onOpenLink(action.url)

                TapAction.Keyboard -> {
                    v.requestFocus()
                    context.getSystemService(InputMethodManager::class.java)?.showSoftInput(v, 0)
                }
            }
        }

        override fun shouldBackButtonBeMappedToEscape() = false
        override fun shouldEnforceCharBasedInput() = true
        override fun shouldUseCtrlSpaceWorkaround() = false
        override fun isTerminalViewSelected() = true
        override fun copyModeChanged(copyMode: Boolean) {}

        override fun onKeyDown(keyCode: Int, e: KeyEvent, session: TerminalSession?): Boolean {
            when (keyCode) {
                // Held volume keys are modifiers: down is Ctrl, up is Termux's fn.
                KeyEvent.KEYCODE_VOLUME_DOWN -> modifiers.hold(Modifier.CTRL, true)

                KeyEvent.KEYCODE_VOLUME_UP -> modifiers.hold(Modifier.FN, true)

                KeyEvent.KEYCODE_ENTER -> {
                    // "[Process completed - press Enter]" closes the tab.
                    val tab = tabs.all().firstOrNull { it.session === session }
                    if (tab != null && !session!!.isRunning && tab.kind != Tab.Kind.Console) {
                        tabs.close(tab.id)
                        return true
                    }
                    return false
                }

                else -> return false
            }
            return true
        }

        override fun onKeyUp(keyCode: Int, e: KeyEvent): Boolean {
            when (keyCode) {
                KeyEvent.KEYCODE_VOLUME_DOWN -> modifiers.hold(Modifier.CTRL, false)
                KeyEvent.KEYCODE_VOLUME_UP -> modifiers.hold(Modifier.FN, false)
                else -> return false
            }
            return true
        }

        override fun onLongPress(event: MotionEvent?) = false

        override fun onContextMenu(x: Float, y: Float) = this@TerminalController.onContextMenu(x, y)

        override fun readControlKey() = consume(Modifier.CTRL)
        override fun readAltKey() = consume(Modifier.ALT)
        override fun readShiftKey() = consume(Modifier.SHIFT)

        // The view uses up one-shot modifiers; the key bar must show that.
        private fun consume(m: Modifier): Boolean {
            val wasOn = modifiers.state(m) != Modifiers.State.OFF
            return modifiers.consume(m).also { if (wasOn) onModifiersChanged() }
        }

        // Consumed in onCodePoint, where the fn combination is decided.
        override fun readFnKey() = modifiers.active(Modifier.FN)

        override fun onCodePoint(codePoint: Int, ctrlDown: Boolean, session: TerminalSession): Boolean {
            if (!consume(Modifier.FN)) return false
            val action = FnKeys.action(codePoint) ?: return false
            when (action) {
                is KeyAction.Code -> {
                    val emu = session.emulator ?: return true
                    KeyHandler.getCode(action.keyCode, 0, emu.isCursorKeysApplicationMode, emu.isKeypadApplicationMode)
                        ?.let { session.write(it) }
                }

                is KeyAction.Text -> session.write(action.text)

                is KeyAction.Toggle -> {}
            }
            onModifiersChanged()
            return true
        }

        override fun onEmulatorSet() {
            view?.setTerminalCursorBlinkerState(true, true)
        }

        override fun logError(tag: String?, message: String?) = Log.e(tag, message ?: "").let {}
        override fun logWarn(tag: String?, message: String?) = Log.w(tag, message ?: "").let {}
        override fun logInfo(tag: String?, message: String?) = Log.i(tag, message ?: "").let {}
        override fun logDebug(tag: String?, message: String?) = Log.d(tag, message ?: "").let {}
        override fun logVerbose(tag: String?, message: String?) = Log.v(tag, message ?: "").let {}
        override fun logStackTraceWithMessage(tag: String?, message: String?, e: Exception?) = Log.e(tag, message, e).let {}
        override fun logStackTrace(tag: String?, e: Exception?) = Log.e(tag, "", e).let {}
    }

    private companion object {
        const val DEFAULT_TEXT_DP = 12
        const val MIN_TEXT_PX = 12
        const val MAX_TEXT_PX = 96
        const val TEXT_SIZE_DELAY_MS = 150L
    }
}
