package dev.nickolaj.pocketnix.ui

import dev.nickolaj.pocketnix.gui.MenuAction
import org.junit.Assert.assertEquals
import org.junit.Test

class TabMenuTest {
    private fun chip(console: Boolean = false, ended: Boolean = false, canOpen: Boolean = true, reopen: () -> Unit = {}) = tabMenu(console, ended, canOpen, rename = {}, newTab = {}, shareTranscript = {}, reopen = reopen)

    private fun List<MenuAction>.labels() = map { it.label }.toSet()

    private fun List<MenuAction>.entry(label: String) = single { it.label == label }

    @Test
    fun aRunningTabCanBeRenamedSharedOrJoinedByANewOne() {
        assertEquals(setOf("Rename…", "New tab", "Share transcript"), chip().labels())
    }

    @Test
    fun anEndedTabCanBeReopened() {
        var reopened = false
        val items = chip(ended = true, reopen = { reopened = true })
        assertEquals(setOf("Reopen", "Rename…", "New tab", "Share transcript"), items.labels())
        items.entry("Reopen").run()
        assertEquals(true, reopened)
    }

    @Test
    fun theConsoleOnlyShares() {
        assertEquals(setOf("Share transcript"), chip(console = true).labels())
    }

    @Test
    fun withoutTheAgentNothingOpens() {
        val items = chip(ended = true, canOpen = false)
        assertEquals(false, items.entry("Reopen").enabled)
        assertEquals(false, items.entry("New tab").enabled)
        assertEquals(true, items.entry("Rename…").enabled)
    }

    @Test
    fun aThrowingActionDoesNotEscape() {
        chip(ended = true, reopen = { throw IllegalStateException("boom") }).entry("Reopen").run()
    }

    private val up = MainMenuState(apps = false, windowShown = false, agentUp = true, endedTabs = true, keepScreenOn = false, running = true, ownsGuest = true, canStart = true)

    @Test
    fun reopenEndedTabsNeedsAnEndedTabAndTheAgent() {
        val ran = mutableListOf<MainItem>()
        val items = mainMenu(up) { ran += it }
        items.entry("Reopen ended tabs").run()
        assertEquals(listOf(MainItem.REOPEN_ENDED), ran)
        assertEquals(false, mainMenu(up.copy(endedTabs = false)) {}.entry("Reopen ended tabs").enabled)
        assertEquals(false, mainMenu(up.copy(agentUp = false)) {}.entry("Reopen ended tabs").enabled)
    }

    @Test
    fun theBootMenuEntryFollowsWhetherTheGuestRuns() {
        assertEquals(true, "Restart with boot menu" in mainMenu(up) {}.labels())
        val stopped = mainMenu(up.copy(running = false)) {}.labels()
        assertEquals(true, "Start with boot menu" in stopped)
        assertEquals(false, "Reboot" in stopped)
        assertEquals(false, "Start with boot menu" in mainMenu(up.copy(running = false, ownsGuest = false)) {}.labels())
    }
}
