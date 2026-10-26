package dev.nickolaj.pocketnix.terminal

import dev.nickolaj.pocketnix.terminal.TabAlerts.Companion.shouldNotify
import org.junit.Assert.assertEquals
import org.junit.Test

class TabAlertsTest {
    @Test
    fun notificationsPostUnlessTheirTabIsInView() {
        assertEquals(false, shouldNotify(watched = true, appVisible = true, bell = false, sinceLastBellMs = null))
        assertEquals(true, shouldNotify(watched = false, appVisible = true, bell = false, sinceLastBellMs = null))
        assertEquals(true, shouldNotify(watched = false, appVisible = false, bell = false, sinceLastBellMs = null))
    }

    @Test
    fun bellsPostOnlyFromTheBackgroundAndNotTooOften() {
        assertEquals(false, shouldNotify(watched = false, appVisible = true, bell = true, sinceLastBellMs = null))
        assertEquals(true, shouldNotify(watched = false, appVisible = false, bell = true, sinceLastBellMs = null))
        assertEquals(false, shouldNotify(watched = false, appVisible = false, bell = true, sinceLastBellMs = 5_000))
        assertEquals(true, shouldNotify(watched = false, appVisible = false, bell = true, sinceLastBellMs = 60_000))
    }
}
