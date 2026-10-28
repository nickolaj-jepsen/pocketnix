package dev.nickolaj.pocketnix.gui

import kotlin.math.abs
import kotlin.math.max
import kotlin.math.min

/** UTF-8 lengths of UTF-16 text, and back (gui-protocol.md, Units). */
object Utf8 {
    /** The bytes [s] from [from] to [to] (UTF-16 indices) takes in UTF-8, as the wire encodes it. */
    fun length(s: CharSequence, from: Int = 0, to: Int = s.length): Int {
        var n = 0
        var i = from
        while (i < to) {
            val c = s[i].code
            n += when {
                c < 0x80 -> 1
                c < 0x800 -> 2
                Character.isHighSurrogate(s[i]) && i + 1 < to && Character.isLowSurrogate(s[i + 1]) -> {
                    i++
                    4
                }
                // A lone surrogate is encoded as '?'.
                Character.isSurrogate(s[i]) -> 1
                else -> 3
            }
            i++
        }
        return n
    }

    /** The UTF-16 index of byte offset [bytes] in [s], clamped to it and down to a character's start. */
    fun index(s: String, bytes: Int): Int {
        var n = 0
        var i = 0
        while (i < s.length) {
            val w = if (Character.isHighSurrogate(s[i]) && i + 1 < s.length && Character.isLowSurrogate(s[i + 1])) 2 else 1
            n += length(s, i, i + w)
            if (n > bytes) return i
            i += w
        }
        return s.length
    }
}

/**
 * One window's text as Android's IME edits it, and as the guest client
 * has it (gui-protocol.md, Text input). The IME's calls change [text], its
 * selection and composing region with InputConnection's semantics;
 * [flush] turns the difference from the client's text into what the
 * client is sent, and [client] and [resync] bring the copy back in line
 * with the client's states. Positions are UTF-16 indices. Not
 * thread-safe.
 */
internal class TextInputModel {
    /** Where edits go. */
    enum class Target {
        /** No text input: committed text is typed as keys, and the composing text stays here until then. */
        KEYS,

        /** A text input that sends its surrounding text. */
        CLIENT,

        /** A text input without surrounding text: committed text, deletes and moves go as keys; the preedit as edits. */
        BLIND,
    }

    /** What the client is sent, in order. */
    sealed interface Op {
        data class Edit(val edit: ImeEdit) : Op

        /** Text typed as key presses (`text`). */
        data class Type(val text: String) : Op

        /** [count] taps of evdev [key] with [mods] (MOD_* bits) added. */
        data class Key(val key: Int, val count: Int = 1, val mods: Int = 0) : Op
    }

    var target = Target.KEYS
        private set
    var text = ""
        private set
    var anchor = 0
        private set
    var cursor = 0
        private set

    /** The composing region, -1 for none. */
    var composeStart = -1
        private set
    var composeEnd = -1
        private set

    val selectionStart: Int get() = min(anchor, cursor)
    val selectionEnd: Int get() = max(anchor, cursor)
    val composing: Boolean get() = composeStart in 0 until composeEnd

    // The client's text, without its preedit, as last known or as the edits sent so far leave it.
    private var base = ""
    private var baseAnchor = 0
    private var baseCursor = 0

    // The preedit the client shows, and its cursor in bytes.
    private var shown = ""
    private var shownBegin = 0
    private var shownEnd = 0

    /** The serial of the last edit sent in this session. */
    var lastSerial = 0

    /**
     * Whether an edit waits for the client to apply the keys [flush] sent
     * before it: GTK and Qt queue key events but apply a text-input `done`
     * as it arrives, so an edit sent with the keys overtakes them. A
     * [client] state that shows the keys applied releases it.
     */
    var held = false
        private set

    // The client's last caught-up state that disagrees, until resync or a state that agrees.
    private var disagreeing: TextInputState? = null

    /** Starts over for [target]: a new session with the client's [state], or no text input. */
    fun reset(target: Target, state: TextInputState? = null) {
        this.target = target
        val t = state?.text.takeIf { target == Target.CLIENT }.orEmpty()
        text = t
        cursor = state?.cursor?.takeIf { target == Target.CLIENT } ?: 0
        anchor = state?.anchor?.takeIf { target == Target.CLIENT } ?: 0
        composeStart = -1
        composeEnd = -1
        base = t
        baseCursor = cursor
        baseAnchor = anchor
        shown = ""
        shownBegin = 0
        shownEnd = 0
        lastSerial = 0
        held = false
        disagreeing = null
    }

