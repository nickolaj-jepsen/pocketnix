package dev.nickolaj.pocketnix.gui

import org.json.JSONObject
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.ByteArrayInputStream
import java.nio.ByteBuffer

class GuiReaderTest {
    private fun read(f: CompositorFrames): List<GuiMessage> {
        val r = GuiReader(ByteArrayInputStream(f.bytes()))
        return generateSequence { r.next() }.toList()
    }

    private fun pixels(n: Int, seed: Int = 1) = ByteArray(n) { (it * 31 + seed).toByte() }

    @Test
    fun readsSessionMessages() {
        val f = CompositorFrames().hello("abc").toplevel(7, "Calc").surface(7, 7).surface(9, 7, "popup", 7)
            .json(JSONObject().put("type", "cursor").put("window", 7).put("name", "text"))
            .json(JSONObject().put("type", "toplevel-request").put("id", 7).put("request", "maximize"))
            .json(JSONObject().put("type", "surface-removed").put("id", 9))
            .json(JSONObject().put("type", "toplevel-removed").put("id", 7))
            .ready()
        val m = read(f)
        assertEquals(GuiMessage.Hello(1, "test", "abc"), m[0])
        assertEquals("Calc", (m[1] as GuiMessage.Toplevel).title)
        assertEquals(GuiMessage.Surface(7, 7, "toplevel", 0), m[2])
        assertEquals(GuiMessage.Surface(9, 7, "popup", 7), m[3])
        assertEquals(GuiMessage.Cursor(7, "text"), m[4])
        assertEquals(GuiMessage.ToplevelRequest(7, "maximize"), m[5])
        assertEquals(GuiMessage.SurfaceRemoved(9), m[6])
        assertEquals(GuiMessage.ToplevelRemoved(7), m[7])
        assertEquals(GuiMessage.Ready, m[8])
        assertEquals(9, m.size)
    }

    @Test
    fun skipsWhatALaterVersionAdds() {
        val f = CompositorFrames()
            .json(JSONObject().put("type", "apps").put("apps", "later"))
            .raw(0x03, byteArrayOf(1, 2, 3))
            .json(JSONObject().put("type", "surface").put("id", 2).put("window", 2).put("role", "toplevel").put("parent", 0).put("future", true))
            // A longer header: the rects start at headerSize.
            .commit(2, 2, 2, 1, listOf(intArrayOf(0, 0, 2, 1)), pixels(8), Gui.FLAG_BUFFER or Gui.FLAG_FULL, headerSize = 96)
        val m = read(f)
        assertEquals(GuiMessage.Surface(2, 2, "toplevel", 0), m[0])
        val c = (m[1] as GuiMessage.Group).commits.single()
        assertArrayEquals(intArrayOf(0, 0, 2, 1), c.rects)
        assertArrayEquals(pixels(8), c.data)
        assertEquals(2, m.size)
    }

    @Test
    fun gathersCommitDataSplitAnywhere() {
        val data = pixels(64 * 32 * 4)
        for (split in listOf(1, 7, 1000, 4096)) {
            val f = CompositorFrames().commit(5, 5, 64, 32, listOf(intArrayOf(0, 0, 64, 32)), data, Gui.FLAG_BUFFER or Gui.FLAG_FULL, split = split)
            assertArrayEquals("split $split", data, (read(f).single() as GuiMessage.Group).commits.single().data)
        }
    }

    @Test
    fun groupsCommitsUntilOneWithoutGrouped() {
        val f = CompositorFrames()
            .commit(1, 1, 1, 1, listOf(intArrayOf(0, 0, 1, 1)), pixels(4), Gui.FLAG_BUFFER or Gui.FLAG_FULL or Gui.FLAG_GROUPED)
            .commit(2, 1, 1, 1, listOf(intArrayOf(0, 0, 1, 1)), pixels(4), Gui.FLAG_BUFFER or Gui.FLAG_FULL or Gui.FLAG_GROUPED)
            .commit(3, 1, 1, 1, listOf(intArrayOf(0, 0, 1, 1)), pixels(4), Gui.FLAG_BUFFER or Gui.FLAG_FULL)
            .commit(1, 1, 1, 1, emptyList(), ByteArray(0), 0)
        val m = read(f)
        assertEquals(listOf(1, 2, 3), (m[0] as GuiMessage.Group).commits.map { it.surface })
        assertEquals(listOf(1), (m[1] as GuiMessage.Group).commits.map { it.surface })
        assertEquals(listOf(1, 2, 3, 4), m.flatMap { (it as GuiMessage.Group).commits }.map { it.seq })
    }

