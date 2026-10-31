package dev.nickolaj.pocketnix.managed

import android.content.Context
import androidx.core.content.edit
import org.json.JSONArray
import org.json.JSONObject

/** A service switch; [id] is a key of go/internal/protocol's Services. */
data class Service(val id: String, val label: String, val detail: String)

val SERVICES = listOf(
    // Port 2022 is the module's forwards.ssh hostPort for this switch.
    Service("ssh", "SSH", "Log in from your computer: phone port 2022, keys only"),
    Service("tailscale", "Tailscale", "Join your tailnet"),
    Service("podman", "Podman", "Run containers"),
    Service("gui", "Desktop apps", "Show Linux apps as Android windows, with their sound and the clipboard")
)

/** What the app configures in a managed guest: its flake's pocketnix.json (go/internal/protocol Selections). */
data class Selections(val user: String, val services: Set<String>, val keepGenerations: Int = 0) {
    fun toJson(): JSONObject = JSONObject()
        .put("user", user)
        .put("services", JSONObject().apply { for (s in services) put(s, true) })
        .put("keepGenerations", keepGenerations)

    companion object {
        /** A Linux user name, as the module's selections.user takes it (and go/internal/agent's userName). */
        val USER_NAME = Regex("[a-z_][a-z0-9_-]{0,31}")
    }
}

/**
 * The managed guest's settings, kept by the app so a fresh disk rebuilds to
 * the same system: the selections (null until set up) and the packages.
 */
class ManagedStore(context: Context) {
    private val prefs = context.getSharedPreferences("managed", Context.MODE_PRIVATE)

    var selections: Selections?
        get() {
            val j = prefs.getString("selections", null) ?: return null
            return runCatching {
                val o = JSONObject(j)
                val services = o.optJSONObject("services") ?: JSONObject()
                Selections(
                    user = o.getString("user"),
                    services = services.keys().asSequence().filter { services.optBoolean(it) }.toSet(),
                    keepGenerations = o.optInt("keepGenerations")
                )
            }.getOrNull()
        }
        set(v) = prefs.edit { putString("selections", v?.toJson()?.toString()) }

    /** The guest may not run [selections] yet: set until a rebuild to them finishes. */
    var rebuildPending: Boolean
        get() = prefs.getBoolean("rebuildPending", false)
        set(v) = prefs.edit { putBoolean("rebuildPending", v) }

    var packages: Set<String>
        get() {
            val a = JSONArray(prefs.getString("packages", "[]"))
            return (0 until a.length()).map { a.getString(it) }.toSet()
        }
        set(v) = prefs.edit { putString("packages", JSONArray(v.sorted()).toString()) }
}
