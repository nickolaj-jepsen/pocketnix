package dev.nickolaj.pocketnix.engine

import android.util.Log
import com.termux.terminal.TerminalBackend
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/**
 * Makes systemd-boot show its menu despite `timeout 0`: that menu appears
 * only for a key pressed while it starts, so this taps space on the console
 * from the VM's start until the kernel starts or [timeoutMs] runs out. Space
 * is no entry's hotkey, and does nothing once the menu is up.
 */
class BootMenu(private val console: ConsoleLog, private val scope: CoroutineScope, private val timeoutMs: Long = 20_000) {
    fun tap() {
        val watch = KernelWatch()
        val sink = object : TerminalBackend.Sink {
            override fun onOutput(data: ByteArray, offset: Int, count: Int): Boolean {
                watch.feed(data, offset, count)
                return !watch.started
            }

            override fun onExit(status: Int?, reason: String?) {}
        }
        // Live output only: the ring still holds the previous boot's kernel lines.
        console.listen(sink)
        scope.launch(Dispatchers.IO) {
            val end = System.currentTimeMillis() + timeoutMs
            while (!watch.started && System.currentTimeMillis() < end) {
                console.input?.let {
                    runCatching {
                        it.write(SPACE)
                        it.flush()
                    }
                }
                delay(TAP_MS)
            }
            console.detach(sink)
            Log.i(TAG, if (watch.started) "the kernel started; done tapping" else "stopped tapping after ${timeoutMs / 1000}s")
        }
    }

    private companion object {
        const val TAG = "BootMenu"
        const val TAP_MS = 50L
        val SPACE = byteArrayOf(' '.code.toByte())
    }
}

/** Watches console output for the kernel taking over from the boot loader. */
class KernelWatch {
    @Volatile var started = false
        private set

    // Enough of the previous output to match a marker split across reads.
    private val tail = StringBuilder()

    fun feed(data: ByteArray, offset: Int, count: Int) {
        if (started) return
        tail.append(String(data, offset, count, Charsets.ISO_8859_1))
        if (MARKERS.any { tail.contains(it) }) started = true
        if (tail.length > KEEP) tail.delete(0, tail.length - KEEP)
    }

    private companion object {
        // The EFI stub's first line, and the kernel's banner for a stub that stays quiet.
        val MARKERS = listOf("EFI stub: ", "Booting Linux", "Linux version ")
        const val KEEP = 64
    }
}
