package dev.nickolaj.pocketnix.host

import org.junit.Assert.assertEquals
import org.junit.Test

class StorageBalloonTest {
    private val gib = 1L shl 30
    private val mib = 1L shl 20

    @Test
    fun keepsTheReserveForThePhone() {
        assertEquals(19 * gib, StorageBalloon.available(20 * gib))
    }

    @Test
    fun nothingWhenThePhoneIsWithinItsReserve() {
        assertEquals(0L, StorageBalloon.available(512 * mib))
    }

    @Test
    fun stepsDownSoSmallWritesDontReportAgain() {
        assertEquals(StorageBalloon.available(gib + 128 * mib), StorageBalloon.available(gib + 150 * mib))
        assertEquals(128 * mib, StorageBalloon.available(gib + 150 * mib))
    }
}
