package dev.nickolaj.pocketnix

import dev.nickolaj.pocketnix.agent.ControlChannel.Link
import dev.nickolaj.pocketnix.engine.VmState
import dev.nickolaj.pocketnix.gui.LaunchState
import dev.nickolaj.pocketnix.gui.launcherStatus
import dev.nickolaj.pocketnix.gui.splashStatus
import dev.nickolaj.pocketnix.service.notificationText
import dev.nickolaj.pocketnix.service.tileSubtitle
import dev.nickolaj.pocketnix.ui.statusText
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class VmPhaseTest {
    private fun phase(
        state: VmState = VmState.Running,
        link: Link = Link.Up("1"),
        stopping: Boolean = false,
        late: Boolean = false,
        hostname: String? = "box"
    ) = vmPhase(state, link, stopping, late, hostname)

    @Test
    fun stoppingWinsOverRunning() {
        assertEquals(VmPhase.Stopping, phase(stopping = true))
    }

    @Test
    fun aRebootWithAStaleLinkIsStarting() {
        assertEquals(VmPhase.Starting, phase(state = VmState.Starting, link = Link.Up("1")))
        assertEquals(VmPhase.Starting, phase(state = VmState.Starting, link = Link.Refused("too old")))
    }

    @Test
    fun aRunningGuestWithoutItsAgentIsBootingThenLate() {
        assertEquals(VmPhase.Booting, phase(link = Link.Down))
        assertEquals(VmPhase.AgentLate, phase(link = Link.Down, late = true))
    }

    @Test
    fun aRefusingAgentIsNeverLate() {
        assertEquals(VmPhase.Refused("too old"), phase(link = Link.Refused("too old"), late = true))
    }

    @Test
    fun anEmptyHostnameIsNone() {
        assertEquals(VmPhase.Running("box"), phase())
        assertEquals(VmPhase.Running(null), phase(hostname = ""))
        assertEquals(VmPhase.Running(null), phase(hostname = null))
    }

    @Test
    fun theNotificationAndTileSayRefusedAndLate() {
        val refused = VmPhase.Refused("too old")
        assertTrue(notificationText(refused).contains("too old"))
        assertTrue(tileSubtitle(refused).contains("too old"))
        assertNotEquals(notificationText(VmPhase.Booting), notificationText(VmPhase.AgentLate))
        assertNotEquals(tileSubtitle(VmPhase.Booting), tileSubtitle(VmPhase.AgentLate))
    }

    @Test
    fun theTileNamesTheHost() {
        assertTrue(tileSubtitle(VmPhase.Running("box")).contains("box"))
    }

    @Test
    fun everySurfaceSaysWhyTheAgentRefused() {
        val refused = VmPhase.Refused("too old")
        assertTrue(statusText(refused).contains("too old"))
        assertTrue(launcherStatus(refused, guiEnabled = true).contains("too old"))
        assertTrue(splashStatus("Foot", refused, true, true, LaunchState.Pending).text.contains("too old"))
    }
}
