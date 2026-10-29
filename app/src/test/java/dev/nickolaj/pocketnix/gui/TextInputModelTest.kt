package dev.nickolaj.pocketnix.gui

import dev.nickolaj.pocketnix.gui.TextInputModel.Op
import dev.nickolaj.pocketnix.gui.TextInputModel.Target
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import kotlin.math.max
import kotlin.math.min

class TextInputModelTest {
    /**
     * A guest client as text-input-v3 and its keyboard leave it: surrounding
     * text without the preedit, the cursor, and the preedit shown there.
     */
    private class Client(var text: String = "", var cursor: Int = text.length, var anchor: Int = cursor) {
        var preedit = ""
        var serial = 0

        /** As GTK and Qt take a batch: edits as they arrive, keys queued until after. */
        fun apply(ops: List<Op>) {
            ops.filterIsInstance<Op.Edit>().forEach(::apply)
            ops.filter { it !is Op.Edit }.forEach(::apply)
        }

        fun apply(op: Op) {
            when (op) {
                is Op.Edit -> {
                    serial++
                    val e = op.edit
                    preedit = ""
                    val lo = min(anchor, cursor)
                    val hi = max(anchor, cursor)
                    val start = back(lo, e.deleteBefore)
                    val end = forward(hi, e.deleteAfter)
                    val kept = text.substring(lo, hi)
                    text = text.substring(0, start) + kept + text.substring(end)
                    // A commit replaces the selection; without one, the selection stays.
                    if (e.commit.isNotEmpty()) {
                        insert(start, start + kept.length, e.commit)
                    } else {
                        val shift = lo - start
                        anchor -= shift
                        cursor -= shift
                    }
                    preedit = e.preedit
                }
                is Op.Type -> insert(min(anchor, cursor), max(anchor, cursor), op.text)
                is Op.Key -> repeat(op.count) { key(op.key, op.mods and Gui.MOD_SHIFT != 0) }
            }
        }

        private fun insert(lo: Int, hi: Int, t: String) {
            text = text.substring(0, lo) + t + text.substring(hi)
            cursor = lo + t.length
            anchor = cursor
        }

        private fun key(code: Int, shift: Boolean) {
            val lo = min(anchor, cursor)
            val hi = max(anchor, cursor)
            when (code) {
                TextInputModel.KEY_LEFT, TextInputModel.KEY_RIGHT -> {
                    val right = code == TextInputModel.KEY_RIGHT
                    if (!shift && lo != hi) {
                        cursor = if (right) hi else lo
                    } else {
                        cursor = if (right) text.offsetByCodePoints(cursor, if (cursor < text.length) 1 else 0) else text.offsetByCodePoints(cursor, if (cursor > 0) -1 else 0)
                    }
                    if (!shift) anchor = cursor
                }
                TextInputModel.KEY_BACKSPACE, TextInputModel.KEY_DELETE -> {
                    if (lo != hi) {
                        insert(lo, hi, "")
                    } else if (code == TextInputModel.KEY_BACKSPACE && cursor > 0) {
                        insert(text.offsetByCodePoints(cursor, -1), cursor, "")
                    } else if (code == TextInputModel.KEY_DELETE && cursor < text.length) {
                        insert(cursor, text.offsetByCodePoints(cursor, 1), "")
                    }
                }
            }
        }

        private fun back(from: Int, bytes: Int): Int {
            var i = from
            while (i > 0 && Utf8.length(text, i, from) < bytes) i--
            return i
        }

        private fun forward(from: Int, bytes: Int): Int {
            var i = from
            while (i < text.length && Utf8.length(text, from, i) < bytes) i++
            return i
        }

        fun state(session: Int = 1, applied: Int = serial) = TextInputState(
            1, session, applied, text, cursor, anchor, "normal", emptySet(), null,
        )
    }

    private fun session(client: Client, target: Target = Target.CLIENT) = TextInputModel().apply { reset(target, client.state()) }