    @Test
    fun readsZstdCommitsAsSent() {
        val content = pixels(16 * 16 * 4)
        val f = CompositorFrames().full(4, 4, 16, 16, content, zstd = true)
        val c = (read(f).single() as GuiMessage.Group).commits.single()
        assertEquals(Gui.ENCODING_ZSTD, c.encoding)
        val out = ByteArray(content.size)
        JavaUnpacker.unpack(c.data, out, 0, out.size)
        assertArrayEquals(content, out)
    }

    @Test
    fun endsCleanlyAtAFrameBoundary() {
        assertNull(GuiReader(ByteArrayInputStream(ByteArray(0))).next())
    }

    private fun assertViolation(f: CompositorFrames) {
        assertThrows(GuiProtocolException::class.java) { read(f) }
    }

    @Test
    fun rejectsMalformedJson() = assertViolation(CompositorFrames().raw(Gui.JSON, "{nope".toByteArray()))

    @Test
    fun rejectsJsonInsideAGroup() = assertViolation(
        CompositorFrames()
            .commit(1, 1, 1, 1, listOf(intArrayOf(0, 0, 1, 1)), pixels(4), Gui.FLAG_BUFFER or Gui.FLAG_FULL or Gui.FLAG_GROUPED)
            .ready()
            .commit(2, 1, 1, 1, listOf(intArrayOf(0, 0, 1, 1)), pixels(4), Gui.FLAG_BUFFER or Gui.FLAG_FULL),
    )

    @Test
    fun rejectsASurfaceTwiceInAGroup() = assertViolation(
        CompositorFrames()
            .commit(1, 1, 1, 1, listOf(intArrayOf(0, 0, 1, 1)), pixels(4), Gui.FLAG_BUFFER or Gui.FLAG_FULL or Gui.FLAG_GROUPED)
            .commit(1, 1, 1, 1, listOf(intArrayOf(0, 0, 1, 1)), pixels(4), Gui.FLAG_BUFFER),
    )

    @Test
    fun rejectsAFrameBetweenACommitAndItsData() {
        val f = CompositorFrames().commit(1, 1, 8, 8, listOf(intArrayOf(0, 0, 8, 8)), pixels(256), Gui.FLAG_BUFFER or Gui.FLAG_FULL, split = 100)
        // Move the ready frame in front of the commit's last COMMIT_DATA.
        val bytes = f.bytes()
        val lastData = bytes.size - (5 + 4 + 56)
        val ready = CompositorFrames().ready().bytes()
        val spliced = bytes.copyOfRange(0, lastData) + ready + bytes.copyOfRange(lastData, bytes.size)
        assertThrows(GuiProtocolException::class.java) {
            val r = GuiReader(ByteArrayInputStream(spliced))
            while (r.next() != null) Unit
        }
    }

    @Test
    fun rejectsCommitDataForAnotherCommit() {
        val f = CompositorFrames().commit(1, 1, 8, 8, listOf(intArrayOf(0, 0, 8, 8)), pixels(256), Gui.FLAG_BUFFER or Gui.FLAG_FULL, split = 200)
        val bytes = f.bytes()
        // The COMMIT_DATA's seq sits right after its 5-byte frame header.
        val dataFrame = bytes.size - (5 + 4 + 56)
        ByteBuffer.wrap(bytes).putInt(dataFrame + 5, 99)
        assertThrows(GuiProtocolException::class.java) {
            val r = GuiReader(ByteArrayInputStream(bytes))
            while (r.next() != null) Unit
        }
    }