    // --- InputConnection's edits ---

    fun commitText(t: CharSequence, newCursorPosition: Int) {
        val (a, b) = replaced()
        replace(a, b, t.toString())
        clearComposing()
        place(a, a + t.length, newCursorPosition)
    }

    fun setComposingText(t: CharSequence, newCursorPosition: Int) {
        val (a, b) = replaced()
        replace(a, b, t.toString())
        if (t.isEmpty()) clearComposing() else setComposing(a, a + t.length)
        place(a, a + t.length, newCursorPosition)
    }

    fun setComposingRegion(start: Int, end: Int) {
        val a = boundary(min(start, end).coerceIn(0, text.length))
        val b = boundary(max(start, end).coerceIn(0, text.length))
        if (a == b) clearComposing() else setComposing(a, b)
    }

    fun finishComposingText() = clearComposing()

    /** Deletes around the selection and the composing text, in UTF-16 units, never half a character. */
    fun deleteSurroundingText(before: Int, after: Int) {
        var a = selectionStart
        var b = selectionEnd
        if (composing) {
            a = min(a, composeStart)
            b = max(b, composeEnd)
        }
        val end = boundaryUp((b + max(after, 0)).coerceAtMost(text.length))
        if (end > b) replace(b, end, "")
        val start = boundary((a - max(before, 0)).coerceAtLeast(0))
        if (start < a) replace(start, a, "")
    }

    fun deleteSurroundingTextInCodePoints(before: Int, after: Int) {
        var a = selectionStart
        var b = selectionEnd
        if (composing) {
            a = min(a, composeStart)
            b = max(b, composeEnd)
        }
        deleteSurroundingText(a - offsetByCodePoints(a, -max(before, 0)), offsetByCodePoints(b, max(after, 0)) - b)
    }

    /** False when the range is outside the text, which Android ignores. */
    fun setSelection(start: Int, end: Int): Boolean {
        if (start !in 0..text.length || end !in 0..text.length) return false
        anchor = boundary(start)
        cursor = boundary(end)
        return true
    }

    fun replaceText(start: Int, end: Int, t: CharSequence, newCursorPosition: Int): Boolean {
        if (start !in 0..text.length || end !in 0..text.length) return false
        clearComposing()
        val a = boundary(min(start, end))
        val b = boundaryUp(max(start, end))
        replace(a, b, t.toString())
        place(a, a + t.length, newCursorPosition)
        return true
    }

    fun textBeforeCursor(n: Int): String =
        text.substring(boundaryUp((selectionStart - max(n, 0)).coerceAtLeast(0)).coerceAtMost(selectionStart), selectionStart)

    fun textAfterCursor(n: Int): String =
        text.substring(selectionEnd, boundary((selectionEnd + max(n, 0)).coerceAtMost(text.length)).coerceAtLeast(selectionEnd))

    fun selectedText(): String? = if (anchor == cursor) null else text.substring(selectionStart, selectionEnd)

    // --- Keys the client gets as keys ---

    /**
     * BackSpace or Delete as a key: the copy deletes what the client will,
     * the selection or a character. Flush first.
     */
    fun deleteKey(forward: Boolean): Op {
        val (a, b) = if (anchor != cursor) {
            selectionStart to selectionEnd
        } else if (forward) {
            cursor to offsetByCodePoints(cursor, 1)
        } else {
            offsetByCodePoints(cursor, -1) to cursor
        }
        val known = disagreeing == null && base == text
        if (a < b) {
            replace(a, b, "")
            anchor = a
            cursor = a
            if (known) predictBase()
        }
        return Op.Key(if (forward) KEY_DELETE else KEY_BACKSPACE)
    }

    /** Left or Right as a key, which moves or collapses the selection by a character. Flush first. */
    fun moveKey(right: Boolean): Op {
        val known = disagreeing == null && base == text && baseCursor == cursor && baseAnchor == anchor
        val to = when {
            anchor != cursor -> if (right) selectionEnd else selectionStart
            else -> offsetByCodePoints(cursor, if (right) 1 else -1)
        }
        anchor = to
        cursor = to
        if (known) predictBase()
        return Op.Key(if (right) KEY_RIGHT else KEY_LEFT)
    }

