package dev.nickolaj.pocketnix.gui

import android.graphics.Matrix
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.text.InputType
import android.text.TextUtils
import android.view.KeyCharacterMap
import android.view.KeyEvent
import android.view.View
import android.view.WindowInsets
import android.view.inputmethod.BaseInputConnection
import android.view.inputmethod.CompletionInfo
import android.view.inputmethod.CorrectionInfo
import android.view.inputmethod.CursorAnchorInfo
import android.view.inputmethod.EditorInfo
import android.view.inputmethod.ExtractedText
import android.view.inputmethod.ExtractedTextRequest
import android.view.inputmethod.InputConnection
import android.view.inputmethod.InputMethodManager
import android.view.inputmethod.SurroundingText
import android.view.inputmethod.TextAttribute
import dev.nickolaj.pocketnix.terminal.ExtraKeys
import dev.nickolaj.pocketnix.terminal.FnKeys
import dev.nickolaj.pocketnix.terminal.KeyAction
import dev.nickolaj.pocketnix.terminal.Modifier
import dev.nickolaj.pocketnix.terminal.Modifiers

/**
 * A window's phone keyboard (gui-apps.md, Input): the InputConnection
 * Android's IME types into. While the window's client has a text input
 * focused, it follows that text input's state and sends `ime` edits;
 * otherwise committed text is typed as keys. It also takes the keyboard
 * button, virtual keyboards' key events and the extra keys row. Main
 * thread only.
 */
