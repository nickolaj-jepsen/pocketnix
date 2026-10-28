package dev.nickolaj.pocketnix.gui

import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.ByteArrayInputStream

class GuiStateTest {
    private val state = GuiState(JavaUnpacker)
    private val hashes = mutableListOf<Pair<Long, Long>>()

    init {
        state.onHash = { _, _, expected, actual -> hashes += expected to actual }
    }

    /** Applies [f]'s messages; with [unpack], decompresses each group first as the app does. */
    private fun feed(f: CompositorFrames, state: GuiState = this.state, unpack: Boolean = false) {
        val r = GuiReader(ByteArrayInputStream(f.bytes()))
        while (true) {
            when (val m = r.next() ?: break) {
                is GuiMessage.Hello -> state.hello(m)
                is GuiMessage.Toplevel -> state.toplevel(m)
                is GuiMessage.Surface -> state.surface(m)
                is GuiMessage.SurfaceRemoved -> state.surfaceRemoved(m.id)
                is GuiMessage.ToplevelRemoved -> state.toplevelRemoved(m.id)
                is GuiMessage.Group -> state.apply(if (unpack) GuiMessage.Group(m.commits.map { it.unpacked(JavaUnpacker) }) else m)
                GuiMessage.Ready -> state.ready()
                else -> {}
            }
        }
    }

    /** A W × H image whose pixel (x, y) is [x, y, seed, 255]. */
    private fun image(w: Int, h: Int, seed: Int = 0) = ByteArray(w * h * 4) { i ->
        val p = i / 4
        when (i % 4) {
            0 -> (p % w).toByte()
            1 -> (p / w).toByte()
            2 -> seed.toByte()
            else -> -1
        }
    }

    private fun rows(img: ByteArray, w: Int, x: Int, y: Int, rw: Int, rh: Int): ByteArray {
        val out = ByteArray(rw * rh * 4)
        for (r in 0 until rh) System.arraycopy(img, ((y + r) * w + x) * 4, out, r * rw * 4, rw * 4)
        return out
    }

    private fun window() = CompositorFrames().hello().toplevel(1).surface(1, 1)

    @Test
    fun contentHashMatchesTheSpikesRowHash() {
        // Reference values from an independent implementation of the spec's pseudocode.
        assertEquals(0xa21b81845b306009uL.toLong(), contentHash(ByteArray(21) { it.toByte() }, 0, 21))
        assertEquals(0xcbf29ce484222325uL.toLong(), contentHash(ByteArray(0), 0, 0))
    }

    @Test
    fun fullThenRectsBuildTheContent() {
        val a = image(8, 4, 1)
        val b = image(8, 4, 2)
        val f = window().full(1, 1, 8, 4, a)
            .commit(1, 1, 8, 4, listOf(intArrayOf(2, 1, 3, 2), intArrayOf(0, 3, 8, 1)), rows(b, 8, 2, 1, 3, 2) + rows(b, 8, 0, 3, 8, 1), zstd = true)
        feed(f)
        val expected = a.copyOf()
        for (y in 1..2) System.arraycopy(b, (y * 8 + 2) * 4, expected, (y * 8 + 2) * 4, 12)
        System.arraycopy(b, 3 * 8 * 4, expected, 3 * 8 * 4, 32)
        assertArrayEquals(expected, state.surfaces.getValue(1).content.pixels)
    }

    @Test
    fun commitsUnpackedAheadApplyTheSame() {
        val a = image(8, 4, 1)
        val b = image(8, 4, 2)
        val frames = {
            window().full(1, 1, 8, 4, a, zstd = true)
                .commit(1, 1, 8, 4, listOf(intArrayOf(1, 1, 4, 2)), rows(b, 8, 1, 1, 4, 2), zstd = true)
                .commit(1, 1, 8, 4, listOf(intArrayOf(0, 3, 8, 1)), rows(b, 8, 0, 3, 8, 1), Gui.FLAG_BUFFER or Gui.FLAG_SCROLL, scroll = 1, zstd = true)
        }
        feed(frames())
        val ahead = GuiState(JavaUnpacker)
        feed(frames(), ahead, unpack = true)
        assertArrayEquals(state.surfaces.getValue(1).content.pixels, ahead.surfaces.getValue(1).content.pixels)
    }

