package dev.nickolaj.pocketnix.agent

import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.json.JSONObject
import org.junit.Test
import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.io.IOException

class ProtocolTest {
    // As the agent sends it (go/internal/agent normalizes defaults before sending).
    private val config = """
        {"type":"config","hostname":"phone","forwards":[{"name":"ssh","guestPort":22,"hostPort":2222,"lan":false},
        {"name":"web","guestPort":8080,"hostPort":8080,"lan":true,"protocol":"tcp"},
        {"name":"mosh","guestPort":60001,"hostPort":60001,"lan":false,"protocol":"udp"}],
        "terminal":{"profiles":[{"name":"htop","command":"htop"},{"name":"shell"}],"defaultProfile":"shell",
        "extraKeys":[[{"key":"ESC"},{"key":"-","popup":"|"},{"key":"x","label":"X!"}]],
        "theme":{"colors":{"background":"#1e1e2e"},"font":"/nix/store/x-font/a.ttf","fontSha256":"ab"}}}
    """.trimIndent()

    @Test
    fun parsesConfig() {
        val c = GuestConfig.parse(config)
        assertEquals("phone", c.hostname)
        assertEquals(
            listOf(Forward("ssh", 22, 2222, false), Forward("web", 8080, 8080, true), Forward("mosh", 60001, 60001, false, udp = true)),
            c.forwards,
        )
        assertEquals(listOf(Profile("htop", "htop"), Profile("shell", null)), c.profiles)
        assertEquals("shell", c.defaultProfile)
        assertEquals(listOf(ExtraKey("ESC", null, null), ExtraKey("-", "|", null), ExtraKey("x", null, "X!")), c.extraKeys.single())
        assertEquals(mapOf("background" to "#1e1e2e"), c.theme.colors)
        assertEquals("/nix/store/x-font/a.ttf", c.theme.font)
    }

    @Test
    fun parsesSnippets() {
        val c = GuestConfig.parse("""{"type":"config","terminal":{"snippets":[{"name":"Logs","command":"journalctl -f"},{"name":"df","command":"df -h","submit":true}]}}""")
        assertEquals(listOf(Snippet("Logs", "journalctl -f", false), Snippet("df", "df -h", true)), c.snippets)
    }

    @Test
    fun parsesUpdates() {
        val u = Updates.parse(
            JSONObject(
                """
                {"type":"updates","sources":["buildHost"],"state":"idle","op":3,
                 "current":"/nix/store/b-sys","booted":"/nix/store/a-sys","available":"/nix/store/c-sys","source":"buildHost",
                 "checkedAt":1700000000,"log":["x"],
                 "generations":[{"number":2,"path":"/nix/store/b-sys","date":1700000000,"label":"26.05","current":true},
                                {"number":1,"path":"/nix/store/a-sys","date":1690000000,"booted":true}]}
                """.trimIndent(),
            ),
        )
        assertEquals(false, u.busy)
        assertEquals(true, u.rebootPending)
        assertEquals("/nix/store/c-sys", u.available)
        assertEquals(listOf(2, 1), u.generations.map { it.number })
        assertEquals(Generation(1, "/nix/store/a-sys", 1690000000, null, current = false, booted = true), u.generations[1])
        // A minimal state: nothing configured, nothing found.
        val empty = Updates.parse(JSONObject("""{"type":"updates","state":"checking"}"""))
        assertEquals(true, empty.busy)
        assertNull(empty.available)
        assertEquals(false, empty.rebootPending)
    }

    @Test
    fun parsesAMinimalConfig() {
        val c = GuestConfig.parse("""{"type":"config"}""")
        assertEquals(emptyList<Forward>(), c.forwards)
        assertEquals(emptyList<List<ExtraKey>>(), c.extraKeys)
        assertNull(c.theme.font)
        assertNull("an agent without GUI support", c.gui)
    }

    @Test
    fun parsesGuiConfig() {
        val c = GuestConfig.parse(
            """{"type":"config","gui":{"enable":true,"defaults":{"size":"phone","input":"touch"},
               "appDefaults":{"org.gnome.Calculator":{"size":"compact"}}}}""",
        )
        val gui = c.gui!!
        assertEquals(true, gui.enable)
        assertEquals(GuiAppSettings("phone", "touch"), gui.defaults)
        assertEquals(mapOf("org.gnome.Calculator" to GuiAppSettings("compact", null)), gui.appDefaults)
        assertEquals(false, GuestConfig.parse("""{"type":"config","gui":{}}""").gui!!.enable)
    }

    @Test
    fun ignoresUnknownFields() {
        val c = GuestConfig.parse("""{"type":"config","future":1,"terminal":{"profiles":[{"name":"a","new":true}]}}""")
        assertEquals(listOf(Profile("a", null)), c.profiles)
    }

    @Test
    fun readLineLeavesTheRestOfTheStream() {
        val input = ByteArrayInputStream("{\"type\":\"hello\",\"version\":1}\n\u0000rest".toByteArray())
        assertEquals(1, Protocol.readHello(input).getInt("version"))
        assertArrayEquals("\u0000rest".toByteArray(), input.readBytes())
    }

    @Test
    fun agentErrorsAndVersionMismatchesThrow() {
        val err = assertThrows(AgentException::class.java) {
            Protocol.readHello(ByteArrayInputStream("{\"type\":\"error\",\"message\":\"unknown profile\"}\n".toByteArray()))
        }
        assertEquals("unknown profile", err.message)
        assertThrows(AgentException::class.java) {
            Protocol.readHello(ByteArrayInputStream("{\"type\":\"hello\",\"version\":99}\n".toByteArray()))
        }
        assertThrows(IOException::class.java) { Protocol.readLine(ByteArrayInputStream("no newline".toByteArray())) }
    }

    @Test
    fun helloNamesTheStream() {
        val out = ByteArrayOutputStream()
        Protocol.writeJson(out, Protocol.hello("pty", "test") { put("profile", "htop"); put("rows", 24) })
        val line = out.toString()
        assert(line.endsWith("\n"))
        val h = org.json.JSONObject(line)
        assertEquals("hello", h.getString("type"))
        assertEquals(Protocol.VERSION, h.getInt("version"))
        assertEquals("pty", h.getString("stream"))
        assertEquals("htop", h.getString("profile"))
    }
}