    /**
     * Flushes [m] into [c], with the client's state releasing a held edit,
     * and checks the client ended up with the copy's text.
     */
    private fun flush(m: TextInputModel, c: Client): List<Op> {
        val ops = ArrayList<Op>()
        do {
            val batch = m.flush()
            c.apply(batch)
            ops += batch
            m.lastSerial = c.serial
        } while (m.held && m.client(c.state()) == TextInputModel.Agreement.CAUGHT_UP)
        assertTrue("nothing left held", !m.held)
        val committed = if (m.composing) m.text.removeRange(m.composeStart, m.composeEnd) else m.text
        if (m.target != Target.KEYS) {
            assertEquals("the client's text", committed, c.text)
            assertEquals("the client's preedit", if (m.composing) m.text.substring(m.composeStart, m.composeEnd) else "", c.preedit)
        }
        return ops
    }

    @Test
    fun composingShowsAsThePreeditAndCommitsIntoTheText() {
        val c = Client()
        val m = session(c)
        m.setComposingText("h", 1)
        assertEquals(listOf(Op.Edit(ImeEdit(preedit = "h"))), flush(m, c))
        m.setComposingText("he", 1)
        flush(m, c)
        m.setComposingText("hel", 1)
        flush(m, c)
        m.commitText("hello ", 1)
        assertEquals(listOf(Op.Edit(ImeEdit(commit = "hello "))), flush(m, c))
        assertEquals("hello ", c.text)
        assertEquals(6, c.cursor)
    }

    @Test
    fun aBatchOfEditsIsOneEdit() {
        // Gboard reverting an autocorrection: delete the word, commit another, in one batch.
        val c = Client("teh ")
        val m = session(c)
        m.deleteSurroundingText(4, 0)
        m.commitText("the ", 1)
        val ops = flush(m, c)
        assertEquals(1, ops.size)
        assertEquals("the ", c.text)
    }

    @Test
    fun deletesAreInBytesAndNeverSplitACharacter() {
        val c = Client("aø😀")
        val m = session(c)
        // One UTF-16 unit of the emoji: the whole emoji goes.
        m.deleteSurroundingText(1, 0)
        assertEquals(listOf(Op.Edit(ImeEdit(deleteBefore = 4))), flush(m, c))
        m.deleteSurroundingTextInCodePoints(1, 0)
        assertEquals(listOf(Op.Edit(ImeEdit(deleteBefore = 2))), flush(m, c))
        assertEquals("a", c.text)
    }

    @Test
    fun thePreeditCursorIsInBytesOfThePreedit() {
        val c = Client()
        val m = session(c)
        m.setComposingText("ø€", 1)
        m.setSelection(1, 1)
        val ops = flush(m, c)
        assertEquals(listOf(Op.Edit(ImeEdit(preedit = "ø€", preeditCursorBegin = 2, preeditCursorEnd = 2))), ops)
    }

    @Test
    fun recomposingAWordTakesItBackIntoThePreedit() {
        val c = Client("hello world")
        val m = session(c)
        m.setComposingRegion(6, 11)
        flush(m, c)
        assertEquals("hello ", c.text)
        assertEquals("world", c.preedit)
        m.setComposingText("words", 1)
        flush(m, c)
        m.finishComposingText()
        flush(m, c)
        assertEquals("hello words", c.text)
        assertEquals(11, c.cursor)
    }

    @Test
    fun aComposingRegionAwayFromTheCursorMovesTheCursorThere() {
        val c = Client("hello world")
        val m = session(c)
        m.setComposingRegion(0, 5)
        val ops = flush(m, c)
        assertTrue("arrows, not the text resent", ops.none { it is Op.Edit && it.edit.commit.isNotEmpty() })
        assertEquals(0, c.cursor)
        m.commitText("Hello", 1)
        flush(m, c)
        assertEquals("Hello world", c.text)
        m.setSelection(11, 11)
        flush(m, c)
        assertEquals(11, c.cursor)
    }