    /** Return without surrounding text starts a new line the copy can't follow: forget what it held. */
    fun newLine() {
        if (target == Target.CLIENT) return
        text = ""
        anchor = 0
        cursor = 0
        clearComposing()
        base = ""
        baseAnchor = 0
        baseCursor = 0
    }

    private fun predictBase() {
        base = text
        baseCursor = cursor
        baseAnchor = anchor
    }

    // --- To the client ---

    /**
     * What the client must be sent so that it has the copy's text, selection
     * and composing text. With [hold], a text input with surrounding text
     * gets keys and the edits that follow them in separate flushes (see
     * [held]), and nothing while an edit is held.
     */
    fun flush(hold: Boolean = true): List<Op> {
        val out = ArrayList<Op>()
        if (held && hold) return out
        held = false
        val waits = hold && target == Target.CLIENT
        val newText: String
        val preedit: String
        val newCursor: Int
        val newAnchor: Int
        if (composing) {
            newText = text.removeRange(composeStart, composeEnd)
            preedit = text.substring(composeStart, composeEnd)
            newCursor = composeStart
            newAnchor = composeStart
        } else {
            newText = text
            preedit = ""
            newCursor = cursor
            newAnchor = anchor
        }
        val sentPreedit = if (target == Target.KEYS) "" else preedit
        val (pb, pe) = preeditCursor(preedit)

        // A change within the client's selection that leaves text in its place: a commit replaces the selection.
        var replacing: Pair<Int, Int>? = null
        if (newText != base && baseAnchor != baseCursor) {
            val lo = min(baseAnchor, baseCursor)
            val hi = max(baseAnchor, baseCursor)
            val (p, e) = region(newText, newCursor)
            if (target == Target.CLIENT && lo <= p && e <= hi && newText.length - (base.length - hi) > lo) {
                replacing = lo to hi
            } else {
                clearShown(out)
                if (p <= lo && hi <= e) {
                    // Deleting a selection takes a key.
                    out += Op.Key(KEY_BACKSPACE)
                    base = base.removeRange(lo, hi)
                } else {
                    // Left collapses a selection to its start.
                    out += Op.Key(KEY_LEFT)
                }
                baseAnchor = lo
                baseCursor = lo
                if (waits) return held(out)
            }
        }

        var afterEdit = baseCursor
        if (newText != base) {
            val (p, e) = replacing ?: region(newText, newCursor)
            val r = newText.substring(p, newText.length - (base.length - e))
            if (baseCursor < p || baseCursor > e) {
                clearShown(out)
                moveTo(out, if (baseCursor < p) p else e)
                if (waits) return held(out)
            }
            afterEdit = p + r.length
            val moveAfter = afterEdit != newCursor || newAnchor != newCursor
            val keep = if (moveAfter) "" else sentPreedit
            edit(out, p, e, r, keep, pb, pe)
            base = newText
            baseCursor = afterEdit
            baseAnchor = afterEdit
        }
        if (baseCursor != newCursor || baseAnchor != newAnchor) {
            clearShown(out)
            if (newAnchor == newCursor) {
                moveTo(out, newCursor)
            } else {
                moveTo(out, newAnchor)
                moveTo(out, newCursor, Gui.MOD_SHIFT)
                baseAnchor = newAnchor
            }
        }
        if (sentPreedit != shown || (sentPreedit.isNotEmpty() && (pb != shownBegin || pe != shownEnd))) {
            if (waits && out.any { it is Op.Key }) return held(out)
            show(out, sentPreedit, pb, pe)
        }
        return out
    }

    private fun held(out: List<Op>): List<Op> {
        held = true
        return out
    }

