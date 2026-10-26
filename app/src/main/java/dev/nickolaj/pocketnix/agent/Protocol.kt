// The app's side of the agent wire protocol; go/internal/protocol is the contract.
package dev.nickolaj.pocketnix.agent

import org.json.JSONArray
import org.json.JSONObject
import java.io.ByteArrayOutputStream
import java.io.EOFException
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream

object Protocol {
    const val VERSION = 1
    const val PORT = 9100L
    private const val MAX_LINE = 1 shl 20

    /** The Hello that opens every stream. */
    fun hello(stream: String, app: String, extra: JSONObject.() -> Unit = {}): JSONObject =
        JSONObject().apply {
            put("type", "hello")
            put("version", VERSION)
            put("app", app)
            put("stream", stream)
            extra()
        }

    /** Writes one JSON line in a single write. */
    fun writeJson(out: OutputStream, obj: JSONObject) {
        out.write((obj.toString() + "\n").toByteArray(Charsets.UTF_8))
        out.flush()
    }

    /** Reads one line a byte at a time, so a following frame or raw stream stays unread. */
    fun readLine(input: InputStream): String {
        val buf = ByteArrayOutputStream()
        while (true) {
            val b = input.read()
            if (b < 0) throw EOFException("stream ended inside a line")
            if (b == '\n'.code) return buf.toString(Charsets.UTF_8.name())
            if (buf.size() >= MAX_LINE) throw IOException("line too long")
            buf.write(b)
        }
    }

    /** Reads the agent's answer to a Hello, throwing [AgentException] for an error reply. */
    fun readHello(input: InputStream): JSONObject {
        val msg = JSONObject(readLine(input))
        when (msg.optString("type")) {
            "hello" -> {
                val v = msg.optInt("version")
                if (v != VERSION) throw AgentException("agent speaks protocol $v, app speaks $VERSION")
                return msg
            }
            "error" -> throw AgentException(msg.optString("message"))
            else -> throw IOException("expected hello, got ${msg.optString("type")}")
        }
    }
}

/** The agent refused a request. */
class AgentException(message: String) : IOException(message)

data class Forward(val name: String, val guestPort: Int, val hostPort: Int, val lan: Boolean)

data class Profile(val name: String, val command: String?)

data class ExtraKey(val key: String, val popup: String?, val label: String?)

data class Theme(val colors: Map<String, String>, val font: String?, val fontSha256: String?)

/** What the NixOS module declares for the app; arrives on the control stream. */
data class GuestConfig(
    val hostname: String,
    val forwards: List<Forward>,
    val profiles: List<Profile>,
    val defaultProfile: String,
    val extraKeys: List<List<ExtraKey>>,
    val theme: Theme,
) {
    companion object {
        fun parse(json: String): GuestConfig = parse(JSONObject(json))

        fun parse(o: JSONObject): GuestConfig {
            val t = o.optJSONObject("terminal") ?: JSONObject()
            val theme = t.optJSONObject("theme") ?: JSONObject()
            val colors = theme.optJSONObject("colors") ?: JSONObject()
            return GuestConfig(
                hostname = o.optString("hostname"),
                forwards = o.optJSONArray("forwards").objects().map {
                    val guest = it.getInt("guestPort")
                    Forward(it.optString("name"), guest, it.optInt("hostPort", guest).takeIf { p -> p > 0 } ?: guest, it.optBoolean("lan"))
                },
                profiles = t.optJSONArray("profiles").objects().map { Profile(it.getString("name"), it.optStringOrNull("command")) },
                defaultProfile = t.optString("defaultProfile"),
                extraKeys = t.optJSONArray("extraKeys").arrays().map { row ->
                    row.objects().map { ExtraKey(it.getString("key"), it.optStringOrNull("popup"), it.optStringOrNull("label")) }
                },
                theme = Theme(
                    colors = colors.keys().asSequence().associateWith { colors.getString(it) },
                    font = theme.optStringOrNull("font"),
                    fontSha256 = theme.optStringOrNull("fontSha256"),
                ),
            )
        }
    }
}

private fun JSONArray?.objects(): List<JSONObject> =
    if (this == null) emptyList() else (0 until length()).map { getJSONObject(it) }

private fun JSONArray?.arrays(): List<JSONArray> =
    if (this == null) emptyList() else (0 until length()).map { getJSONArray(it) }

private fun JSONObject.optStringOrNull(key: String): String? =
    if (has(key) && !isNull(key)) getString(key).takeIf { it.isNotEmpty() } else null
