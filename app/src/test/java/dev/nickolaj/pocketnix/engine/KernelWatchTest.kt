package dev.nickolaj.pocketnix.engine

import org.junit.Assert.assertEquals
import org.junit.Test

class KernelWatchTest {
    private fun KernelWatch.feed(s: String) = s.toByteArray().let { feed(it, 0, it.size) }

    @Test
    fun bootLoaderOutputIsNotTheKernel() {
        val w = KernelWatch()
        w.feed("U-Boot 2024.04\r\nScanning for bootflows\r\n\u001b[2J\u001b[1;1HNixOS (Generation 3 NixOS 26.05, Linux Kernel 6.12.23)\r\n")
        assertEquals(false, w.started)
    }

    @Test
    fun theEfiStubMarksTheKernelEvenSplitAcrossReads() {
        val w = KernelWatch()
        w.feed("Booting NixOS\r\nEFI st")
        assertEquals(false, w.started)
        w.feed("ub: Loaded initrd from LINUX_EFI_INITRD_MEDIA_GUID device path\r\n")
        assertEquals(true, w.started)
    }
}