    /** The edit [from, to) of the client's text to [r], with [preedit] shown after it. */
    private fun edit(out: MutableList<Op>, from: Int, to: Int, r: String, preedit: String, pb: Int, pe: Int) {
        when (target) {
            Target.CLIENT -> {
                // The deletes exclude a selection, which the commit replaces.
                val lo = min(baseAnchor, baseCursor)
                val hi = max(baseAnchor, baseCursor)
                out += Op.Edit(ImeEdit(Utf8.length(base, from, lo), Utf8.length(base, hi, to), r, preedit, pb, pe))
                shown = preedit
                shownBegin = pb
                shownEnd = pe
            }
            Target.BLIND, Target.KEYS -> {
                val back = codePoints(base, from, baseCursor)
                val forward = codePoints(base, baseCursor, to)
                if (back + forward > 0) clearShown(out)
                if (back > 0) out += Op.Key(KEY_BACKSPACE, back)
                if (forward > 0) out += Op.Key(KEY_DELETE, forward)
                // A client that sends no text can't be seen to drop an edit (foot ignores a `done` that
                // crossed one of its commits): what's committed goes as keys, only the preedit as an edit.
                if (r.isNotEmpty()) {
                    clearShown(out)
                    out += Op.Type(r)
                }
                if (target == Target.BLIND && preedit != shown) {
                    out += Op.Edit(ImeEdit(preedit = preedit, preeditCursorBegin = pb, preeditCursorEnd = pe))
                    shown = preedit
                    shownBegin = pb
                    shownEnd = pe
                }
            }
        }
    }

    private fun show(out: MutableList<Op>, preedit: String, pb: Int, pe: Int) {
        if (target == Target.KEYS) return
        out += Op.Edit(ImeEdit(preedit = preedit, preeditCursorBegin = pb, preeditCursorEnd = pe))
        shown = preedit
        shownBegin = pb
        shownEnd = pe
    }

    /** Keys move the client's cursor: they'd act on a preedit the client shows, so it goes first. */
    private fun clearShown(out: MutableList<Op>) {
        if (shown.isEmpty()) return
        out += Op.Edit(ImeEdit())
        shown = ""
    }

    private fun moveTo(out: MutableList<Op>, to: Int, mods: Int = 0) {
        if (mods == 0 && baseAnchor != baseCursor) {
            // An arrow collapses a selection to its side first.
            val lo = min(baseAnchor, baseCursor)
            val hi = max(baseAnchor, baseCursor)
            out += Op.Key(if (to <= lo) KEY_LEFT else KEY_RIGHT)
            baseCursor = if (to <= lo) lo else hi
            baseAnchor = baseCursor
        }
        if (to > baseCursor) {
            out += Op.Key(KEY_RIGHT, codePoints(base, baseCursor, to), mods)
        } else if (to < baseCursor) {
            out += Op.Key(KEY_LEFT, codePoints(base, to, baseCursor), mods)
        }
        baseCursor = to
        if (mods == 0) baseAnchor = to
    }

    /**
     * The part of the client's text that differs from [t], as [start, end)
     * in the client's text: the narrowest one, or the one around the
     * client's cursor when that takes fewer keys and characters to send.
     */
    private fun region(t: String, newCursor: Int): Pair<Int, Int> {
        val free = region(t, base.length, base.length)
        val near = region(t, baseCursor, base.length - baseCursor)
        fun cost(r: Pair<Int, Int>): Int {
            val (p, e) = r
            val inserted = t.length - (base.length - e) - p
            return (e - p) + inserted + abs(baseCursor - baseCursor.coerceIn(p, e)) + abs(newCursor - (p + inserted))
        }
        return if (cost(near) <= cost(free)) near else free
    }

    /** The differing part with at most [prefix] equal characters kept before it and [suffix] after. */
    private fun region(t: String, prefix: Int, suffix: Int): Pair<Int, Int> {
        val most = min(base.length, t.length)
        var p = 0
        while (p < min(most, prefix) && base[p] == t[p]) p++
        if (p > 0 && Character.isHighSurrogate(base[p - 1])) p--
        var s = 0
        while (s < min(most - p, suffix) && base[base.length - 1 - s] == t[t.length - 1 - s]) s++
        if (s > 0 && Character.isLowSurrogate(base[base.length - s])) s--
        return p to base.length - s
    }

    /** The preedit's cursor in bytes: the selection's, where it is within the composing text, else its end. */
    private fun preeditCursor(preedit: String): Pair<Int, Int> {
        if (!composing || selectionStart < composeStart || selectionEnd > composeEnd) {
            val end = Utf8.length(preedit)
            return end to end
        }
        return Utf8.length(preedit, 0, selectionStart - composeStart) to Utf8.length(preedit, 0, selectionEnd - composeStart)
    }

    // --- From the client ---

    enum class Agreement { AGREES, CAUGHT_UP, AHEAD, DISAGREES }

