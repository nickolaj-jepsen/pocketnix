package dev.nickolaj.pocketnix.agent

import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Test
import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.io.IOException

class ProtocolTest {
    // As the agent sends it (go/internal/agent normalizes defaults before sending).
    private val config = """
        {"type":"config","hostname":"phone","forwards":[{"name":"ssh","guestPort":22,"hostPort":2222,"lan":false},
        {"name":"web","guestPort":8080,"hostPort":8080,"lan":true}],
        "terminal":{"profiles":[{"name":"htop","command":"htop"},{"name":"shell"}],"defaultProfile":"shell",
        "extraKeys":[[{"key":"ESC"},{"key":"-","popup":"|"},{"key":"x","label":"X!"}]],
        "theme":{"colors":{"background":"#1e1e2e"},"font":"/nix/store/x-font/a.ttf","fontSha256":"ab"}}}
    """.trimIndent()

    @Test
    fun parsesConfig() {
        val c = GuestConfig.parse(config)
        assertEquals("phone", c.hostname)
        assertEquals(listOf(Forward("ssh", 22, 2222, false), Forward("web", 8080, 8080, true)), c.forwards)
        assertEquals(listOf(Profile("htop", "htop"), Profile("shell", null)), c.profiles)
        assertEquals("shell", c.defaultProfile)
        assertEquals(listOf(ExtraKey("ESC", null, null), ExtraKey("-", "|", null), ExtraKey("x", null, "X!")), c.extraKeys.single())
        assertEquals(mapOf("background" to "#1e1e2e"), c.theme.colors)
        assertEquals("/nix/store/x-font/a.ttf", c.theme.font)
    }

    @Test
    fun parsesAMinimalConfig() {
        val c = GuestConfig.parse("""{"type":"config"}""")
        assertEquals(emptyList<Forward>(), c.forwards)
        assertEquals(emptyList<List<ExtraKey>>(), c.extraKeys)
        assertNull(c.theme.font)
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