    @Test
    fun scrollShiftsBeforeTheRects() {
        val a = image(4, 6, 1)
        val fresh = image(4, 6, 9)
        // Content moved up two rows; the bottom two are new.
        val f = window().full(1, 1, 4, 6, a)
            .commit(1, 1, 4, 6, listOf(intArrayOf(0, 4, 4, 2)), rows(fresh, 4, 0, 4, 4, 2), Gui.FLAG_BUFFER or Gui.FLAG_SCROLL, scroll = 2)
        feed(f)
        val expected = a.copyOfRange(2 * 16, 6 * 16) + fresh.copyOfRange(4 * 16, 6 * 16)
        assertArrayEquals(expected, state.surfaces.getValue(1).content.pixels)

        // And down by one, the top row new.
        val g = CompositorFrames().commit(1, 1, 4, 6, listOf(intArrayOf(0, 0, 4, 1)), rows(fresh, 4, 0, 0, 4, 1), Gui.FLAG_BUFFER or Gui.FLAG_SCROLL, scroll = -1, seq = 3)
        feed(g)
        assertArrayEquals(fresh.copyOfRange(0, 16) + expected.copyOfRange(0, 5 * 16), state.surfaces.getValue(1).content.pixels)
    }

    @Test
    fun checksHashesWhenSent() {
        val a = image(5, 3)
        val f = window().commit(1, 1, 5, 3, listOf(intArrayOf(0, 0, 5, 3)), a, Gui.FLAG_BUFFER or Gui.FLAG_FULL, hash = contentHash(a, 0, a.size))
        feed(f)
        assertEquals(1, hashes.size)
        assertEquals(hashes[0].first, hashes[0].second)
    }

    @Test
    fun aPartialCommitNeedsMatchingContent() {
        assertThrows(GuiProtocolException::class.java) {
            feed(window().commit(1, 1, 2, 2, listOf(intArrayOf(0, 0, 1, 1)), ByteArray(4), Gui.FLAG_BUFFER))
        }
        assertThrows(GuiProtocolException::class.java) {
            feed(
                window().full(1, 1, 2, 2, ByteArray(16))
                    .commit(1, 1, 3, 2, listOf(intArrayOf(0, 0, 1, 1)), ByteArray(4), Gui.FLAG_BUFFER),
            )
        }
    }

    @Test
    fun aCommitForAnUnknownSurfaceIsAViolation() {
        assertThrows(GuiProtocolException::class.java) { feed(window().full(2, 1, 1, 1, ByteArray(4))) }
    }

    @Test
    fun zstdThatDoesntDecompressToTheRectsIsAViolation() {
        val f = window()
            .commit(1, 1, 2, 2, listOf(intArrayOf(0, 0, 2, 2)), ByteArray(12), Gui.FLAG_BUFFER or Gui.FLAG_FULL, zstd = true)
        assertThrows(GuiProtocolException::class.java) { feed(f) }
    }

    @Test
    fun concatenatedZstdFramesAreOneStream() {
        val a = image(16, 8)
        val packed = CompositorFrames.compress(a.copyOfRange(0, 200)) + CompositorFrames.compress(a.copyOfRange(200, a.size))
        feed(window().commit(1, 1, 16, 8, listOf(intArrayOf(0, 0, 16, 8)), a, Gui.FLAG_BUFFER or Gui.FLAG_FULL, zstd = true, encoded = packed))
        assertArrayEquals(a, state.surfaces.getValue(1).content.pixels)
    }

    @Test
    fun hiddenDropsTheContent() {
        feed(window().full(1, 1, 1, 1, ByteArray(4)).commit(1, 1, 0, 0, emptyList(), ByteArray(0), Gui.FLAG_HIDDEN))
        assertNull(state.surfaces.getValue(1).content.pixels)
    }

