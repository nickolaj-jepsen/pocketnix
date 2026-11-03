package dev.nickolaj.pocketnix

import dev.nickolaj.pocketnix.LimiterKills.Exit
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class LimiterKillsTest {
    private val kill = Exit(100, "MemoryLimiter:AnonSwap")
    private val update = Exit(200, "stop dev.nickolaj.pocketnix due to killDueToPackageUpdate")

    @Test
    fun aKillIsFoundAmongOtherExits() {
        assertEquals(kill, LimiterKills.newest(listOf(update, kill, Exit(50, null)), 0))
    }

    @Test
    fun aSeenKillIsntToldAgain() {
        assertNull(LimiterKills.newest(listOf(kill), 100))
    }

    @Test
    fun theNewestOfSeveralIsTold() {
        val later = Exit(300, "MemoryLimiter:AnonSwap")
        assertEquals(later, LimiterKills.newest(listOf(kill, later), 0))
    }

    @Test
    fun otherExitsArentKills() {
        assertNull(LimiterKills.newest(listOf(update, Exit(1, null)), 0))
    }
}
