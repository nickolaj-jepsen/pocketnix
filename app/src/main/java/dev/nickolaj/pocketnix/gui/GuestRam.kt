package dev.nickolaj.pocketnix.gui

import android.os.ParcelFileDescriptor

/**
 * The VM's RAM, in shared memory the app makes and maps: crosvm runs the
 * guest on it, and PAGES commits (gui-protocol.md) point into it, so frames
 * cross without going over the stream.
 */
object GuestRam {
    /** Guest-physical address of RAM's first byte: crosvm's `AARCH64_PHYS_MEM_START`. */
    const val BASE = 0x8000_0000L

    /**
     * Whether the VM runs on the RAM this maps ([backsVm]), so the GUI link
     * may ask for PAGES commits.
     */
    @Volatile var mapped = false
        private set

    private var fd: ParcelFileDescriptor? = null

    /**
     * Makes and maps [bytes] of guest RAM; returns the descriptor for crosvm
     * to back it with, valid until [release], or throws.
     */
    @Synchronized
    fun create(bytes: Long): ParcelFileDescriptor {
        release()
        val n = GuiNative.guestRamCreate(BASE, bytes)
        check(n >= 0) { "shared memory for guest RAM: errno ${-n}" }
        return ParcelFileDescriptor.adoptFd(n).also { fd = it }
    }

    /** The VM's config carries [create]'s descriptor as its RAM. */
    @Synchronized
    fun backsVm() {
        check(fd != null) { "no guest RAM made" }
        mapped = true
    }

    /** Unmaps guest RAM, after any copy from it ends. */
    @Synchronized
    fun release() {
        if (fd == null) return
        mapped = false
        GuiNative.guestRamRelease()
        fd?.close()
        fd = null
    }
}
