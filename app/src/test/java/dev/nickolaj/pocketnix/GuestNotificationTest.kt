package dev.nickolaj.pocketnix

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class GuestNotificationTest {
    @Test
    fun parsesTheAgentsNotifyEvent() {
        // As go/internal/agent sends it.
        val n = GuestNotification.parse(
            JSONObject(
                """{"type":"notify","title":"build","body":"done","notification":7,"appName":"make","urgency":"critical",
                "actions":[{"key":"default","label":"Open"},{"key":"ok","label":"OK"}]}""",
            ),
        )!!
        assertEquals(7L, n.id)
        assertEquals(listOf("ok" to "OK"), n.buttons)
        assertTrue(n.hasDefault)
        assertEquals(GuestNotifications.CHANNEL_CRITICAL, n.channel)
    }

    @Test
    fun aNotificationWithoutAnIdIsTheOldKind() {
        assertNull(GuestNotification.parse(JSONObject("""{"type":"notify","title":"t","body":"b"}""")))
    }

    @Test
    fun urgencyPicksTheChannel() {
        fun channel(urgency: String?) = GuestNotification.parse(
            JSONObject().put("notification", 1).put("body", "b").apply { if (urgency != null) put("urgency", urgency) },
        )!!.channel
        assertEquals(GuestNotifications.CHANNEL_LOW, channel("low"))
        assertEquals(GuestNotifications.CHANNEL, channel("normal"))
        assertEquals(GuestNotifications.CHANNEL, channel(null))
        assertEquals(GuestNotifications.CHANNEL_CRITICAL, channel("critical"))
    }

    @Test
    fun guestIdsMapToDistinctStableAndroidIds() {
        fun androidId(id: Long) = GuestNotification.parse(JSONObject().put("notification", id).put("body", "b"))!!.androidId
        assertEquals(androidId(5), androidId(5))
        // Past Int.MAX_VALUE, a uint32 still gets its own id.
        assertNotEquals(androidId(1), androidId(0xFFFF_FFFFL))
        assertNotEquals(androidId(0x7FFF_FFFFL), androidId(0x8000_0000L))
    }
}
