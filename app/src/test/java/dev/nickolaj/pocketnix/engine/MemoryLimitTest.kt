package dev.nickolaj.pocketnix.engine

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class MemoryLimitTest {
    // A Pixel 8 Pro's /vendor/etc/memory-limiter-config.xml.
    private val pixel = """
        <?xml version="1.0" encoding="UTF-8"?>
        <!-- The values here are chosen to be applicable to 12GB and 16GB devices. -->
        <MemoryLimiterConfig>
          <version>1</version>
          <configList>
            <limitSet>
              <!-- Limits for a phone with at least 14G of ram: 8G/4G/4G/4G -->
              <minimumRequiredMemTotal>14336</minimumRequiredMemTotal>
              <memVisible>8192</memVisible>
              <memNotVisible>4096</memNotVisible>
              <swapVisible>4096</swapVisible>
              <swapNotVisible>4096</swapNotVisible>
            </limitSet>
            <limitSet>
              <!-- Limits for a phone with at least 10G of ram: 6G/3G/3G/3G -->
              <minimumRequiredMemTotal>10240</minimumRequiredMemTotal>
              <memVisible>6144</memVisible>
              <memNotVisible>3072</memNotVisible>
              <swapVisible>3072</swapVisible>
              <swapNotVisible>3072</swapNotVisible>
            </limitSet>
          </configList>
        </MemoryLimiterConfig>
    """.trimIndent()

    // A Pixel 8 Pro reports 11,470,912 KiB of RAM.
    private val pixelMib = 11_470_912L shr 10

    @Test
    fun thePhonesRamPicksItsSet() {
        assertEquals(MemoryLimit(3072, 3072), MemoryLimit.parse(pixel, pixelMib))
        assertEquals(MemoryLimit(4096, 4096), MemoryLimit.parse(pixel, 16_000))
    }

    @Test
    fun aSmallPhoneHasNoLimit() {
        assertNull(MemoryLimit.parse(pixel, 8_000))
    }

    @Test
    fun aConfigWithoutASetHasNoLimit() {
        assertNull(MemoryLimit.parse("<MemoryLimiterConfig/>", pixelMib))
        assertNull(MemoryLimit.parse("<limitSet><minimumRequiredMemTotal>1</minimumRequiredMemTotal></limitSet>", pixelMib))
    }

    @Test
    fun aVmThatFitsGetsNoWarning() {
        val limit = MemoryLimit(3072, 3072)
        assertNull(limit.warning(2048))
        assertNull(limit.warning(2800))
    }

    @Test
    fun aVmOverTheLimitLessOverheadIsWarnedOf() {
        val limit = MemoryLimit(3072, 3072)
        assertNotNull(limit.warning(3000))
        val warning = limit.warning(4096)!!
        assertTrue(warning, warning.contains("3072") && warning.contains("6144"))
    }
}