internal class WindowIme(
    private val view: View,
    private val window: Int,
    private val session: GuiSession,
    private val input: WindowInput,
    private val zoom: Zoom,
) {
    private val imm = view.context.getSystemService(InputMethodManager::class.java)
    private val main = Handler(Looper.getMainLooper())
    private val model = TextInputModel()

    /** The extra keys row's modifiers. */
    val modifiers = Modifiers()

    /** The row's modifiers changed: redraw it. */
    var onModifiers: () -> Unit = {}

    /** The text input's cursor rectangle, in window pixels, whenever a state has one. */
    var onCursorRect: (PixelRect) -> Unit = {}

    // This window's text input, or NONE.
    private var state = TextInputState.NONE

    // Each InputConnection is for one generation: one made before a restart edits nothing.
    private var generation = 0
    private var batch = 0

    // The keyboard was opened with the button, so a session's end leaves it up.
    private var userOpened = false

    // Whether the keyboard was up when the window lost focus mid-session: the session that
    // follows focus coming back gets that, not a fresh keyboard, as Android's own fields do.
    private var restore: Boolean? = null
    private var extractToken = -1
    private var monitorCursor = false
    private var reported = IntArray(4) { -2 }
    private val resync = Runnable { guard("window ${window.u}: text input resync") { resync() } }

    // --- The compositor's text input ---

    /** A `text-input` for any window: this one's session starting, changing or ending. */
    fun textInput(s: TextInputState) {
        if (s.active && s.window == window) {
            if (s.session != state.session || !state.active) {
                state = s
                main.removeCallbacks(resync)
                model.reset(if (s.text != null) TextInputModel.Target.CLIENT else TextInputModel.Target.BLIND, s)
                restart()
                if (!view.isFocused) view.requestFocus()
                if (restore != false) imm.showSoftInput(view, 0)
                restore = null
            } else {
                state = s
                when (model.client(s)) {
                    TextInputModel.Agreement.AGREES -> main.removeCallbacks(resync)
                    TextInputModel.Agreement.CAUGHT_UP -> {
                        main.removeCallbacks(resync)
                        // An open batch flushes as it ends.
                        if (batch == 0) flush()
                    }
                    TextInputModel.Agreement.AHEAD -> {}
                    TextInputModel.Agreement.DISAGREES -> {
                        main.removeCallbacks(resync)
                        main.postDelayed(resync, RESYNC_MS)
                    }
                }
                cursorAnchor()
            }
            s.cursorRect?.let(onCursorRect)
        } else if (state.active) {
            state = TextInputState.NONE
            main.removeCallbacks(resync)
            model.reset(TextInputModel.Target.KEYS)
            restart()
            if (!userOpened) imm.hideSoftInputFromWindow(view.windowToken, 0)
        }
    }

    /** The client's text differs from the copy's for longer than a commit's round trip: the client's wins. */
    private fun resync() {
        val fixed = model.resync()
        if (fixed == null) {
            // No state showed the keys a held edit waits for, but they've landed by now.
            if (model.held) flush(hold = false)
            return
        }
        val (ops, textChanged) = fixed
        send(ops)
        if (textChanged) restart() else report()
    }

    private fun restart() {
        generation++
        batch = 0
        reported = IntArray(4) { -2 }
        imm.restartInput(view)
    }

    // --- The keyboard button ---

    /** Opens the phone keyboard, on any window, or hides it when it's up. */
    fun toggleKeyboard() {
        if (view.rootWindowInsets?.isVisible(WindowInsets.Type.ime()) == true) {
            userOpened = false
            imm.hideSoftInputFromWindow(view.windowToken, 0)
            return
        }
        userOpened = true
        restore = null
        if (!view.isFocused) view.requestFocus()
        imm.showSoftInput(view, 0)
    }

    /** The keyboard went away, by any means: a session's end hides it again. */
    fun keyboardHidden() {
        userOpened = false
    }

    /** The window gained or lost Android's window focus. */
    fun windowFocus(focused: Boolean) {
        if (!focused && state.active) restore = view.rootWindowInsets?.isVisible(WindowInsets.Type.ime()) == true
    }

    /** A touch went down on the window: a text field it focuses brings the keyboard up. */
    fun touched() {
        restore = null
    }

    // --- Keys ---

    /**
     * A key event for the window. A virtual keyboard's printable keys are
     * typed as text, its other keys tapped; a hardware keyboard's go
     * through [input] as they are, once the composing text is committed.
     * True when it went to the guest.
     */
    fun key(e: KeyEvent): Boolean {
        if (e.deviceId != KeyCharacterMap.VIRTUAL_KEYBOARD) {
            if (e.isSystem && e.keyCode != KeyEvent.KEYCODE_MENU) return false
            if (e.action == KeyEvent.ACTION_DOWN) settle()
            session.typedOn(e.deviceId)
            return input.key(e)
        }
        return virtualKey(e)
    }

    private fun virtualKey(e: KeyEvent): Boolean {
        if (e.isSystem) return false
        if (e.action == KeyEvent.ACTION_MULTIPLE && e.keyCode == KeyEvent.KEYCODE_UNKNOWN) {
            e.characters?.let { type(it, 0) }
            return true
        }
        val c = e.getUnicodeChar(e.metaState and (KeyEvent.META_CTRL_MASK or KeyEvent.META_ALT_MASK or KeyEvent.META_META_MASK).inv())
        val printable = c != 0 && c and KeyCharacterMap.COMBINING_ACCENT == 0 && !Character.isISOControl(c)
        if (!printable && EvdevKeys.code(e.keyCode, 0, true) == 0) return false
        // Clients repeat nothing they're typed, and a tap is down and up at once.
        if (e.action != KeyEvent.ACTION_DOWN) return true
        val mods = EvdevKeys.mods(e.metaState) and (Gui.MOD_CTRL or Gui.MOD_ALT or Gui.MOD_LOGO)
        if (printable) {
            type(String(Character.toChars(c)), mods)
        } else {
            press(e.keyCode, mods)
        }
        return true
    }

    /** One extra key, or its swipe-up popup, by name. */
    fun extraKey(name: String) = guard("window ${window.u}: extra key $name") {
        when (val a = ExtraKeys.action(name)) {
            is KeyAction.Toggle -> modifiers.tap(a.modifier)
            is KeyAction.Code -> press(a.keyCode, consumeModifiers())
            is KeyAction.Text -> {
                val fn = modifiers.consume(Modifier.FN)
                val cp = a.text.codePointAt(0)
                when (val f = if (fn && a.text.codePointCount(0, a.text.length) == 1) FnKeys.action(cp) else null) {
                    is KeyAction.Code -> press(f.keyCode, consumeModifiers())
                    is KeyAction.Text -> type(f.text, 0)
                    else -> type(a.text, 0)
                }
            }
        }
        onModifiers()
    }

    /** The row's Ctrl, Alt and Shift as MOD_* bits, a one-shot one used up. */
    private fun consumeModifiers(): Int {
        var m = 0
        if (modifiers.consume(Modifier.CTRL)) m = m or Gui.MOD_CTRL
        if (modifiers.consume(Modifier.ALT)) m = m or Gui.MOD_ALT
        if (modifiers.consume(Modifier.SHIFT)) m = m or Gui.MOD_SHIFT
        return m
    }

    /**
     * Text from a key: with Ctrl or Alt (the event's [mods], or the row's) a
     * key combination; else an edit like the IME's own.
     */
    private fun type(text: String, mods: Int) {
        var m = mods
        if (modifiers.active(Modifier.CTRL) || modifiers.active(Modifier.ALT)) {
            m = m or consumeModifiers()
            onModifiers()
        }
        if (m and (Gui.MOD_CTRL or Gui.MOD_ALT or Gui.MOD_LOGO) != 0) {
            settle()
            // The keysym's level gives Shift: Ctrl+Shift+C is Ctrl with "C".
            input.type(if (m and Gui.MOD_SHIFT != 0) text.uppercase() else text, m and Gui.MOD_SHIFT.inv())
            return
        }
        model.finishComposingText()
        model.commitText(text, 1)
        flush()
    }

    /** A non-printing key (an Android key code) as a tap, the composing text committed first. */
    private fun press(keyCode: Int, mods: Int) {
        settle()
        val code = EvdevKeys.code(keyCode, 0, true)
        if (code == 0) return
        val predicted = mods == 0
        when {
            predicted && keyCode == KeyEvent.KEYCODE_DEL -> model.deleteKey(forward = false)
            predicted && keyCode == KeyEvent.KEYCODE_FORWARD_DEL -> model.deleteKey(forward = true)
            predicted && keyCode == KeyEvent.KEYCODE_DPAD_LEFT -> model.moveKey(right = false)
            predicted && keyCode == KeyEvent.KEYCODE_DPAD_RIGHT -> model.moveKey(right = true)
            keyCode == KeyEvent.KEYCODE_ENTER || keyCode == KeyEvent.KEYCODE_NUMPAD_ENTER -> model.newLine()
        }
        input.tap(code, 1, mods)
        report()
    }

    /** Commits the composing text and sends whatever is pending, before keys that act where it stands. */
    private fun settle() {
        model.finishComposingText()
        // The key that follows can't wait with a held edit, and mustn't overtake it.
        flush(hold = false)
    }

    // --- The InputConnection ---

    /** For the window view's onCreateInputConnection. */
    fun connect(out: EditorInfo): InputConnection {
        val s = state
        if (s.active) {
            val (type, options) = editorType(s.purpose, s.hints)
            out.inputType = type
            out.imeOptions = options or EditorInfo.IME_FLAG_NO_FULLSCREEN or EditorInfo.IME_FLAG_NO_EXTRACT_UI
        } else {
            // No text field: what's typed becomes keys at once, so no suggestions to compose.
            out.inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_VISIBLE_PASSWORD or InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS
            out.imeOptions = EditorInfo.IME_ACTION_NONE or EditorInfo.IME_FLAG_NO_ENTER_ACTION or
                EditorInfo.IME_FLAG_NO_FULLSCREEN or EditorInfo.IME_FLAG_NO_EXTRACT_UI
        }
        out.initialSelStart = model.selectionStart
        out.initialSelEnd = model.selectionEnd
        out.setInitialSurroundingText(model.text)
        return Connection(generation)
    }

    private fun flush(hold: Boolean = true) {
        val was = model.held
        send(model.flush(hold))
        if (model.held && !was) {
            main.removeCallbacks(resync)
            main.postDelayed(resync, RESYNC_MS)
        }
        report()
    }

    private fun send(ops: List<TextInputModel.Op>) {
        for (op in ops) {
            when (op) {
                is TextInputModel.Op.Edit -> if (state.active) {
                    val serial = session.nextImeSerial()
                    model.lastSerial = serial
                    session.send(Gui.ime(state.session, serial, op.edit))
                }
                is TextInputModel.Op.Type -> input.type(op.text)
                is TextInputModel.Op.Key -> input.tap(op.key, op.count, op.mods)
            }
        }
    }

    /** Tells the IME where the selection and composing text are, as an editor does after every change. */
    private fun report() {
        val now = intArrayOf(model.selectionStart, model.selectionEnd, model.composeStart, model.composeEnd)
        if (!now.contentEquals(reported)) {
            reported = now
            imm.updateSelection(view, now[0], now[1], now[2], now[3])
            cursorAnchor()
        }
        if (extractToken >= 0) imm.updateExtractedText(view, extractToken, extracted())
    }

    private fun extracted(): ExtractedText = ExtractedText().apply {
        text = model.text
        startOffset = 0
        partialStartOffset = -1
        partialEndOffset = -1
        selectionStart = model.selectionStart
        selectionEnd = model.selectionEnd
        flags = if ("multiline" in state.hints) 0 else ExtractedText.FLAG_SINGLE_LINE
    }

    /** The cursor's place on screen, for an IME that asked to follow it. */
    private fun cursorAnchor() {
        if (!monitorCursor) return
        val b = CursorAnchorInfo.Builder()
            .setSelectionRange(model.selectionStart, model.selectionEnd)
        if (model.composing) b.setComposingText(model.composeStart, model.text.substring(model.composeStart, model.composeEnd))
        state.cursorRect?.let { r ->
            val left = zoom.x + r.x * zoom.scale
            val top = zoom.y + r.y * zoom.scale
            val bottom = zoom.y + (r.y + r.height) * zoom.scale
            b.setInsertionMarkerLocation(left, top, bottom, bottom, CursorAnchorInfo.FLAG_HAS_VISIBLE_REGION)
        }
        val loc = IntArray(2)
        view.getLocationOnScreen(loc)
        b.setMatrix(Matrix().apply { setTranslate(loc[0].toFloat(), loc[1].toFloat()) })
        imm.updateCursorAnchorInfo(view, b.build())
    }

    fun release() {
        main.removeCallbacks(resync)
        generation++
    }

    /**
     * One IME connection. Every edit goes to the model, and the client
     * hears of it once the outermost batch ends; a connection from before a
     * restart does nothing.
     */
    private inner class Connection(private val gen: Int) : BaseInputConnection(view, true) {
        private val live: Boolean get() = gen == generation

        private inline fun edit(what: String, block: () -> Unit): Boolean {
            if (!live) return false
            guard("window ${window.u}: ime $what") {
                block()
                if (batch == 0) flush()
            }
            return true
        }

        override fun beginBatchEdit(): Boolean {
            if (!live) return false
            batch++
            return true
        }

        override fun endBatchEdit(): Boolean {
            if (!live) return false
            if (batch > 0 && --batch == 0) guard("window ${window.u}: ime batch") { flush() }
            return batch > 0
        }

        override fun commitText(text: CharSequence?, newCursorPosition: Int): Boolean = edit("commit") {
            val t = text?.toString().orEmpty()
            when {
                latched() -> typeLatched(t)
                // Return in a single-line field is its action; in any field it's what a keyboard's Return does.
                t == "\n" -> press(KeyEvent.KEYCODE_ENTER, 0)
                else -> model.commitText(t, newCursorPosition)
            }
        }

        override fun commitText(text: CharSequence, newCursorPosition: Int, textAttribute: TextAttribute?): Boolean =
            commitText(text, newCursorPosition)

        override fun setComposingText(text: CharSequence?, newCursorPosition: Int): Boolean = edit("compose") {
            val t = text?.toString().orEmpty()
            if (latched()) {
                // The new character only: what the IME composed before it is committed as it was.
                val old = if (model.composing) model.text.substring(model.composeStart, model.composeEnd) else ""
                typeLatched(if (t.startsWith(old)) t.substring(old.length) else t)
            } else {
                model.setComposingText(t, newCursorPosition)
            }
        }

        override fun setComposingText(text: CharSequence, newCursorPosition: Int, textAttribute: TextAttribute?): Boolean =
            setComposingText(text, newCursorPosition)

        override fun setComposingRegion(start: Int, end: Int): Boolean = edit("region") { model.setComposingRegion(start, end) }

        override fun setComposingRegion(start: Int, end: Int, textAttribute: TextAttribute?): Boolean = setComposingRegion(start, end)

        override fun finishComposingText(): Boolean = edit("finish") { model.finishComposingText() }

        override fun deleteSurroundingText(beforeLength: Int, afterLength: Int): Boolean =
            edit("delete") { model.deleteSurroundingText(beforeLength, afterLength) }

        override fun deleteSurroundingTextInCodePoints(beforeLength: Int, afterLength: Int): Boolean =
            edit("delete") { model.deleteSurroundingTextInCodePoints(beforeLength, afterLength) }

        override fun setSelection(start: Int, end: Int): Boolean {
            var ok = false
            edit("select") { ok = model.setSelection(start, end) }
            return ok
        }

        override fun replaceText(start: Int, end: Int, text: CharSequence, newCursorPosition: Int, textAttribute: TextAttribute?): Boolean {
            var ok = false
            edit("replace") { ok = model.replaceText(start, end, text, newCursorPosition) }
            return ok
        }

        override fun getTextBeforeCursor(length: Int, flags: Int): CharSequence? = if (live) model.textBeforeCursor(length) else null

        override fun getTextAfterCursor(length: Int, flags: Int): CharSequence? = if (live) model.textAfterCursor(length) else null

        override fun getSelectedText(flags: Int): CharSequence? = if (live) model.selectedText() else null

        override fun getSurroundingText(beforeLength: Int, afterLength: Int, flags: Int): SurroundingText? {
            if (!live) return null
            return guard("window ${window.u}: surrounding text", null) {
                val before = model.textBeforeCursor(beforeLength)
                val after = model.textAfterCursor(afterLength)
                val start = model.selectionStart - before.length
                val t = model.text.substring(start, model.selectionEnd + after.length)
                SurroundingText(t, model.selectionStart - start, model.selectionEnd - start, start)
            }
        }

        override fun getCursorCapsMode(reqModes: Int): Int =
            if (live) guard("window ${window.u}: caps mode", 0) { TextUtils.getCapsMode(model.text, model.selectionStart, reqModes) } else 0

        override fun getExtractedText(request: ExtractedTextRequest?, flags: Int): ExtractedText? {
            if (!live) return null
            if (flags and GET_EXTRACTED_TEXT_MONITOR != 0) extractToken = request?.token ?: -1
            return extracted()
        }

        override fun getEditable() = null

        override fun sendKeyEvent(event: KeyEvent): Boolean {
            if (!live) return false
            guard("window ${window.u}: ime key") { virtualKey(event) }
            return true
        }

        override fun performEditorAction(actionCode: Int): Boolean {
            if (!live) return false
            guard("window ${window.u}: ime action") { press(KeyEvent.KEYCODE_ENTER, 0) }
            return true
        }

        override fun performContextMenuAction(id: Int): Boolean {
            if (!live) return false
            val key = when (id) {
                android.R.id.selectAll -> KEY_A
                android.R.id.cut -> KEY_X
                android.R.id.copy -> KEY_C
                android.R.id.paste -> KEY_V
                else -> return false
            }
            guard("window ${window.u}: ime menu action") {
                settle()
                input.tap(key, 1, Gui.MOD_CTRL)
            }
            return true
        }

        override fun requestCursorUpdates(cursorUpdateMode: Int): Boolean {
            if (!live) return false
            monitorCursor = cursorUpdateMode and InputConnection.CURSOR_UPDATE_MONITOR != 0
            if (cursorUpdateMode and InputConnection.CURSOR_UPDATE_IMMEDIATE != 0) {
                val was = monitorCursor
                monitorCursor = true
                guard("window ${window.u}: cursor anchor") { cursorAnchor() }
                monitorCursor = was
            }
            return true
        }

        override fun commitCompletion(text: CompletionInfo?): Boolean = text?.text?.let { commitText(it, 1) } ?: false

        override fun commitCorrection(correctionInfo: CorrectionInfo?): Boolean = live

        override fun performPrivateCommand(action: String?, data: Bundle?): Boolean = false

        override fun clearMetaKeyStates(states: Int): Boolean = live

        override fun closeConnection() {
            if (live) guard("window ${window.u}: ime close") { settle() }
            batch = 0
            super.closeConnection()
        }

        /** Whether the extra keys row holds Ctrl or Alt for the next character. */
        private fun latched(): Boolean = modifiers.active(Modifier.CTRL) || modifiers.active(Modifier.ALT)

        /** Text typed with the row's latched modifiers: a key combination, and no composing text left behind. */
        private fun typeLatched(t: String) {
            settle()
            val m = consumeModifiers()
            onModifiers()
            if (t.isNotEmpty()) input.type(if (m and Gui.MOD_SHIFT != 0) t.uppercase() else t, m and Gui.MOD_SHIFT.inv())
            // The IME still composes what it thinks it typed: tell it there's no composing text.
            reported = IntArray(4) { -2 }
        }
    }

    companion object {
        // Longer than a client's round trip for an edit, so a commit that crossed one isn't taken for a change.
        private const val RESYNC_MS = 200L

        private const val KEY_A = 30
        private const val KEY_X = 45
        private const val KEY_C = 46
        private const val KEY_V = 47

        /** EditorInfo's inputType and imeOptions for a text input's content purpose and hints. */
        fun editorType(purpose: String, hints: Set<String>): Pair<Int, Int> {
            var type = when (purpose) {
                "digits" -> InputType.TYPE_CLASS_NUMBER
                "number" -> InputType.TYPE_CLASS_NUMBER or InputType.TYPE_NUMBER_FLAG_DECIMAL or InputType.TYPE_NUMBER_FLAG_SIGNED
                "pin" -> InputType.TYPE_CLASS_NUMBER or InputType.TYPE_NUMBER_VARIATION_PASSWORD
                "phone" -> InputType.TYPE_CLASS_PHONE
                "date" -> InputType.TYPE_CLASS_DATETIME or InputType.TYPE_DATETIME_VARIATION_DATE
                "time" -> InputType.TYPE_CLASS_DATETIME or InputType.TYPE_DATETIME_VARIATION_TIME
                "datetime" -> InputType.TYPE_CLASS_DATETIME
                "url" -> InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_URI
                "email" -> InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_EMAIL_ADDRESS
                "name" -> InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PERSON_NAME
                "password" -> InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD
                // A terminal takes keys as typed: no suggestions to compose.
                "terminal" -> InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS
                else -> InputType.TYPE_CLASS_TEXT
            }
            var options = EditorInfo.IME_ACTION_NONE or EditorInfo.IME_FLAG_NO_ENTER_ACTION
            if (type and InputType.TYPE_MASK_CLASS == InputType.TYPE_CLASS_TEXT) {
                if ("hidden-text" in hints && purpose != "password") {
                    type = type and InputType.TYPE_MASK_VARIATION.inv() or InputType.TYPE_TEXT_VARIATION_PASSWORD
                }
                if ("spellcheck" in hints) type = type or InputType.TYPE_TEXT_FLAG_AUTO_CORRECT
                if ("completion" in hints) type = type or InputType.TYPE_TEXT_FLAG_AUTO_COMPLETE
                when {
                    "uppercase" in hints -> type = type or InputType.TYPE_TEXT_FLAG_CAP_CHARACTERS
                    "titlecase" in hints -> type = type or InputType.TYPE_TEXT_FLAG_CAP_WORDS
                    "auto-capitalization" in hints -> type = type or InputType.TYPE_TEXT_FLAG_CAP_SENTENCES
                }
                if ("multiline" in hints) type = type or InputType.TYPE_TEXT_FLAG_MULTI_LINE
                if ("sensitive-data" in hints) type = type or InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS
            }
            if ("sensitive-data" in hints || "hidden-text" in hints) options = options or EditorInfo.IME_FLAG_NO_PERSONALIZED_LEARNING
            if ("latin" in hints) options = options or EditorInfo.IME_FLAG_FORCE_ASCII
            return type to options
        }
    }
}
