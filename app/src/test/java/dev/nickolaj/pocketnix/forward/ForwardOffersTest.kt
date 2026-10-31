package dev.nickolaj.pocketnix.forward

import dev.nickolaj.pocketnix.agent.Forward
import dev.nickolaj.pocketnix.forward.ForwardOffers.State
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ForwardOffersTest {
    private val none = emptyList<Forward>()

    private fun ForwardOffers.see(vararg ports: Int, now: Long = 0, lan: Boolean = false, module: List<Forward> = none) = observe(ports.toList(), lan, module, now)

    private fun ForwardOffers.stateOf(port: Int) = offers[port]?.state

    @Test
    fun aNewPortIsOfferedOnce() {
        val first = ForwardOffers().see(8765)
        assertEquals(listOf(8765), first.noticesSince(ForwardOffers()).post.map { it.port })
        val again = first.see(8765, 3000, now = 1_000)
        assertEquals(listOf(3000), again.noticesSince(first).post.map { it.port })
    }

    @Test
    fun anOfferedPortThatLeavesIsWithdrawn() {
        val offered = ForwardOffers().see(8765)
        val gone = offered.see()
        assertNull(gone.stateOf(8765))
        assertEquals(listOf(8765), gone.noticesSince(offered).cancel)
    }

    @Test
    fun aDismissedPortIsNotOfferedAgainUntilABootReset() {
        val dismissed = ForwardOffers().see(8765).dismiss(8765)
        val back = dismissed.see().see(8765)
        assertEquals(State.DISMISSED, back.stateOf(8765))
        assertTrue(back.noticesSince(dismissed).post.isEmpty())
        // The app starts from an empty value when the guest boots.
        val rebooted = ForwardOffers().see(8765)
        assertEquals(State.OFFERED, rebooted.stateOf(8765))
    }

    @Test
    fun anAllowedPortKeepsItsForwardThroughTheGracePeriod() {
        val allowed = ForwardOffers().see(8765, now = 0).allow(8765, 8766)
        assertEquals(listOf(Forward("offered 8765", 8765, 8766, lan = false)), allowed.forwards(none))

        val gone = allowed.see(now = 1_000)
        assertEquals(1_000 + ForwardOffers.GRACE_MS, gone.nextExpiry())
        val within = gone.expire(1_000 + ForwardOffers.GRACE_MS - 1)
        assertEquals(State.ALLOWED, within.stateOf(8765))
        assertEquals(listOf(8765), within.forwards(none).map { it.guestPort })

        // Back within the grace period: kept, not offered again.
        val back = within.see(8765, now = 20_000)
        assertEquals(State.ALLOWED, back.stateOf(8765))
        assertNull(back.nextExpiry())
        assertTrue(back.noticesSince(within).post.isEmpty())

        val closed = gone.expire(1_000 + ForwardOffers.GRACE_MS)
        assertNull(closed.stateOf(8765))
        assertTrue(closed.forwards(none).isEmpty())
    }

    @Test
    fun anAllowedPortThatBecomesAModuleForwardIsDroppedAtOnce() {
        val allowed = ForwardOffers().see(8765).allow(8765, 8765)
        val module = listOf(Forward("web", 8765, 18765, lan = false))
        assertTrue(allowed.forwards(module).isEmpty())
        val reloaded = allowed.see(8765, module = module)
        assertNull(reloaded.stateOf(8765))
        assertNull(reloaded.nextExpiry())
    }

    @Test
    fun aBindFailureIsReportedOnceAndNotRetried() {
        val allowed = ForwardOffers().see(8765).allow(8765, 8765)
        val failed = allowed.bindFailed(8765, 8765, "taken")
        assertEquals(State.OFFERED, failed.stateOf(8765))
        assertEquals("taken", failed.offers.getValue(8765).failure)
        assertTrue(failed.forwards(none).isEmpty())
        assertEquals(listOf(8765), failed.noticesSince(allowed).post.map { it.port })
        // The next snapshot neither re-posts it nor brings its forward back.
        val next = failed.see(8765, now = 1_000)
        assertTrue(next.noticesSince(failed).post.isEmpty())
        assertTrue(next.forwards(none).isEmpty())
        // A late report for a phone port it no longer has changes nothing.
        val reallowed = next.allow(8765, 8770)
        assertEquals(reallowed, reallowed.bindFailed(8765, 8765, "taken"))
    }

    @Test
    fun aSessionForwardBindsLoopbackUnlessLan() {
        val local = ForwardOffers().see(8765).allow(8765, 8765).forwards(none).single()
        assertFalse(local.lan)
        assertEquals("127.0.0.1", bindAddress(local.lan).hostAddress)
        val lan = ForwardOffers().see(8765, lan = true).allow(8765, 8765).forwards(none).single()
        assertTrue(lan.lan)
    }

    @Test
    fun takenPhonePortsAreModuleTcpAndAllowedOnes() {
        val module = listOf(Forward("web", 80, 8080, lan = false), Forward("dns", 53, 5353, lan = false, udp = true))
        val o = ForwardOffers().see(8765, 3000).allow(8765, 8766)
        assertEquals(setOf(8080, 8766), o.takenPhonePorts(module))
    }

    @Test
    fun describesEachKnownPort() {
        val o = ForwardOffers().see(3000, 8765, 9000).allow(8765, 8766).dismiss(9000)
        assertEquals(
            listOf("port=3000 state=offered phone=-", "port=8765 state=allowed phone=8766", "port=9000 state=dismissed phone=-"),
            o.describe()
        )
    }

    @Test
    fun anAllowLosesAPhonePortAnotherAllowTook() {
        val o = ForwardOffers().observe(listOf(8000, 8001), lan = false, module = emptyList(), now = 0)
        val first = o.allow(8000, 8001)
        assertNull(first.allowUnlessTaken(8001, 8001, module = emptyList()))
        assertEquals(State.ALLOWED, first.allowUnlessTaken(8001, 8002, module = emptyList())!!.offers[8001]!!.state)
    }
}