    /**
     * A `text-input` of this session. CAUGHT_UP: it agrees, which releases
     * the [held] edit: flush. AHEAD: it doesn't reflect every edit sent yet.
     * DISAGREES: it does, but its text or selection isn't the copy's, which
     * [resync] fixes; a client often commits just before an edit arrives,
     * or between the keys it was sent, so wait for another state before
     * calling it.
     */
    fun client(s: TextInputState): Agreement {
        if (target != Target.CLIENT || s.text == null) return Agreement.AGREES
        if (s.applied < lastSerial) return Agreement.AHEAD
        if (s.text == base && s.cursor == baseCursor && s.anchor == baseAnchor) {
            disagreeing = null
            if (!held) return Agreement.AGREES
            held = false
            return Agreement.CAUGHT_UP
        }
        disagreeing = s
        return Agreement.DISAGREES
    }

    /**
     * Takes the client's last disagreeing state as the text, dropping the
     * composing text (its preedit too: the client moved on). Null when
     * there's nothing to fix; else the ops to send and whether the text
     * itself changed, which the IME must hear of. A [held] edit is dropped.
     */
    fun resync(): Pair<List<Op>, Boolean>? {
        val s = disagreeing ?: return null
        disagreeing = null
        held = false
        val out = ArrayList<Op>()
        clearShown(out)
        val t = s.text.orEmpty()
        val changed = t != (if (composing) text.removeRange(composeStart, composeEnd) else text)
        text = t
        cursor = s.cursor
        anchor = s.anchor
        clearComposing()
        base = t
        baseCursor = s.cursor
        baseAnchor = s.anchor
        return out to changed
    }

    // --- Text and positions ---

    /** The composing region, else the selection: what an insertion replaces. */
    private fun replaced(): Pair<Int, Int> = if (composing) composeStart to composeEnd else selectionStart to selectionEnd

    private fun replace(a: Int, b: Int, t: String) {
        text = text.substring(0, a) + t + text.substring(b)
        fun map(p: Int): Int = when {
            p <= a -> p
            p >= b -> p - (b - a) + t.length
            else -> a
        }
        anchor = map(anchor)
        cursor = map(cursor)
        if (composing) {
            val cs = map(composeStart)
            val ce = map(composeEnd)
            if (cs < ce) setComposing(cs, ce) else clearComposing()
        }
    }

    /** Android's newCursorPosition: past the inserted text's end when positive, else from its start. */
    private fun place(start: Int, end: Int, newCursorPosition: Int) {
        val c = if (newCursorPosition > 0) end + newCursorPosition - 1 else start + newCursorPosition
        cursor = boundary(c.coerceIn(0, text.length))
        anchor = cursor
    }

    private fun setComposing(a: Int, b: Int) {
        composeStart = a
        composeEnd = b
    }

    private fun clearComposing() {
        composeStart = -1
        composeEnd = -1
    }

    /** [i] moved back off the middle of a surrogate pair. */
    private fun boundary(i: Int): Int =
        if (i in 1 until text.length && Character.isLowSurrogate(text[i]) && Character.isHighSurrogate(text[i - 1])) i - 1 else i

    /** [i] moved forward off the middle of a surrogate pair. */
    private fun boundaryUp(i: Int): Int =
        if (i in 1 until text.length && Character.isLowSurrogate(text[i]) && Character.isHighSurrogate(text[i - 1])) i + 1 else i

    private fun offsetByCodePoints(i: Int, n: Int): Int {
        var p = i
        var left = n
        while (left > 0 && p < text.length) {
            p += if (Character.isHighSurrogate(text[p]) && p + 1 < text.length && Character.isLowSurrogate(text[p + 1])) 2 else 1
            left--
        }
        while (left < 0 && p > 0) {
            p -= if (p >= 2 && Character.isLowSurrogate(text[p - 1]) && Character.isHighSurrogate(text[p - 2])) 2 else 1
            left++
        }
        return p
    }

    private fun codePoints(s: String, from: Int, to: Int): Int = if (to <= from) 0 else Character.codePointCount(s, from, to)

    companion object {
        // Linux evdev codes.
        const val KEY_BACKSPACE = 14
        const val KEY_ENTER = 28
        const val KEY_LEFT = 105
        const val KEY_RIGHT = 106
        const val KEY_DELETE = 111
    }
}
