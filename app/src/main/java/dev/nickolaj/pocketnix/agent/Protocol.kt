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
    /** The app speaks MIN_VERSION..VERSION; the agent picks one (go/internal/protocol Negotiate). */
    const val VERSION = 1
    const val MIN_VERSION = 1
    const val PORT = 9100L
    private const val MAX_LINE = 1 shl 20

    /** The Hello that opens every stream. */
    fun hello(stream: String, app: String, extra: JSONObject.() -> Unit = {}): JSONObject =
        JSONObject().apply {
            put("type", "hello")
            put("version", VERSION)
            put("minVersion", MIN_VERSION)
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
                if (v !in MIN_VERSION..VERSION) throw AgentException("the agent chose protocol $v; the app speaks $MIN_VERSION–$VERSION")
                return msg
            }
            "error" -> throw AgentException(msg.optString("message"))
            else -> throw IOException("expected hello, got ${msg.optString("type")}")
        }
    }
}

/** The agent refused a request. */
class AgentException(message: String) : IOException(message)

/** [udp] forwards datagrams instead of TCP connections; the two may share a [hostPort]. */
data class Forward(val name: String, val guestPort: Int, val hostPort: Int, val lan: Boolean, val udp: Boolean = false)

data class Profile(val name: String, val command: String?)

data class ExtraKey(val key: String, val popup: String?, val label: String?)

data class Theme(val colors: Map<String, String>, val font: String?, val fontSha256: String?)

/** A command-palette entry; [submit] presses Enter after typing [command]. */
data class Snippet(val name: String, val command: String, val submit: Boolean)

/** What the NixOS module declares for the app; arrives on the control stream. */
data class GuestConfig(
    val hostname: String,
    val forwards: List<Forward>,
    val profiles: List<Profile>,
    val defaultProfile: String,
    val extraKeys: List<List<ExtraKey>>,
    val theme: Theme,
    val snippets: List<Snippet> = emptyList(),
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
                    Forward(
                        it.optString("name"), guest, it.optInt("hostPort", guest).takeIf { p -> p > 0 } ?: guest, it.optBoolean("lan"),
                        udp = it.optString("protocol") == "udp",
                    )
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
                snippets = t.optJSONArray("snippets").objects().map {
                    Snippet(it.getString("name"), it.getString("command"), it.optBoolean("submit"))
                },
            )
        }
    }
}

/** One system profile generation. */
data class Generation(
    val number: Int,
    val path: String,
    /** Unix seconds. */
    val date: Long,
    val label: String?,
    val current: Boolean,
    val booted: Boolean,
)

/** The guest's update state, pushed by the agent (go/internal/protocol/updates.go). */
data class Updates(
    val sources: List<String>,
    /** idle, checking, fetching or switching. */
    val state: String,
    val op: Int,
    val current: String,
    val booted: String,
    val available: String?,
    val source: String?,
    /** Unix seconds of the last finished check, or 0. */
    val checkedAt: Long,
    val error: String?,
    val log: List<String>,
    /** Newest first. */
    val generations: List<Generation>,
) {
    val busy: Boolean get() = state != "idle"

    /** The running system isn't the booted one: a reboot boots what a switch only activated. */
    val rebootPending: Boolean get() = booted.isNotEmpty() && current.isNotEmpty() && booted != current

    companion object {
        fun parse(o: JSONObject) = Updates(
            sources = o.optJSONArray("sources").strings(),
            state = o.optString("state", "idle"),
            op = o.optInt("op"),
            current = o.optString("current"),
            booted = o.optString("booted"),
            available = o.optStringOrNull("available"),
            source = o.optStringOrNull("source"),
            checkedAt = o.optLong("checkedAt"),
            error = o.optStringOrNull("error"),
            log = o.optJSONArray("log").strings(),
            generations = o.optJSONArray("generations").objects().map {
                Generation(
                    number = it.getInt("number"),
                    path = it.getString("path"),
                    date = it.optLong("date"),
                    label = it.optStringOrNull("label"),
                    current = it.optBoolean("current"),
                    booted = it.optBoolean("booted"),
                )
            },
        )
    }
}

private fun JSONArray?.strings(): List<String> =
    if (this == null) emptyList() else (0 until length()).map { getString(it) }

private fun JSONArray?.objects(): List<JSONObject> =
    if (this == null) emptyList() else (0 until length()).map { getJSONObject(it) }

private fun JSONArray?.arrays(): List<JSONArray> =
    if (this == null) emptyList() else (0 until length()).map { getJSONArray(it) }

private fun JSONObject.optStringOrNull(key: String): String? =
    if (has(key) && !isNull(key)) getString(key).takeIf { it.isNotEmpty() } else null
