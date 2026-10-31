package dev.nickolaj.pocketnix.forward

import dev.nickolaj.pocketnix.agent.AgentDialer
import dev.nickolaj.pocketnix.agent.Forward
import java.io.IOException
import java.net.DatagramSocket
import java.net.InetAddress
import java.net.ServerSocket
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
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

    @Test
    fun listeningLeavesOutFailedAndUdpForwards() {
        val taken = ServerSocket(0, 1, InetAddress.getByName("127.0.0.1"))
        val forwards = PortForwards("test")
        val failed = Forward("web", 8000, taken.localPort, lan = false)
        val up = Forward("ssh", 22, 0, lan = false)
        val udp = Forward("mosh", 60001, 0, lan = false, udp = true)
        try {
            forwards.apply(listOf(failed, up, udp), unreachable)
            assertEquals(listOf("ssh"), forwards.listening().map { it.name })
        } finally {
            taken.close()
            forwards.apply(emptyList(), null)
        }
    }

    // A port the system just handed out and gave back: free, wherever the test runs.
    private fun freePort() = ServerSocket(0, 1, InetAddress.getByName("127.0.0.1")).use { it.localPort }

    @Test
    fun picksTheGuestPortWhenItIsFree() {
        val p = freePort()
        assertEquals(p, pickPhonePort(p, lan = false, taken = emptySet()))
    }

    @Test
    fun picksAPortAboveAHeldOne() {
        ServerSocket(0, 1, InetAddress.getByName("127.0.0.1")).use { held ->
            val picked = pickPhonePort(held.localPort, lan = false, taken = emptySet())!!
            assertTrue("$picked is above ${held.localPort}", picked > held.localPort)
        }
    }

    @Test
    fun skipsAPortAModuleForwardUses() {
        val p = freePort()
        val taken = ForwardOffers().takenPhonePorts(listOf(Forward("web", 80, p, lan = false)))
        assertTrue(pickPhonePort(p, lan = false, taken = taken)!! > p)
    }

    @Test
    fun probesTheAddressTheForwardBinds() {
        // Another loopback address: in the way of 0.0.0.0, not of 127.0.0.1.
        ServerSocket(0, 1, InetAddress.getByName("127.0.0.2")).use { held ->
            val p = held.localPort
            assertEquals(p, pickPhonePort(p, lan = false, taken = emptySet()))
            assertTrue(pickPhonePort(p, lan = true, taken = emptySet())!! > p)
        }
    }

    @Test
    fun aPortTakenAfterTheProbeIsAFailureOfThatForward() {
        val forwards = PortForwards("test")
        val port = freePort()
        val f = Forward("offered 8123", 8123, port, lan = false)
        try {
            ServerSocket(port, 1, InetAddress.getByName("127.0.0.1")).use {
                forwards.apply(listOf(f), unreachable)
                assertNotNull(forwards.failure(f))
            }
        } finally {
            forwards.apply(emptyList(), null)
        }
    }
}
