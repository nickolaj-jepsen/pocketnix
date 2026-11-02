package dev.nickolaj.pocketnix.gui

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class GuestRamTest {
    private val mib = 1L shl 20

    // A Pixel 8 Pro reports 11,470,912 KiB of RAM.
    private val phone = 11_470_912L shl 10

    @Test
    fun aFourGibVmOnAnElevenGbPhoneStaysOnCrosvmsRam() {
        assertFalse(GuestRam.fits(4096 * mib, phone))
    }

    @Test
    fun aTwoGibVmFitsInGuestRam() {
        assertTrue(GuestRam.fits(2048 * mib, phone))
    }
}
