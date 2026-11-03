package dev.nickolaj.pocketnix

import android.os.SharedMemory
import java.nio.ByteBuffer
import kotlin.random.Random

/**
 * A debug build's unpinned shared memory in the app's own cgroup, beside the VM.
 * Reclaim can take these pages and not, it seems, a guest's (pKVM pins them), so
 * comparing the two under one memory limit tests that explanation of the freeze.
 */
internal object MemoryHog {
    private const val SEGMENT_MIB = 1024
    private const val CHUNK = 1 shl 20

    private val held = ArrayList<Pair<SharedMemory, ByteBuffer>>()

    /** Holds [mib] of touched memory in place of what it held: incompressible, as zram drops same-filled pages. */
    @Synchronized
    fun hold(mib: Int) {
        free()
        val noise = Random.nextBytes(CHUNK)
        var left = mib
        var counter = 0L
        while (left > 0) {
            val size = minOf(left, SEGMENT_MIB)
            val memory = SharedMemory.create("memory-hog", size shl 20)
            val buffer = memory.mapReadWrite()
            held += memory to buffer
            while (buffer.remaining() >= CHUNK) {
                // Each MiB differs, so no two are the same to a compressor either.
                buffer.put(noise)
                buffer.putLong(buffer.position() - CHUNK, counter++)
            }
            left -= size
        }
    }

    /** Lets go of what it held. */
    @Synchronized
    fun free() {
        for ((memory, buffer) in held) {
            SharedMemory.unmap(buffer)
            memory.close()
        }
        held.clear()
    }
}
