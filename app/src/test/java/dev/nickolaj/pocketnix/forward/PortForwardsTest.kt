package dev.nickolaj.pocketnix.forward

import dev.nickolaj.pocketnix.agent.AgentDialer
import dev.nickolaj.pocketnix.agent.Forward
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.IOException
import java.net.InetAddress
import java.net.ServerSocket

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
}
