package dev.nickolaj.pocketnix

import android.content.ContentProvider
import android.content.ContentValues
import android.database.Cursor
import android.net.Uri
import android.os.Bundle
import android.os.ParcelFileDescriptor
import android.util.Log
import dev.nickolaj.pocketnix.provision.requireAdbCaller
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull
import java.io.FileNotFoundException
import kotlin.concurrent.thread

/**
 * Debug builds' scripted checks over adb, answered to the caller rather
 * than in logcat, so they work with the phone locked too. `pocketnix
 * control` wraps them.
 *
 * - `content read --uri content://<package>.control/dump[?tab=<id>&lines=<n>]`:
 *   the VM's state, the last `lines` ([DUMP_LINES]) of every tab's transcript (or one
 *   tab's) and the GUI windows, as text; `error: <message>` when it can't
 * - `content call --uri content://<package>.control --method tab [--arg <profile>]
 *   [--extra input:s:<text>] [--extra enter:b:true] [--extra timeoutMs:l:<ms>]`:
 *   opens a terminal tab once the agent is up (`timeoutMs`, [TAB_TIMEOUT_MS]),
 *   types `input` and, with `enter`, Enter into it, and answers its id as `tab`
 * - `--method type --arg <tab id> --extra input:s:<text> [--extra enter:b:true]`:
 *   types into an open tab
 *
 * `content` takes a backslash in a value as escaping the next character, and
 * splits at unescaped colons: send `\\` for each backslash in `input` and
 * `\:` for each colon.
 * A call's errors come back as an `error` key.
 */
class ControlProvider : ContentProvider() {
    override fun onCreate() = true

    // Binder threads, never the main one: blocking on it here can't deadlock.
    override fun call(method: String, arg: String?, extras: Bundle?): Bundle {
        requireAdbCaller("control the app")
        val out = Bundle()
        try {
            if (!BuildConfig.DEBUG) error("debug builds only")
            val vm = context!!.vm
            val input = extras?.getString("input").orEmpty()
            val enter = extras?.getBoolean("enter", false) ?: false
            when (method) {
                "tab" -> {
                    val timeout = extras?.getLong("timeoutMs", TAB_TIMEOUT_MS) ?: TAB_TIMEOUT_MS
                    val id = runBlocking { withTimeoutOrNull(timeout) { vm.debugOpenTab(arg, input, enter) } }
                        ?: error("the agent wasn't up within $timeout ms")
                    out.putInt("tab", id)
                }
                "type" -> onMain { vm.debugType(tabId(arg ?: error("type needs a tab id")), input, enter) }
                else -> error("unknown method $method")
            }
            out.putBoolean("ok", true)
        } catch (e: Exception) {
            out.putString("error", e.message ?: e.javaClass.simpleName)
        }
        return out
    }

    override fun openFile(uri: Uri, mode: String): ParcelFileDescriptor {
        requireAdbCaller("control the app")
        if (uri.path != "/dump") throw FileNotFoundException(uri.toString())
        val text = try {
            if (!BuildConfig.DEBUG) error("debug builds only")
            val tab = uri.getQueryParameter("tab")?.let(::tabId)
            val lines = uri.getQueryParameter("lines")?.toIntOrNull() ?: DUMP_LINES
            onMain { context!!.vm.debugDump(tab, lines) }.joinToString("\n")
        } catch (e: Exception) {
            "error: ${e.message ?: e.javaClass.simpleName}"
        }
        val (read, write) = ParcelFileDescriptor.createPipe()
        // A pipe holds 64 KiB: the reader drains it while this writes.
        thread(name = "control-dump", isDaemon = true) {
            runCatching { ParcelFileDescriptor.AutoCloseOutputStream(write).use { it.write("$text\n".toByteArray()) } }
                .onFailure { Log.w(TAG, "dump: ${it.message}") }
        }
        return read
    }

    private fun <T> onMain(block: () -> T): T = runBlocking { withContext(Dispatchers.Main) { block() } }

    private fun tabId(arg: String) = arg.toIntOrNull() ?: error("not a tab id: $arg")

    override fun query(uri: Uri, projection: Array<out String>?, selection: String?, selectionArgs: Array<out String>?, sortOrder: String?): Cursor? = null
    override fun getType(uri: Uri): String? = null
    override fun insert(uri: Uri, values: ContentValues?): Uri? = null
    override fun delete(uri: Uri, selection: String?, selectionArgs: Array<out String>?): Int = 0
    override fun update(uri: Uri, values: ContentValues?, selection: String?, selectionArgs: Array<out String>?): Int = 0

    private companion object {
        const val TAG = "ControlProvider"
        const val DUMP_LINES = 40
        const val TAB_TIMEOUT_MS = 60_000L
    }
}
