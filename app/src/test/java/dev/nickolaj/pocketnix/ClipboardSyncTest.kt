package dev.nickolaj.pocketnix

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ClipboardSyncTest {
    private fun text(s: String, at: Long) = ClipSnapshot(s, listOf("text/plain"), at)
    private fun png(at: Long) = ClipSnapshot("", listOf("image/png"), at)

    @Test
    fun theSameTextAndTypesAreSkipped() {
        val sync = ClipboardSync().sent(text("b", 1))
        assertFalse(sync.isNew(text("b", 1)))
        assertFalse(sync.isNew(text("b", 2)))
        assertTrue(sync.isNew(text("c", 2)))
        assertTrue(sync.isNew(ClipSnapshot("b", listOf("text/plain", "text/html"), 2)))
    }

    @Test
    fun afterAGuestCopyTheSameAndroidTextIsSentAgain() {
        val sync = ClipboardSync().sent(text("b", 1)).guestCopied("c")
        assertTrue(sync.isNew(text("b", 3)))
        // The guest's copy itself, read back on focus, isn't.
        assertFalse(sync.isNew(text("c", 2)))
    }

    @Test
    fun afterATypedGuestCopyTheSameAndroidTextIsSentAgain() {
        assertTrue(ClipboardSync().sent(text("b", 1)).guestCopied(null).isNew(text("b", 3)))
    }

    @Test
    fun aGuestsEarlierTextIsSentOnceAndroidHasMovedOn() {
        assertTrue(ClipboardSync().guestCopied("c").sent(text("b", 2)).isNew(text("c", 3)))
    }

    @Test
    fun twoImagesOfOneTypeAreBothSent() {
        val sync = ClipboardSync().sent(png(1))
        assertTrue(sync.isNew(png(2)))
        assertFalse(sync.isNew(png(1)))
    }

    @Test
    fun aSensitiveGuestCopyParses() {
        assertEquals(GuestCopy("pw", true), GuestCopy.parse(JSONObject("""{"type":"guest-clipboard","text":"pw","sensitive":true}""")))
        assertEquals(GuestCopy("x", false), GuestCopy.parse(JSONObject("""{"type":"guest-clipboard","text":"x"}""")))
        assertEquals(GuestCopy(null, true), GuestCopy.parse(JSONObject("""{"type":"guest-clipboard-data","ref":"r1","mime":"image/png","size":4,"sensitive":true}""")))
        assertNull(GuestCopy.parse(JSONObject("""{"type":"notify","title":"t"}""")))
    }

    @Test
    fun aGuestCopyDuringASendLeavesTheNextFocusSending() {
        val a = ClipSnapshot("from android", listOf("text/plain"), 1)
        val before = ClipboardSync()
        val during = before.guestCopied("from guest")
        val after = during.afterSend(before, a)
        // The agent may have applied A after the guest's copy: the guest's text goes to it again.
        assertTrue(after.isNew(ClipSnapshot("from guest", listOf("text/plain"), 2)))
        assertTrue(after.isNew(a))
    }

    @Test
    fun anUndisturbedSendIsRecorded() {
        val a = ClipSnapshot("from android", listOf("text/plain"), 1)
        val before = ClipboardSync()
        assertFalse(before.afterSend(before, a).isNew(a))
    }
}
