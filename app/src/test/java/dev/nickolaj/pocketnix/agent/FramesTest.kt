package dev.nickolaj.pocketnix.agent

import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.io.IOException
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Test

class FramesTest {
    @Test
    fun roundTrips() {
        val out = ByteArrayOutputStream()
        Frames.write(out, Frames.DATA, "hello".toByteArray())
        Frames.write(out, Frames.RESIZE, Frames.resize(50, 132))
        val input = ByteArrayInputStream(out.toByteArray())
        val data = Frames.read(input)!!
        assertEquals(Frames.DATA, data.type)
        assertArrayEquals("hello".toByteArray(), data.payload)
        val resize = Frames.read(input)!!
        assertEquals(Frames.RESIZE, resize.type)
        assertArrayEquals(byteArrayOf(0, 50, 0, 132.toByte(), 0, 0, 0, 0), resize.payload)
        assertNull(Frames.read(input))
    }

    @Test
    fun matchesTheGoEncoding() {
        // go/internal/protocol: [type u8][length u32 BE][payload]
        assertArrayEquals(byteArrayOf(0, 0, 0, 0, 2, 'h'.code.toByte(), 'i'.code.toByte()), Frames.encode(Frames.DATA, "hi".toByteArray()))
    }

    @Test
    fun decodesExitStatus() {
        assertEquals(3, Frames.exitStatus(byteArrayOf(0, 0, 0, 3)))
        assertEquals(-15, Frames.exitStatus(byteArrayOf(-1, -1, -1, -15)))
    }

    @Test
    fun rejectsTruncatedAndHugeFrames() {
        assertThrows(IOException::class.java) { Frames.read(ByteArrayInputStream(byteArrayOf(0, 0, 0, 0, 5, 1))) }
        assertThrows(IOException::class.java) { Frames.read(ByteArrayInputStream(byteArrayOf(0, 0x7f, -1, -1, -1))) }
    }

    @Test
    fun datagramsRoundTripAndMatchTheGoEncoding() {
        // go/internal/protocol: [length u16 BE][payload]
        assertArrayEquals(byteArrayOf(0, 2, 'h'.code.toByte(), 'i'.code.toByte()), Datagrams.encode("hi".toByteArray()))
        val big = ByteArray(Datagrams.MAX_PAYLOAD) { 7 }
        val input = ByteArrayInputStream(Datagrams.encode("hi".toByteArray()) + Datagrams.encode(ByteArray(0)) + Datagrams.encode(big))
        val buf = ByteArray(Datagrams.MAX_PAYLOAD)
        assertEquals(2, Datagrams.read(input, buf))
        assertEquals(0, Datagrams.read(input, buf))
        assertEquals(big.size, Datagrams.read(input, buf))
        assertArrayEquals(big, buf)
        assertEquals(-1, Datagrams.read(input, buf))
        assertThrows(IOException::class.java) { Datagrams.read(ByteArrayInputStream(byteArrayOf(0, 5, 1)), buf) }
    }
}
