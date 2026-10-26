package dev.nickolaj.pocketnix.service

import dev.nickolaj.pocketnix.agent.Wake
import dev.nickolaj.pocketnix.agent.WakeHolder
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Test

class WakeHoldTest {
    private val rsync = WakeHolder("rsync", "backup")
    private val held = Wake.parse(JSONObject("""{"type":"wake","held":true,"holders":[{"who":"rsync","why":"backup"}]}"""))

    @Test
    fun holdsOnlyWhileTheGuestAsksOverALiveStream() {
        assertEquals(listOf(rsync), wakeHoldersOf(held, linkUp = true, running = true, allowed = true))
        assertEquals(null, wakeHoldersOf(Wake(false, emptyList()), linkUp = true, running = true, allowed = true))
        assertEquals(null, wakeHoldersOf(null, linkUp = true, running = true, allowed = true))
        // A dropped stream (a hung guest), a stopped VM, or an owner who said no: it sleeps.
        assertEquals(null, wakeHoldersOf(held, linkUp = false, running = true, allowed = true))
        assertEquals(null, wakeHoldersOf(held, linkUp = true, running = false, allowed = true))
        assertEquals(null, wakeHoldersOf(held, linkUp = true, running = true, allowed = false))
    }

    @Test
    fun theNotificationNamesWhoAndWhy() {
        val text = wakeText(listOf(rsync, WakeHolder("mpv", "")))
        for (part in listOf("rsync", "backup", "mpv")) assertEquals(text, true, part in text)
    }
}