    @Test
    fun selectionsGoAsArrowKeys() {
        val c = Client("abc")
        val m = session(c)
        m.setSelection(1, 1)
        assertEquals(listOf(Op.Key(TextInputModel.KEY_LEFT, 2)), flush(m, c))
        m.setSelection(0, 3)
        flush(m, c)
        assertEquals(0 to 3, c.anchor to c.cursor)
        // Typing over a selection replaces it.
        m.commitText("x", 1)
        flush(m, c)
        assertEquals("x", c.text)
    }

    @Test
    fun typingOverTheClientsSelectionIsOneCommit() {
        val c = Client("foo bar", cursor = 3, anchor = 0)
        val m = session(c)
        m.commitText("x", 1)
        assertEquals(listOf(Op.Edit(ImeEdit(commit = "x"))), flush(m, c))
        assertEquals("x bar", c.text)
        // A replacement that keeps the selection's first letter still replaces all of it.
        val c2 = Client("foo bar", cursor = 0, anchor = 3)
        val m2 = session(c2)
        m2.commitText("f", 1)
        flush(m2, c2)
        assertEquals("f bar", c2.text)
    }

    @Test
    fun anEditWaitsForTheKeysBeforeIt() {
        // Gboard recomposing the word before the space just typed.
        val c = Client("helo ")
        val m = session(c)
        m.setComposingRegion(0, 4)
        m.setComposingText("hello", 1)
        val first = m.flush()
        assertTrue("keys first", first.isNotEmpty() && first.all { it is Op.Key })
        assertTrue(m.held)
        assertTrue("nothing more while held", m.flush().isEmpty())
        c.apply(first)
        assertEquals(TextInputModel.Agreement.CAUGHT_UP, m.client(c.state()))
        c.apply(m.flush())
        assertEquals(" ", c.text)
        assertEquals("hello", c.preedit)
        assertEquals(0, c.cursor)
    }

    @Test
    fun aHeldEditGoesUnheldWhenTheClientSaysNothing() {
        val c = Client("helo ")
        val m = session(c)
        m.setComposingRegion(0, 4)
        m.setComposingText("hello", 1)
        c.apply(m.flush())
        assertTrue(m.held)
        c.apply(m.flush(hold = false))
        assertTrue(!m.held)
        assertEquals(" ", c.text)
        assertEquals("hello", c.preedit)
    }

    @Test
    fun aDeleteKeyIsPredicted() {
        val c = Client("abc")
        val m = session(c)
        val op = m.deleteKey(forward = false)
        c.apply(op)
        assertEquals("ab", m.text)
        assertEquals(TextInputModel.Agreement.AGREES, m.client(c.state()))
    }

    @Test
    fun aStateBehindTheEditsSentIsIgnored() {
        val c = Client()
        val m = session(c)
        m.commitText("a", 1)
        flush(m, c)
        m.commitText("b", 1)
        flush(m, c)
        assertEquals(TextInputModel.Agreement.AHEAD, m.client(c.state().copy(applied = 1, text = "a", cursor = 1, anchor = 1)))
        assertEquals(TextInputModel.Agreement.AGREES, m.client(c.state()))
        assertEquals("ab", m.text)
    }

    @Test
    fun aClientThatChangedItsTextIsFollowedOnlyIfItStaysChanged() {
        val c = Client("abc")
        val m = session(c)
        m.commitText("d", 1)
        flush(m, c)
        // A commit that crossed the edit: it claims the serial but shows the text before it.
        assertEquals(TextInputModel.Agreement.DISAGREES, m.client(c.state().copy(text = "abc", cursor = 3, anchor = 3)))
        assertEquals(TextInputModel.Agreement.AGREES, m.client(c.state()))
        assertNull("the next state agreed: nothing to fix", m.resync())

        // The app cleared the field itself.
        c.text = ""
        c.cursor = 0
        c.anchor = 0
        assertEquals(TextInputModel.Agreement.DISAGREES, m.client(c.state()))
        val (ops, changed) = m.resync()!!
        assertTrue(ops.isEmpty())
        assertTrue(changed)
        assertEquals("", m.text)
    }

