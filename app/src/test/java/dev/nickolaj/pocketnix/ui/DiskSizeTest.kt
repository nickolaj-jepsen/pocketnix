package dev.nickolaj.pocketnix.ui

import org.junit.Assert.assertEquals
import org.junit.Test

class DiskSizeTest {
    @Test
    fun aSizeFromTheMinimumToTheFreeSpaceFits() {
        assertEquals(DiskSize.Ok(8), checkDiskSize("8", 8, 32))
        assertEquals(DiskSize.Ok(32), checkDiskSize("32", 8, 32))
    }

    @Test
    fun aSizeOutsideTheRangeDoesNot() {
        assertEquals(DiskSize.OutOfRange, checkDiskSize("4", 8, 32))
        assertEquals(DiskSize.OutOfRange, checkDiskSize("40", 8, 32))
        assertEquals(DiskSize.OutOfRange, checkDiskSize("", 8, 32))
    }

    @Test
    fun lessFreeSpaceThanTheMinimumFitsNothing() {
        assertEquals(DiskSize.NoRoom, checkDiskSize("8", 8, 4))
        assertEquals(DiskSize.NoRoom, checkDiskSize("4", 8, 4))
    }
}
