package dev.nickolaj.pocketnix

import org.junit.Assert.assertFalse
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
}
