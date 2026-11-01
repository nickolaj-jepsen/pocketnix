package dev.nickolaj.pocketnix.host

/**
 * What the guest's disk may still take of the phone's storage, for the
 * agent's storage balloon (go/internal/agent's applyBalloon). The phone's
 * free space is read every so often, more often the less there is.
 */
object StorageBalloon {
    /** Left for the phone whatever the guest does. */
    const val RESERVE = 1L shl 30

    /** Reports step by this much, so a guest writing a little doesn't send a new state each read. */
    const val STEP = 64L shl 20

    private const val GIB = 1L shl 30

    /** What the guest may take, of [allocatable] bytes the phone can give out. */
    fun available(allocatable: Long): Long = (allocatable - RESERVE).coerceAtLeast(0) / STEP * STEP

    /** When to read the phone's free space again, given [allocatable] now. */
    fun nextReadMs(allocatable: Long): Long = when {
        allocatable < GIB -> 5_000L
        allocatable < 5 * GIB -> 60_000L
        allocatable < 10 * GIB -> 15 * 60_000L
        else -> 60 * 60_000L
    }
}
