/*
 * Ported from Podroid (GPL-2.0-or-later), Copyright (C) 2024-2026 Podroid contributors.
 */
package dev.nickolaj.pocketnix.engine

/** Names of VirtualMachineCallback STOP_REASON_* codes, for logs and the UI. */
object AvfReasons {
    const val STOP_REASON_KILLED = 1
    const val STOP_REASON_SHUTDOWN = 3
    const val STOP_REASON_REBOOT = 5

    private val STOP = mapOf(
        -1 to "VIRTUALIZATION_SERVICE_DIED",
        0 to "INFRASTRUCTURE_ERROR",
        1 to "KILLED",
        2 to "UNKNOWN",
        3 to "SHUTDOWN",
        4 to "START_FAILED",
        5 to "REBOOT",
        6 to "CRASH",
        7 to "PVM_FIRMWARE_PUBLIC_KEY_MISMATCH",
        8 to "PVM_FIRMWARE_INSTANCE_IMAGE_CHANGED",
        9 to "BOOTLOADER_PUBLIC_KEY_MISMATCH",
        10 to "BOOTLOADER_INSTANCE_IMAGE_CHANGED",
        16 to "HANGUP"
    )

    fun stopReason(code: Int): String = STOP[code]?.let { "$code $it" } ?: "$code"
}
