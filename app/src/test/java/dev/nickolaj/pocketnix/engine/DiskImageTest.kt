package dev.nickolaj.pocketnix.engine

import org.junit.Assert.assertEquals
import org.junit.Test

class DiskImageTest {
    private val gib = 1L shl 30

    @Test
    fun aBalloonTakesTheWholeStorage() {
        assertEquals(256 * gib, DiskImage.growTo(256 * gib, allocated = 10 * gib, free = 20 * gib, balloon = true))
    }

    @Test
    fun withoutABalloonTheGuestGetsOnlyThePhonesFreeSpaceLessTheReserve() {
        assertEquals(29 * gib, DiskImage.growTo(256 * gib, allocated = 10 * gib, free = 20 * gib, balloon = false))
    }

    @Test
    fun withoutABalloonAFullPhoneGrowsNothing() {
        assertEquals(0L, DiskImage.growTo(256 * gib, allocated = 0, free = 512L shl 20, balloon = false))
    }
}