    @Test
    fun rejectsStrayCommitData() = assertViolation(CompositorFrames().raw(Gui.COMMIT_DATA, ByteBuffer.allocate(8).putInt(1).putInt(0).array()))

    @Test
    fun rejectsRawDataOfTheWrongLength() = assertViolation(
        CompositorFrames().commit(1, 1, 2, 2, listOf(intArrayOf(0, 0, 2, 2)), pixels(12), Gui.FLAG_BUFFER or Gui.FLAG_FULL),
    )

    @Test
    fun rejectsRectsOutsideTheBuffer() = assertViolation(
        CompositorFrames().commit(1, 1, 2, 2, listOf(intArrayOf(1, 1, 2, 1)), pixels(8), Gui.FLAG_BUFFER),
    )

    @Test
    fun rejectsAFullCommitThatIsntWhole() = assertViolation(
        CompositorFrames().commit(1, 1, 2, 2, listOf(intArrayOf(0, 0, 2, 1)), pixels(8), Gui.FLAG_BUFFER or Gui.FLAG_FULL),
    )

    @Test
    fun rejectsTooManyRects() = assertViolation(
        CompositorFrames().commit(1, 1, 1, 1, List(Gui.MAX_RECTS + 1) { intArrayOf(0, 0, 1, 1) }, pixels(4 * (Gui.MAX_RECTS + 1)), Gui.FLAG_BUFFER),
    )

    @Test
    fun rejectsSeqsThatDontIncrease() = assertViolation(
        CompositorFrames()
            .commit(1, 1, 1, 1, emptyList(), ByteArray(0), 0, seq = 5)
            .commit(1, 1, 1, 1, emptyList(), ByteArray(0), 0, seq = 5),
    )

    @Test
    fun rejectsAnOversizedFrame() {
        val bytes = ByteBuffer.allocate(5).put(Gui.JSON.toByte()).putInt((1 shl 20) + 1).array()
        assertThrows(java.io.IOException::class.java) { GuiReader(ByteArrayInputStream(bytes)).next() }
    }

    @Test
    fun encodesInputAsSpecified() {
        val b = ByteBuffer.wrap(Gui.pointerAxis(7, 1.5f, 2f, Gui.AXIS_WHEEL, 2, 0f, 22.5f, 0, 120))
        assertEquals(Gui.POINTER_AXIS, b.get().toInt())
        assertEquals(30, b.int)
        assertEquals(7, b.int)
        assertEquals(1.5f, b.float)
        assertEquals(2f, b.float)
        assertEquals(0, b.get().toInt())
        assertEquals(2, b.get().toInt())
        assertEquals(0f, b.float)
        assertEquals(22.5f, b.float)
        assertEquals(0, b.int)
        assertEquals(120, b.int)
        assertTrue(!b.hasRemaining())

        val k = ByteBuffer.wrap(Gui.key(-2, 30, true))
        assertEquals(Gui.KEY, k.get().toInt())
        assertEquals(9, k.int)
        // u32 ids past 2^31 survive as their bit pattern.
        assertEquals(0xfffffffeL, k.int.toLong() and 0xffffffffL)
        assertEquals(30, k.int)
        assertEquals(1, k.get().toInt())

        assertArrayEquals(byteArrayOf(0x10, 0, 0, 0, 8, 0, 0, 0, 3, 0, 0, 0, 9), Gui.presented(3, 9))
    }

    @Test
    fun encodesJsonMessagesWithUnsignedIds() {
        val frame = Gui.window(WindowState(-1, 1080, 2000, 2.625f, focused = true, visible = true, maximized = false))
        val o = JSONObject(String(frame, 5, frame.size - 5))
        assertEquals("window", o.getString("type"))
        assertEquals(0xffffffffL, o.getLong("id"))
        assertEquals(2.625, o.getDouble("scale"), 0.0)
        assertEquals(false, o.getBoolean("fullscreen"))
    }
}