    @Test
    fun replayKeepsTheSameInstancesWindowsAndDropsTheRest() {
        feed(window().toplevel(2).surface(2, 2).surface(3, 2, "popup", 2).full(1, 1, 1, 1, ByteArray(4)).ready())
        // Reconnect: window 1 is replayed, 2 isn't; popup 3 is gone.
        val dropped = state.hello(GuiMessage.Hello(1, "test", "inst-1"))
        assertTrue(dropped.isEmpty())
        assertNull("content waits for the replay's FULL commit", state.surfaces.getValue(1).content.pixels)
        feed(CompositorFrames().toplevel(1).surface(1, 1))
        val (windows, surfaces) = state.ready()
        assertEquals(listOf(2), windows.map { it.id })
        assertEquals(setOf(2, 3), surfaces.map { it.id }.toSet())
        assertEquals(setOf(1), state.windows.keys)
    }

    @Test
    fun anotherInstanceDropsEveryWindow() {
        feed(window().ready())
        val dropped = state.hello(GuiMessage.Hello(1, "test", "inst-2"))
        assertEquals(listOf(1), dropped.map { it.id })
        assertTrue(state.windows.isEmpty() && state.surfaces.isEmpty())
    }

    /** Window 1 with dialog 2 (and its popup 3) drawn in it. */
    private fun withDialog() = window().toplevel(2, window = 1, parent = 1).surface(2, 1, toplevel = 2)
        .surface(3, 1, "popup", parent = 2, toplevel = 2).full(2, 1, 1, 1, ByteArray(4))

    @Test
    fun aDialogIsLayersInItsParentsWindow() {
        feed(withDialog().ready())
        assertEquals(setOf(1), state.windows.keys)
        assertEquals(setOf(2), state.dialogs.keys)
        assertEquals(setOf(1), state.surfaces.values.map { it.window }.toSet())
        assertTrue(state.surfaces.getValue(2).mapped)
    }

    @Test
    fun removingADialogDropsOnlyItsTree() {
        feed(withDialog().json(org.json.JSONObject().put("type", "toplevel-removed").put("id", 2)))
        assertEquals(setOf(1), state.windows.keys)
        assertTrue(state.dialogs.isEmpty())
        assertEquals(setOf(1), state.surfaces.keys)
    }

    @Test
    fun removingAWindowDropsItsDialogs() {
        feed(withDialog().json(org.json.JSONObject().put("type", "toplevel-removed").put("id", 1)))
        assertTrue(state.windows.isEmpty() && state.dialogs.isEmpty() && state.surfaces.isEmpty())
    }

    @Test
    fun aReplayMayFindADialogBecomeAWindow() {
        feed(withDialog().ready())
        state.hello(GuiMessage.Hello(2, "test", "inst-1"))
        // Its parent closed while the app was away.
        feed(CompositorFrames().toplevel(2).surface(2, 2))
        val (windows, _) = state.ready()
        assertEquals(listOf(1), windows.map { it.id })
        assertEquals(setOf(2), state.windows.keys)
        assertTrue(state.dialogs.isEmpty())
        assertEquals(2, state.surfaces.getValue(2).window)
    }

    @Test
    fun aLiveWindowAnnouncedAsADialogIsAViolation() {
        assertThrows(GuiProtocolException::class.java) { feed(window().toplevel(1, window = 3, parent = 3)) }
    }

    @Test
    fun damageHistoryCoversEveryVersionABufferMissed() {
        val h = DamageHistory(capacity = 4)
        h.record(null)
        h.record(intArrayOf(1, 1, 2, 2))
        h.record(intArrayOf(5, 5, 1, 1))
        assertArrayEquals(intArrayOf(), h.since(3))
        assertArrayEquals(intArrayOf(5, 5, 1, 1), h.since(2))
        assertArrayEquals(intArrayOf(1, 1, 2, 2, 5, 5, 1, 1), h.since(1))
        assertNull("a never-filled buffer gets everything", h.since(0))
        h.record(intArrayOf(0, 0, 1, 1))
        h.record(intArrayOf(0, 0, 1, 1))
        assertNull("older than the history reaches", h.since(1))
        h.record(null)
        assertNull("a whole-buffer change in between", h.since(5))
    }
}
