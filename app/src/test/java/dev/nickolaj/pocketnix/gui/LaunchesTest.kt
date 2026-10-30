package dev.nickolaj.pocketnix.gui

import dev.nickolaj.pocketnix.VmPhase
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class LaunchesTest {
    // What reached the compositor, as "type id".
    private val sent = mutableListOf<String>()
    private var n = 0
    private val launches = Launches({ f ->
        val o = JSONObject(String(f, 5, f.size - 5))
        sent += "${o.getString("type")} ${o.getString("id")}"
    }) { "l${++n}" }

    private fun toplevel(id: Int, launch: String, window: Int = id) = GuiMessage.Toplevel(
        id = id, parent = if (window == id) 0 else window, title = "", appId = "foot", width = 0, height = 0,
        minWidth = 0, minHeight = 0, maxWidth = 0, maxHeight = 0, decoration = "client", window = window, launch = launch
    )

    private fun connect(instance: String = "i1") {
        launches.connected(instance)
    }

    @Test
    fun aTapWhileTheVmBootsIsSentAfterTheReplay() {
        val l = launches.start("foot")
        assertEquals(emptyList<String>(), sent)
        connect()
        assertEquals(emptyList<String>(), sent)
        launches.ready()
        assertEquals(listOf("launch l1"), sent)
        assertEquals(LaunchState.Pending, l.state.value)
        launches.started("l1")
        assertEquals(LaunchState.Started, l.state.value)
    }

    @Test
    fun aLaunchStillWaitingIsSentAgainAfterEveryReplay() {
        connect()
        launches.ready()
        launches.start("foot")
        launches.started("l1")
        launches.disconnected()
        connect()
        launches.ready()
        // A new compositor knows nothing of it: it starts afresh.
        launches.disconnected()
        connect("i2")
        launches.ready()
        assertEquals(listOf("launch l1", "launch l1", "launch l1"), sent)
    }

    @Test
    fun aReplayedWindowNamingTheLaunchEndsIt() {
        connect()
        launches.ready()
        val l = launches.start("foot")
        launches.disconnected()
        connect()
        val ended = launches.toplevel(toplevel(7, "l1"))
        launches.ready()
        assertEquals(l, ended)
        assertEquals(LaunchState.Opened(7), l.state.value)
        assertEquals(listOf("launch l1"), sent)
    }

    @Test
    fun aDialogOrAnotherLaunchsWindowDoesntEndIt() {
        connect()
        launches.ready()
        val l = launches.start("foot")
        assertNull(launches.toplevel(toplevel(8, "l1", window = 7)))
        assertNull(launches.toplevel(toplevel(9, "other")))
        assertNull(launches.toplevel(toplevel(10, "")))
        assertEquals(LaunchState.Pending, l.state.value)
    }

    @Test
    fun aLaunchEndsOnce() {
        connect()
        launches.ready()
        val activated = launches.start("firefox")
        val failed = launches.start("gone")
        assertEquals(activated, launches.activate(GuiMessage.Activate(3, "l1")))
        assertEquals(failed, launches.failed(GuiMessage.LaunchFailed("l2", LaunchState.UNKNOWN, "no such app")))
        assertNull(launches.failed(GuiMessage.LaunchFailed("l1", LaunchState.TIMEOUT, "")))
        assertNull(launches.toplevel(toplevel(4, "l2")))
        launches.started("l2")
        assertEquals(LaunchState.Activated(3), activated.state.value)
        assertEquals(LaunchState.Failed(LaunchState.UNKNOWN, "no such app"), failed.state.value)
        // Ended launches aren't sent again.
        launches.disconnected()
        connect()
        launches.ready()
        assertEquals(listOf("launch l1", "launch l2"), sent)
    }

    @Test
    fun cancellingTellsTheCompositorThatHasTheLaunch() {
        val queued = launches.start("a")
        launches.cancel(queued.id)
        connect()
        launches.ready()
        // Never sent: nothing to cancel, and it isn't sent now.
        assertEquals(emptyList<String>(), sent)

        val live = launches.start("b")
        launches.cancel(live.id)
        assertEquals(listOf("launch l2", "launch-cancel l2"), sent)
        assertEquals(LaunchState.Cancelled, live.state.value)

        // Cancelled while disconnected: told after the same compositor's replay, never to another.
        val late = launches.start("c")
        val other = launches.start("d")
        launches.disconnected()
        launches.cancel(late.id)
        launches.cancel(other.id)
        connect()
        launches.ready()
        launches.disconnected()
        connect("i2")
        launches.ready()
        assertEquals(listOf("launch l2", "launch-cancel l2", "launch l3", "launch l4", "launch-cancel l3", "launch-cancel l4"), sent)
    }

    @Test
    fun aCancelledLaunchIsntSentToANewCompositor() {
        connect()
        launches.ready()
        val l = launches.start("a")
        launches.disconnected()
        launches.cancel(l.id)
        connect("i2")
        launches.ready()
        assertEquals(listOf("launch l1"), sent)
    }

    @Test
    fun onlyEndedLaunchesAreForgotten() {
        val l = launches.start("a")
        launches.forget(l.id)
        assertEquals(l, launches[l.id])
        launches.cancel(l.id)
        launches.forget(l.id)
        assertNull(launches[l.id])
    }

    private fun status(
        vm: VmPhase = VmPhase.Running("box"),
        gui: Boolean? = true,
        ready: Boolean = true,
        launch: LaunchState = LaunchState.Pending
    ) = splashStatus("Foot", vm, gui, ready, launch)

    @Test
    fun theSplashFollowsTheBootThenTheLaunch() {
        val booting = status(vm = VmPhase.Starting, gui = null, ready = false)
        // Running, but the agent hasn't answered yet: still the boot.
        assertEquals(booting, status(vm = VmPhase.Booting, gui = null, ready = false))
        val late = status(vm = VmPhase.AgentLate)
        val connecting = status(ready = false)
        val starting = status()
        assertEquals(starting, status(launch = LaunchState.Started))
        val steps = listOf(booting, late, connecting, starting)
        assertEquals(steps.size, steps.map { it.text }.distinct().size)
        assertTrue(steps.none { it.failed })
    }

    @Test
    fun theSplashSaysWhyNothingOpened() {
        val gone = status(launch = LaunchState.Failed(LaunchState.UNKNOWN, "x"))
        assertTrue(gone.failed)
        assertFalse(gone.canRetry)
        val exec = status(launch = LaunchState.Failed(LaunchState.EXEC, "exited with status 127"))
        assertTrue(exec.failed && exec.canRetry)
        assertTrue(exec.text.contains("exited with status 127"))
        assertTrue(status(launch = LaunchState.Failed(LaunchState.TIMEOUT, "")).canRetry)
        assertTrue(status(vm = VmPhase.Stopped).failed)
        val vmFailed = status(vm = VmPhase.Failed("no disk"))
        assertTrue(vmFailed.failed && vmFailed.canRetry)
        assertTrue(vmFailed.text.contains("no disk"))
        val off = status(gui = false, ready = false)
        assertTrue(off.failed)
        assertFalse(off.canRetry)
        assertTrue(status(vm = VmPhase.Refused("too old")).failed)
        // A failure the launch reported wins over a VM that stopped since.
        assertFalse(status(vm = VmPhase.Stopped, launch = LaunchState.Failed(LaunchState.UNKNOWN, "")).canRetry)
    }
}
