package dev.nickolaj.pocketnix.engine

import com.termux.terminal.TerminalBackend
import org.junit.Assert.assertEquals
import org.junit.Test

class ConsoleLogTest {
    private fun replay(log: ConsoleLog): String {
        val got = StringBuilder()
        log.attach(object : TerminalBackend.Sink {
            override fun onOutput(data: ByteArray, offset: Int, count: Int): Boolean {
                got.append(String(data, offset, count))
                return true
            }

            override fun onExit(status: Int?, reason: String?) {}
        })
        return got.toString()
    }

    @Test
    fun keepsOnlyTheNewestOutput() {
        val log = ConsoleLog(capacity = 8)
        for (s in listOf("abc", "defg", "hij", "k")) log.append(s)
        assertEquals("defghijk", replay(log))
    }

    @Test
    fun aWriteBiggerThanTheRingKeepsItsTail() {
        val log = ConsoleLog(capacity = 8)
        log.append("xy")
        log.append("0123456789")
        assertEquals("23456789", replay(log))
    }

    @Test
    fun replaysEverythingBeforeItFills() {
        val log = ConsoleLog(capacity = 8)
        log.append("ab")
        log.append("cd")
        assertEquals("abcd", replay(log))
    }

    @Test
    fun wrapsAcrossTheEndManyTimes() {
        val log = ConsoleLog(capacity = 7)
        val all = StringBuilder()
        for (i in 0 until 100) {
            val s = "<$i>"
            log.append(s)
            all.append(s)
        }
        assertEquals(all.takeLast(7).toString(), replay(log))
    }
}
