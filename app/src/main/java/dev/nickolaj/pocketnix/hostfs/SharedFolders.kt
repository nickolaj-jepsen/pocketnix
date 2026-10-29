package dev.nickolaj.pocketnix.hostfs

import android.content.Context
import android.content.Intent
import android.net.Uri
import android.provider.DocumentsContract
import android.util.Log
import androidx.core.content.edit
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import org.json.JSONArray
import org.json.JSONObject

/** An Android folder shared with the guest, mounted at `<mount point>/<name>`; [uri] is its SAF tree. */
data class SharedFolder(val name: String, val uri: String, val writable: Boolean)

/**
 * The folders the user shares with the guest: device state, so the app owns
 * it (docs/configuration.md, Shared Android folders). Each holds a persisted tree permission.
 */
class SharedFolders(private val context: Context) {
    private val prefs = context.getSharedPreferences("host-folders", Context.MODE_PRIVATE)
    private val _folders = MutableStateFlow(load())
    val folders: StateFlow<List<SharedFolder>> = _folders.asStateFlow()

    private fun load(): List<SharedFolder> = runCatching {
        val a = JSONArray(prefs.getString(KEY, "[]"))
        (0 until a.length()).map { a.getJSONObject(it) }.map {
            SharedFolder(it.getString("name"), it.getString("uri"), it.optBoolean("writable", true))
        }
    }.onFailure { Log.w(TAG, "shared folders: ${it.message}") }.getOrDefault(emptyList())

    private fun save(list: List<SharedFolder>) {
        val a = JSONArray(list.map { JSONObject().put("name", it.name).put("uri", it.uri).put("writable", it.writable) })
        prefs.edit { putString(KEY, a.toString()) }
        _folders.value = list
    }

    /** Why [name] can't name a new folder, or null. */
    fun problem(name: String, except: String? = null): String? = when {
        name.isEmpty() -> "Needs a name"
        name == "." || name == ".." || '/' in name || '\u0000' in name -> "Not a file name"
        name.toByteArray().size > 255 -> "Too long"
        folders.value.any { it.name == name && it.name != except } -> "Already a shared folder's name"
        else -> null
    }

    /**
     * Shares the tree [uri] picked with ACTION_OPEN_DOCUMENT_TREE, keeping
     * its permission across reboots; read-only when only reading was
     * granted. Throws SecurityException when nothing was.
     */
    fun add(uri: Uri, name: String) {
        val resolver = context.contentResolver
        val writable = runCatching {
            resolver.takePersistableUriPermission(uri, Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_GRANT_WRITE_URI_PERMISSION)
        }.isSuccess
        if (!writable) resolver.takePersistableUriPermission(uri, Intent.FLAG_GRANT_READ_URI_PERMISSION)
        save(folders.value + SharedFolder(name, uri.toString(), writable))
    }

    fun update(folder: SharedFolder, name: String = folder.name, writable: Boolean = folder.writable) {
        save(folders.value.map { if (it == folder) it.copy(name = name, writable = writable) else it })
    }

    fun remove(folder: SharedFolder) {
        save(folders.value - folder)
        if (folders.value.none { it.uri == folder.uri }) {
            runCatching {
                context.contentResolver.releasePersistableUriPermission(
                    Uri.parse(folder.uri),
                    Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_GRANT_WRITE_URI_PERMISSION
                )
            }
        }
    }

    companion object {
        private const val TAG = "SharedFolders"
        private const val KEY = "folders"

        /** The tree's own name, as the default for its folder's ("primary:Download/Books" → "Books"). */
        fun defaultName(uri: Uri): String = runCatching {
            DocumentsContract.getTreeDocumentId(uri).substringAfterLast(':').substringAfterLast('/')
        }.getOrDefault("").ifEmpty { "Android" }

        /** Where the tree lives, for display ("primary:Download/Books" → "Download/Books"). */
        fun location(uri: String): String = runCatching {
            DocumentsContract.getTreeDocumentId(Uri.parse(uri)).substringAfter(':')
        }.getOrDefault(uri)
    }
}
