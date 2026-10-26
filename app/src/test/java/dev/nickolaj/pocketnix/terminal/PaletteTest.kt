package dev.nickolaj.pocketnix.terminal

import dev.nickolaj.pocketnix.agent.Snippet
import org.junit.Assert.assertEquals
import org.junit.Test

class PaletteTest {
    private val snippets = listOf(Snippet("Follow the journal", "journalctl -f", false), Snippet("Disk usage", "df -h /", true))
    private val history = listOf("git status", "df -h /", "journalctl -u sshd", "ls")

    @Test
    fun snippetsComeFirstAndHistoryDoesNotRepeatThem() {
        val rows = Palette.entries(snippets, history, "")
        assertEquals(listOf("journalctl -f", "df -h /", "git status", "journalctl -u sshd", "ls"), rows.map { it.command })
        assertEquals(listOf(true, true, false, false, false), rows.map { it.snippet })
        assertEquals(true, rows[1].submit)
    }

    @Test
    fun everyWordMustMatchANameOrCommand() {
        assertEquals(listOf("journalctl -f", "journalctl -u sshd"), Palette.entries(snippets, history, "JOURNAL").map { it.command })
        assertEquals(listOf("journalctl -u sshd"), Palette.entries(snippets, history, "sshd journal").map { it.command })
        // A snippet matches by its name too.
        assertEquals(listOf("df -h /"), Palette.entries(snippets, history, "usage").map { it.command })
        assertEquals(emptyList<String>(), Palette.entries(snippets, history, "nothing").map { it.command })
    }
}
