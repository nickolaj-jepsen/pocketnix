package dev.nickolaj.pocketnix.gui

import android.util.Log
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import org.json.JSONArray
import org.json.JSONException
import org.json.JSONObject
import java.io.File
import java.io.IOException

/** One `[Desktop Action]` of an entry. */
data class AppAction(val id: String, val name: String, val icon: String)

/** One entry of the compositor's app list (gui-protocol.md, The app list). */
data class AppEntry(
    /** The desktop file id without `.desktop`. */
    val id: String,
    val name: String,
    val genericName: String = "",
    val comment: String = "",
    val keywords: List<String> = emptyList(),
    val categories: List<String> = emptyList(),
    /** An icon key, or "" for none. */
    val icon: String = "",
    val noDisplay: Boolean = false,
    val actions: List<AppAction> = emptyList(),
) {
    /** The name, falling back to the id for an entry that somehow has none. */
    val label: String get() = name.ifEmpty { id }

    /** Whether every word of [query] is in its name, generic name, keywords or id, ignoring case. */
    fun matches(query: String): Boolean {
        val hay = (listOf(name, genericName, id) + keywords).joinToString("\n").lowercase()
        return query.lowercase().split(' ').filter { it.isNotEmpty() }.all { it in hay }
    }

    fun toJson(): JSONObject = JSONObject()
        .put("id", id)
        .put("name", name)
        .put("genericName", genericName)
        .put("comment", comment)
        .put("keywords", JSONArray(keywords))
        .put("categories", JSONArray(categories))
        .put("icon", icon)
        .put("noDisplay", noDisplay)
        .put("actions", JSONArray(actions.map { JSONObject().put("id", it.id).put("name", it.name).put("icon", it.icon) }))

    companion object {
        /** Null for an entry without a usable id; an icon key that isn't one counts as none. */
        fun parse(o: JSONObject): AppEntry? {
            val id = o.optString("id")
            if (!validAppId(id)) return null
            return AppEntry(
                id = id,
                name = o.optString("name"),
                genericName = o.optString("genericName"),
                comment = o.optString("comment"),
                keywords = o.optJSONArray("keywords").strings(),
                categories = o.optJSONArray("categories").strings(),
                icon = o.optString("icon").takeIf(::validIconKey).orEmpty(),
                noDisplay = o.optBoolean("noDisplay"),
                actions = o.optJSONArray("actions")?.let { a ->
                    (0 until a.length()).mapNotNull { i ->
                        val x = a.optJSONObject(i) ?: return@mapNotNull null
                        val aid = x.optString("id").takeIf { it.isNotEmpty() } ?: return@mapNotNull null
                        AppAction(aid, x.optString("name"), x.optString("icon").takeIf(::validIconKey).orEmpty())
                    }
                }.orEmpty(),
            )
        }

        fun parseList(a: JSONArray?): List<AppEntry> =
            if (a == null) emptyList() else (0 until a.length()).mapNotNull { a.optJSONObject(it)?.let(::parse) }

        private fun JSONArray?.strings(): List<String> =
            if (this == null) emptyList() else (0 until length()).mapNotNull { optString(it).takeIf { s -> s.isNotEmpty() } }
    }
}

/** An icon key as the compositor makes them; anything else never reaches a file name. */
fun validIconKey(key: String): Boolean = key.length in 1..64 && key.all { it in '0'..'9' || it in 'a'..'z' }

/** A desktop file id the app can name in URIs and shortcut ids. */
fun validAppId(id: String): Boolean = id.length in 1..255 && id.none { it == '/' || it.isISOControl() }

/**
 * The guest's app list, its rendered icons and the recent launches, kept
 * in [dir] so the launcher and pins work with the VM off. Thread-safe.
 */
class AppCatalog(private val dir: File) {
    private val listFile = File(dir, "apps.json")
    private val iconDir = File(dir, "icons")
    private var entries: Map<String, AppEntry> = emptyMap()
    private var recentIds: List<String> = emptyList()

    private val _revision = MutableStateFlow(0)
    /** Bumped on every change to the list, an icon or the recent launches. */
    val revision: StateFlow<Int> = _revision.asStateFlow()