    @Test
    fun aResyncDropsTheComposingText() {
        val c = Client("abc")
        val m = session(c)
        m.setComposingText("x", 1)
        flush(m, c)
        // The user tapped elsewhere in the field.
        c.cursor = 1
        c.anchor = 1
        m.client(c.state())
        val (ops, changed) = m.resync()!!
        c.apply(ops)
        assertEquals("", c.preedit)
        assertEquals(false, changed)
        assertFalse(m.composing)
        assertEquals(1, m.cursor)
    }

    @Test
    fun withoutATextInputCommittedTextIsTyped() {
        val m = TextInputModel().apply { reset(Target.KEYS) }
        m.setComposingText("wor", 1)
        assertTrue("the composing text waits", m.flush().isEmpty())
        m.commitText("word ", 1)
        assertEquals(listOf(Op.Type("word ")), m.flush())
        m.deleteSurroundingText(1, 0)
        assertEquals(listOf(Op.Key(TextInputModel.KEY_BACKSPACE, 1)), m.flush())
        m.commitText("ø€", 1)
        assertEquals(listOf(Op.Type("ø€")), m.flush())
    }

    @Test
    fun withoutSurroundingTextCommitsAreTypedAndOnlyThePreeditIsAnEdit() {
        val c = Client()
        val m = TextInputModel().apply { reset(Target.BLIND, TextInputState(1, 1, 0, null, 0, 0, "terminal", emptySet(), null)) }
        m.setComposingText("hel", 1)
        assertEquals(listOf(Op.Edit(ImeEdit(preedit = "hel"))), flush(m, c))
        m.commitText("hello", 1)
        assertEquals(listOf(Op.Edit(ImeEdit()), Op.Type("hello")), flush(m, c))
    }

    @Test
    fun withoutSurroundingTextDeletesAreKeysAndThePreeditStepsAside() {
        val c = Client()
        val m = TextInputModel().apply { reset(Target.BLIND, TextInputState(1, 1, 0, null, 0, 0, "terminal", emptySet(), null)) }
        m.commitText("ab", 1)
        flush(m, c)
        m.setComposingText("x", 1)
        flush(m, c)
        // Android deletes before the composing text.
        m.deleteSurroundingText(1, 0)
        val ops = flush(m, c)
        assertEquals(listOf(Op.Edit(ImeEdit()), Op.Key(TextInputModel.KEY_BACKSPACE, 1), Op.Edit(ImeEdit(preedit = "x"))), ops)
        assertEquals("a", c.text)
    }

    @Test
    fun anImeCursorAfterTheInsertedTextFollowsAndroidsRule() {
        val c = Client("ab")
        val m = session(c)
        m.setSelection(1, 1)
        flush(m, c)
        // newCursorPosition 0: before the inserted text.
        m.commitText("X", 0)
        flush(m, c)
        assertEquals("aXb", c.text)
        assertEquals(1, c.cursor)
        assertEquals("a", m.textBeforeCursor(10))
        assertEquals("Xb", m.textAfterCursor(10))
    }

    @Test
    fun utf8OffsetsMapToCharacterStarts() {
        val s = "aø😀€"
        assertEquals(10, Utf8.length(s))
        assertEquals(1, Utf8.index(s, 1))
        assertEquals("the middle of ø is ø", 1, Utf8.index(s, 2))
        assertEquals(2, Utf8.index(s, 3))
        assertEquals("the middle of the emoji is the emoji", 2, Utf8.index(s, 5))
        assertEquals(4, Utf8.index(s, 7))
        assertEquals(s.length, Utf8.index(s, 99))
        assertEquals(0, Utf8.index(s, -3))
    }
}
