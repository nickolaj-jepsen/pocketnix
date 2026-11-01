package dev.nickolaj.pocketnix.host

/**
 * What the guest's disk may still take of the phone's storage, for the
 * agent's storage balloon (go/internal/agent's applyBalloon).
 */
object StorageBalloon {
    /** Left for the phone whatever the guest does; more than Android's default low-storage threshold. */
    const val RESERVE = 1L shl 30

    /** Reports step by this much, so a guest writing a little doesn't send a new state each read. */
    const val STEP = 64L shl 20

    /**
     * How often the phone's free space is read. Other apps' writes in between come out of [RESERVE],
     * so this is short; a read is a statfs, and only a change of a [STEP] reaches the guest.
     */
    const val READ_MS = 5_000L

    /** What the guest may take, of [free] bytes the app could write on the phone. */
    fun available(free: Long): Long = (free - RESERVE).coerceAtLeast(0) / STEP * STEP
}
