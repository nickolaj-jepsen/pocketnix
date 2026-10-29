package dev.nickolaj.pocketnix.forward

import dev.nickolaj.pocketnix.agent.AgentDialer
import dev.nickolaj.pocketnix.agent.Forward
import java.io.IOException
import java.net.DatagramSocket
import java.net.InetAddress
import java.net.ServerSocket
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class PortForwardsTest {
    private val unreachable = AgentDialer { throw IOException("no agent in this test") }

    @Test
    fun reportsAPortAnotherAppHolds() {
        val taken = ServerSocket(0, 1, InetAddress.getByName("127.0.0.1"))
        val forwards = PortForwards("test")
        val f = Forward("ssh", 22, taken.localPort, lan = false)
        try {
            val errors = forwards.apply(listOf(f), unreachable)
            assertEquals(1, errors.size)
            assertTrue(errors.single(), errors.single().contains("ssh"))
        } finally {
            taken.close()
        }
        // Once the port is free, the next apply listens and clears the error.
        assertEquals(emptyList<String>(), forwards.apply(listOf(f), unreachable))
        assertEquals(emptyList<String>(), forwards.apply(emptyList(), null))
    }

    @Test
    fun udpAndTcpForwardsShareAPortNumber() {
        val takenUdp = DatagramSocket(0, InetAddress.getByName("127.0.0.1"))
        val port = takenUdp.localPort
        val forwards = PortForwards("test")
        val tcp = Forward("ssh", 22, port, lan = false)
        val udp = Forward("mosh", 60001, port, lan = false, udp = true)
        try {
            // Another app's UDP socket blocks only the UDP forward.
            val errors = forwards.apply(listOf(tcp, udp), unreachable)
            assertEquals(1, errors.size)
            assertTrue(errors.single(), errors.single().contains("mosh"))
        } finally {
            takenUdp.close()
        }
        assertEquals(emptyList<String>(), forwards.apply(listOf(tcp, udp), unreachable))
        assertEquals(emptyList<String>(), forwards.apply(emptyList(), null))
    }
}
