package dev.nickolaj.pocketnix.gui

import android.content.Context
import android.util.Log
import dev.nickolaj.pocketnix.agent.GuiAppSettings
import dev.nickolaj.pocketnix.agent.GuiConfig
import org.json.JSONObject
import kotlin.math.ceil
import kotlin.math.min

/** A window's size preset: its scale is the display's density times [factor] (docs/gui.md, Size). */
enum class SizePreset(val id: String, val label: String, val factor: Float) {
    PHONE("phone", "Phone", 1f),
    COMPACT("compact", "Compact", 0.75f),
    DESKTOP("desktop", "Desktop", 0.5f),
    ;

    companion object {
        fun of(id: String?): SizePreset? = entries.firstOrNull { it.id == id }
    }
}

/** How touches reach a window (docs/gui.md, Input). */
enum class InputMode(val id: String, val label: String) {
    TOUCH("touch", "Touch"),
    POINTER("pointer", "Pointer"),
    TRACKPAD("trackpad", "Trackpad"),
    ;

    companion object {
        fun of(id: String?): InputMode? = entries.firstOrNull { it.id == id }
    }
}

/** One app's settings as resolved, and whether the user changed any of them on the phone. */
data class AppChoice(val size: SizePreset, val input: InputMode, val overridden: Boolean)

/** What the user changed for one app; a null field keeps the config's. */
data class AppOverride(val size: SizePreset? = null, val input: InputMode? = null) {
    val empty: Boolean get() = size == null && input == null
}

/** Where the user's changes live. */
interface OverrideStore {
    fun load(): Map<String, AppOverride>

    /** Stores [o] for [app]; null removes it. */
    fun save(app: String, o: AppOverride?)
}

/**
 * Per-app GUI settings: the module's `gui.defaults` and `gui.appDefaults`,
 * under the changes the user made on the phone, per app id. Thread-safe:
 * reads take an immutable snapshot.
 */
class AppSettings(private val store: OverrideStore) {
    @Volatile private var config: GuiConfig? = null
    @Volatile private var overrides: Map<String, AppOverride> = runCatching { store.load() }
        .onFailure { Log.w(TAG, "the stored app settings don't load", it) }.getOrDefault(emptyMap())

    /** Called after every change, the config's included, on the thread that made it. */
    @Volatile var onChange: () -> Unit = {}

    /** The module's settings; null keeps the last ones (the VM is off, and its config with it). */
    fun config(c: GuiConfig?) {
        if (c == null || c == config) return
        config = c
        onChange()
    }

    fun of(app: String): AppChoice {
        val o = overrides[app]
        val c = config
        val d = c?.appDefaults?.get(app)
        return AppChoice(
            size = o?.size ?: pick(d, c?.defaults, SizePreset.PHONE) { SizePreset.of(it.size) },
            input = o?.input ?: pick(d, c?.defaults, InputMode.TOUCH) { InputMode.of(it.input) },
            overridden = o != null,
        )
    }

    fun setSize(app: String, size: SizePreset) = change(app) { it.copy(size = size) }

    fun setInput(app: String, input: InputMode) = change(app) { it.copy(input = input) }

    /** Drops the user's changes for [app]: "Reset to config". */
    fun reset(app: String) = change(app) { AppOverride() }

    /**
     * New windows' size factors, as `settings` carries them: the default
     * entry's and those of the apps that differ from it.
     */
    fun factors(): Pair<Float, Map<String, Float>> {
        val default = pick(null, config?.defaults, SizePreset.PHONE) { SizePreset.of(it.size) }.factor
        val ids = config?.appDefaults?.keys.orEmpty() + overrides.keys
        return default to ids.filter { it.isNotEmpty() }.associateWith { of(it).size.factor }.filterValues { it != default }
    }

    private fun change(app: String, edit: (AppOverride) -> AppOverride) {
        synchronized(this) {
            val o = edit(overrides[app] ?: AppOverride()).takeUnless { it.empty }
            if (o == overrides[app]) return
            overrides = if (o == null) overrides - app else overrides + (app to o)
            runCatching { store.save(app, o) }.onFailure { Log.w(TAG, "storing $app's settings failed", it) }
        }
        onChange()
    }

    private inline fun <T : Any> pick(app: GuiAppSettings?, defaults: GuiAppSettings?, builtin: T, field: (GuiAppSettings) -> T?): T =
        app?.let(field) ?: defaults?.let(field) ?: builtin

    private companion object {
        const val TAG = "AppSettings"
    }
}

/** The user's changes in shared preferences: one JSON object per app id. */
class PrefsOverrideStore(context: Context) : OverrideStore {
    private val prefs = context.getSharedPreferences("gui-apps", Context.MODE_PRIVATE)

    override fun load(): Map<String, AppOverride> = prefs.all.mapNotNull { (app, v) ->
        val o = runCatching { JSONObject(v as String) }.getOrNull() ?: return@mapNotNull null
        app to AppOverride(SizePreset.of(o.optString("size")), InputMode.of(o.optString("input")))
    }.filter { !it.second.empty }.toMap()

    override fun save(app: String, o: AppOverride?) {
        val e = prefs.edit()
        if (o == null) {
            e.remove(app)
        } else {
            val j = JSONObject()
            o.size?.let { j.put("size", it.id) }
            o.input?.let { j.put("input", it.id) }
            e.putString(app, j.toString())
        }
        e.apply()
    }
}

/** The automatic fit (docs/gui.md, Size). */
object WindowFit {
    /**
     * The scale for a [width] × [height] window: [density] × [factor],
     * lowered while the client's logical minimum ([minWidth] × [minHeight],
     * 0 for none) doesn't fit, but not below the Desktop preset's factor of
     * [density] unless the preset itself is lower.
     */
    fun scale(density: Float, factor: Float, width: Int, height: Int, minWidth: Float, minHeight: Float): Float {
        val preferred = density * factor
        val floor = min(preferred, density * SizePreset.DESKTOP.factor)
        var s = preferred
        if (width > 0 && minWidth > 0f) s = min(s, width / minWidth)
        if (height > 0 && minHeight > 0f) s = min(s, height / minHeight)
        return s.coerceAtLeast(floor)
    }

    /**
     * A client's logical size from [pixels] computed at [scale]: xdg sizes
     * are whole logical pixels, and the wire's rounding is undone upwards,
     * so the fit never gives the client less than it asked for.
     */
    fun logical(pixels: Int, scale: Float): Float =
        if (pixels <= 0 || scale <= 0f) 0f else ceil(pixels / scale - ROUNDING_SLACK).coerceAtLeast(1f)

    private const val ROUNDING_SLACK = 0.01f
}