    /** What a new list changed. */
    data class Change(val gone: Set<String>, val added: Set<String>, val changed: Boolean)

    init {
        load()
    }

    /** Every entry, sorted by id. */
    val apps: List<AppEntry> @Synchronized get() = entries.values.sortedBy { it.id }

    @Synchronized fun app(id: String): AppEntry? = entries[id]

    /** The recently launched apps still listed, most recent first. */
    val recent: List<AppEntry> @Synchronized get() = recentIds.mapNotNull { entries[it] }

    /** Replaces the list with [list] and drops the icons no entry names any more. */
    @Synchronized fun update(list: List<AppEntry>): Change {
        val next = list.associateBy { it.id }
        val change = Change(entries.keys - next.keys, next.keys - entries.keys, next != entries)
        if (!change.changed) return change
        entries = next
        save()
        val keys = iconKeys()
        iconDir.listFiles()?.forEach { f -> if (f.name.substringBefore('-') !in keys) f.delete() }
        _revision.value++
        return change
    }

    /** The cached PNG of [key] at [size], or null when there is none (not fetched yet, or the guest has none). */
    @Synchronized fun icon(key: String, size: Int): File? =
        iconFile(key, size)?.takeIf { it.length() > 0 }

    /** The icons the listed, shown entries name that aren't cached at one of [sizes]. */
    @Synchronized fun missingIcons(sizes: Collection<Int>): List<Pair<String, Int>> =
        entries.values.filter { !it.noDisplay || it.id in recentIds }.map { it.icon }.filter { it.isNotEmpty() }.distinct()
            .flatMap { key -> sizes.filter { iconFile(key, it)?.exists() == false }.map { key to it } }

    /** Stores an ICON answer; an empty [png] records that the guest has no icon there. Ignored for a key no entry names. */
    @Synchronized fun storeIcon(key: String, size: Int, png: ByteArray) {
        if (key !in iconKeys()) return
        val f = iconFile(key, size) ?: return
        try {
            iconDir.mkdirs()
            writeAtomically(f, png)
            _revision.value++
        } catch (e: IOException) {
            Log.w(TAG, "icon $key@$size: ${e.message}")
        }
    }

    /** Records a launch of [id] for the recent apps. */
    @Synchronized fun launched(id: String) {
        recentIds = (listOf(id) + recentIds.filter { it != id }).take(MAX_RECENT)
        save()
        _revision.value++
    }

    private fun iconKeys(): Set<String> = entries.values.flatMap { e -> listOf(e.icon) + e.actions.map { it.icon } }.filter { it.isNotEmpty() }.toSet()

    private fun iconFile(key: String, size: Int): File? = if (validIconKey(key) && size > 0) File(iconDir, "$key-$size.png") else null

    private fun load() {
        val o = try {
            JSONObject(listFile.readText())
        } catch (e: IOException) {
            return
        } catch (e: JSONException) {
            Log.w(TAG, "${listFile.name} is unreadable; starting empty: ${e.message}")
            return
        }
        entries = AppEntry.parseList(o.optJSONArray("apps")).associateBy { it.id }
        val r = o.optJSONArray("recent")
        recentIds = if (r == null) emptyList() else (0 until r.length()).map { r.optString(it) }.filter(::validAppId)
    }

    private fun save() {
        val o = JSONObject()
            .put("apps", JSONArray(entries.values.sortedBy { it.id }.map { it.toJson() }))
            .put("recent", JSONArray(recentIds))
        try {
            dir.mkdirs()
            writeAtomically(listFile, o.toString().toByteArray())
        } catch (e: IOException) {
            Log.w(TAG, "saving the app list: ${e.message}")
        }
    }

    private fun writeAtomically(f: File, bytes: ByteArray) {
        val tmp = File(f.parentFile, f.name + ".tmp")
        tmp.writeBytes(bytes)
        if (!tmp.renameTo(f)) throw IOException("rename to ${f.name} failed")
    }

    private companion object {
        const val TAG = "AppCatalog"
        const val MAX_RECENT = 8
    }
}
