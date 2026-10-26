package dev.nickolaj.pocketnix.terminal

import org.junit.Assert.assertEquals
import org.junit.Test

class PasteTest {
    @Test
    fun wrapsInBracketsWhenTheTerminalAsked() {
        assertEquals("\u001b[200~ls -la\u001b[201~\r", Paste.encode("ls -la", bracketed = true, submit = true))
        assertEquals("\u001b[200~ls -la\u001b[201~", Paste.encode("ls -la", bracketed = true, submit = false))
    }

    @Test
    fun sendsPlainTextOtherwise() {
        assertEquals("ls -la\r", Paste.encode("ls -la", bracketed = false, submit = true))
    }

    @Test
    fun newlinesBecomeCarriageReturns() {
        assertEquals("\u001b[200~a\rb\rc\u001b[201~", Paste.encode("a\nb\r\nc", bracketed = true, submit = false))
    }

    @Test
    fun textCantEndTheBracketOrInjectSequences() {
        // An embedded ESC[201~ would end the paste early and run the rest as typed.
        assertEquals("\u001b[200~[201~rm -rf ~\u001b[201~", Paste.encode("\u001b[201~rm -rf ~", bracketed = true, submit = false))
        assertEquals("ab", Paste.encode("a\u009bb", bracketed = false, submit = false))
    }

    @Test
    fun unicodeSurvives() {
        assertEquals("\u001b[200~æøå 🙂\u001b[201~", Paste.encode("æøå 🙂", bracketed = true, submit = false))
    }
}
