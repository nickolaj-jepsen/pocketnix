package dev.nickolaj.pocketnix.gui

import org.junit.Assert.assertEquals
import org.junit.Test

class WindowTabsTest {
    // Terminal tabs as (name, agent id); the console has none.
    private val console = "console" to ""
    private val shell = "shell" to "aaaa"
    private val build = "build" to "bbbb"

    private fun strip(terminals: List<Pair<String, String>>, vararg windows: WindowTab): List<String> = tabStrip(terminals, { it.second }, windows.toList()).map {
        when (it) {
            is TabStripItem.Terminal -> it.tab.first
            is TabStripItem.Window -> it.window.title
        }
    }

    @Test
    fun aWindowFollowsItsTerminal() {
        assertEquals(
            listOf("console", "shell", "foot", "build", "gtk"),
            strip(listOf(console, shell, build), WindowTab(1, "aaaa", "foot"), WindowTab(2, "bbbb", "gtk"))
        )
    }

    @Test
    fun windowsOfOneTerminalKeepTheirOrder() {
        assertEquals(
            listOf("shell", "second", "first", "build"),
            strip(listOf(shell, build), WindowTab(9, "aaaa", "second"), WindowTab(3, "aaaa", "first"))
        )
    }

    @Test
    fun aWindowWhoseTerminalIsGoneGoesAtTheEnd() {
        assertEquals(
            listOf("console", "shell", "orphan"),
            strip(listOf(console, shell), WindowTab(1, "cccc", "orphan"))
        )
    }

    @Test
    fun noTerminalNamesTheEmptyId() {
        // Terminals without an id yet (the console, a tab whose agent hasn't answered) take no windows.
        assertEquals(listOf("console", "late", "w"), strip(listOf(console, "late" to ""), WindowTab(1, "dddd", "w")))
    }

    @Test
    fun twoTerminalsWithOneIdShowTheWindowOnce() {
        assertEquals(listOf("a", "w", "b"), strip(listOf("a" to "aaaa", "b" to "aaaa"), WindowTab(1, "aaaa", "w")))
    }
}
