package dev.nickolaj.pocketnix.gui

import org.json.JSONArray
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
        assertEquals(GuiMessage.Hello(2, "test", "abc"), m[0])
        assertEquals("Calc", (m[1] as GuiMessage.Toplevel).title)
        assertEquals(GuiMessage.Surface(7, 7, "toplevel", 0, toplevel = 7), m[2])
        assertEquals(GuiMessage.Surface(9, 7, "popup", 7, toplevel = 7), m[3])
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
            .json(JSONObject().put("type", "keyboard-layout").put("layout", "later"))
            .raw(0x04, byteArrayOf(1, 2, 3))
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
    fun aToplevelSaysTheScaleOfItsSizesAndWhetherItTakesTouch() {
        val t = JSONObject().put("type", "toplevel").put("id", 3).put("window", 3).put("minWidth", 1260).put("scale", 2.625).put("touch", true)
        val m = read(CompositorFrames().json(t).toplevel(4))
        val step3 = m[0] as GuiMessage.Toplevel
        assertEquals(2.625f, step3.scale, 0f)
        assertEquals(true, step3.touch)
        val step2 = m[1] as GuiMessage.Toplevel
        assertEquals("a compositor that doesn't say", 0f, step2.scale, 0f)
        assertEquals(false, step2.touch)
    }

    @Test
    fun readsTheWindowModel() {
        val app = JSONObject().put("id", "org.gnome.Calculator").put("name", "Calculator").put("genericName", "Calculator")
            .put("keywords", JSONArray(listOf("math", "sum"))).put("categories", JSONArray(listOf("Utility"))).put("icon", "k1")
            .put("noDisplay", false).put("future", 1)
            .put("actions", JSONArray(listOf(JSONObject().put("id", "new-window").put("name", "New Window").put("icon", ""))))
        val f = CompositorFrames()
            .toplevel(5, "Calc", app = "org.gnome.Calculator", launch = "l-1")
            .toplevel(6, "Open", window = 5, parent = 5)
            .surface(6, 5, toplevel = 6)
            .json(JSONObject().put("type", "apps").put("apps", JSONArray(listOf(app, JSONObject().put("id", "").put("name", "nameless")))))
            .raw(Gui.ICON, byteArrayOf(0, 48, 2, 'k'.code.toByte(), '1'.code.toByte(), 0x89.toByte(), 'P'.code.toByte()))
            .json(JSONObject().put("type", "launch-started").put("id", "l-2"))
            .json(JSONObject().put("type", "launch-failed").put("id", "l-3").put("reason", "unknown").put("message", "no such app"))
            .json(JSONObject().put("type", "activate").put("window", 5).put("launch", "l-4"))
        val m = read(f)
        val window = m[0] as GuiMessage.Toplevel
        assertEquals(listOf(5, "org.gnome.Calculator", "l-1", false), listOf(window.window, window.app, window.launch, window.isDialog))
        val dialog = m[1] as GuiMessage.Toplevel
        assertEquals(listOf(5, 5, true), listOf(dialog.window, dialog.parent, dialog.isDialog))
        assertEquals(GuiMessage.Surface(6, 5, "toplevel", 0, toplevel = 6), m[2])
        assertEquals(
            listOf(
                AppEntry(
                    "org.gnome.Calculator", "Calculator", genericName = "Calculator", keywords = listOf("math", "sum"),
                    categories = listOf("Utility"), icon = "k1", actions = listOf(AppAction("new-window", "New Window", "")),
                ),
            ),
            (m[3] as GuiMessage.Apps).apps,
        )
        val icon = m[4] as GuiMessage.Icon
        assertEquals("k1" to 48, icon.key to icon.size)
        assertArrayEquals(byteArrayOf(0x89.toByte(), 'P'.code.toByte()), icon.png)
        assertEquals(GuiMessage.LaunchStarted("l-2"), m[5])
        assertEquals(GuiMessage.LaunchFailed("l-3", "unknown", "no such app"), m[6])
        assertEquals(GuiMessage.Activate(5, "l-4"), m[7])
    }

    @Test
    fun anIconKeyNeverNamesAPath() = assertViolation(
        CompositorFrames().raw(Gui.ICON, byteArrayOf(0, 48, 5) + "../ab".toByteArray()),
    )

    @Test
    fun anIconKeyPastItsFrameIsAViolation() = assertViolation(CompositorFrames().raw(Gui.ICON, byteArrayOf(0, 48, 9, 'a'.code.toByte())))

    @Test
    fun encodesLaunchesIconRequestsAndMoves() {
        fun fields(frame: ByteArray): Map<String, Any> {
            assertEquals(Gui.JSON, frame[0].toInt())
            val o = JSONObject(String(frame, 5, frame.size - 5))
            return o.keys().asSequence().associateWith { o.get(it) }
        }
        assertEquals(
            mapOf("type" to "launch", "id" to "a-1", "app" to "foot", "action" to "new-window", "tab" to ""),
            fields(Gui.launch("a-1", "foot", "new-window", "")),
        )
        assertEquals(mapOf("type" to "launch-cancel", "id" to "a-1"), fields(Gui.launchCancel("a-1")))
        assertEquals(mapOf("type" to "icon", "icon" to "k1", "size" to 96), fields(Gui.icon("k1", 96)))
        // A u32 id past Int's range stays unsigned.
        assertEquals(mapOf("type" to "move", "id" to 4294967295L, "tab" to "0123456789abcdef"), fields(Gui.move(-1, "0123456789abcdef")))
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

    @Test
    fun readsTextInputWithOffsetsInUtf16() {
        val f = CompositorFrames()
            .json(
                JSONObject().put("type", "text-input").put("window", 7).put("session", 3).put("applied", 12)
                    // Bytes: "ø" is 2, "😀" 4; the cursor after the emoji, the anchor inside it.
                    .put("text", "ø😀x").put("cursor", 6).put("anchor", 4).put("cause", "input-method")
                    .put("purpose", "email").put("hints", JSONArray().put("spellcheck").put("multiline"))
                    .put("cursorRect", JSONObject().put("x", 10).put("y", 20).put("width", 2).put("height", 30)),
            )
            .json(
                JSONObject().put("type", "text-input").put("window", 0).put("session", 0).put("applied", 0)
                    .put("text", JSONObject.NULL).put("cursor", 0).put("anchor", 0).put("cursorRect", JSONObject.NULL),
            )
        val m = read(f)
        assertEquals(
            GuiMessage.TextInput(TextInputState(7, 3, 12, "ø😀x", 3, 1, "email", setOf("spellcheck", "multiline"), PixelRect(10, 20, 2, 30))),
            m[0],
        )
        val none = (m[1] as GuiMessage.TextInput).state
        assertEquals(false, none.active)
        assertNull(none.text)
        assertNull(none.cursorRect)
    }

    @Test
    fun encodesTouchAndTextAsSpecified() {
        val d = ByteBuffer.wrap(Gui.touchDown(7, 3, 1.5f, 2f))
        assertEquals(Gui.TOUCH_DOWN, d.get().toInt())
        assertEquals(16, d.int)
        assertEquals(7, d.int)
        assertEquals(3, d.int)
        assertEquals(1.5f, d.float)
        assertEquals(2f, d.float)
        assertArrayEquals(byteArrayOf(0x41, 0, 0, 0, 8, 0, 0, 0, 7, 0, 0, 0, 3), Gui.touchUp(7, 3))
        assertArrayEquals(byteArrayOf(0x43, 0, 0, 0, 4, 0, 0, 0, 7), Gui.touchCancel(7))
        assertArrayEquals(byteArrayOf(0x44, 0, 0, 0, 4, 0, 0, 0, 7), Gui.touchFrame(7))

        fun json(frame: ByteArray) = JSONObject(String(frame, 5, frame.size - 5))
        val ime = json(Gui.ime(4, 9, ImeEdit(deleteBefore = 2, commit = "ø", preedit = "€")))
        assertEquals("ime", ime.getString("type"))
        assertEquals(4, ime.getInt("session"))
        assertEquals(9, ime.getInt("serial"))
        assertEquals(2, ime.getInt("deleteBefore"))
        assertEquals("ø", ime.getString("commit"))
        assertEquals("€", ime.getString("preedit"))
        assertEquals("the preedit cursor defaults to its end, in bytes", 3, ime.getInt("preeditCursorBegin"))
        assertEquals(3, ime.getInt("preeditCursorEnd"))
        val text = json(Gui.text(-1, "ø\n"))
        assertEquals(0xffffffffL, text.getLong("window"))
        assertEquals("ø\n", text.getString("text"))
        val layout = json(Gui.keyboardLayout("dk", ""))
        assertEquals("keyboard-layout", layout.getString("type"))
        assertEquals("dk", layout.getString("layout"))
        assertEquals("", layout.getString("variant"))
    }
}
