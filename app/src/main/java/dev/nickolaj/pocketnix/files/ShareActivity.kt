package dev.nickolaj.pocketnix.files

import android.app.Activity
import android.content.Intent
import android.net.Uri
import android.os.Bundle
import android.util.Log
import android.widget.Toast
import dev.nickolaj.pocketnix.gui.Helper
import dev.nickolaj.pocketnix.vm
import kotlinx.coroutines.runBlocking
import java.io.File

/**
 * The share sheet's way into the guest: text and files land in
 * `pocketnix.guest.share.directory` (the module's share command then runs on
 * each). Invisible; reports with a toast.
 */
class ShareActivity : Activity(), Helper {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // A rotation recreates the activity: the share already ran.
        if (savedInstanceState != null) {
            finish()
            return
        }
        val files = vm.files()
        if (files == null) {
            Toast.makeText(this, "Start the VM in pocketnix first", Toast.LENGTH_LONG).show()
            finish()
            return
        }
        val uris = streams(intent)
        val text = intent.getStringExtra(Intent.EXTRA_TEXT)
        if (uris.isEmpty() && text.isNullOrEmpty()) {
            finish()
            return
        }
        Thread({
            val result = runCatching {
                if (uris.isEmpty()) {
                    runBlocking { vm.shareText(text!!) }
                } else {
                    uris.map { share(files, it) }.last()
                }
            }
            runOnUiThread {
                val msg = result.fold(
                    { p -> if (uris.size > 1) "Shared ${uris.size} files to ${File(p).parent}" else "Shared to $p" },
                    { e -> "Share failed: ${e.message}" },
                )
                Toast.makeText(this, msg, Toast.LENGTH_LONG).show()
                finish()
            }
        }, "share").start()
    }

    private fun streams(intent: Intent): List<Uri> = when (intent.action) {
        Intent.ACTION_SEND -> listOfNotNull(intent.getParcelableExtra(Intent.EXTRA_STREAM, Uri::class.java))
        Intent.ACTION_SEND_MULTIPLE -> intent.getParcelableArrayListExtra(Intent.EXTRA_STREAM, Uri::class.java).orEmpty()
        else -> emptyList()
    }.filter(::shareable)

    /**
     * Only other apps' content: a file: URI, or one of this app's own
     * providers, would be read with this app's rights, so any app could have
     * it copy its private files (the disk image) into the guest.
     */
    private fun shareable(uri: Uri): Boolean {
        val ok = uri.scheme == "content" && uri.authority?.startsWith("$packageName.") != true && uri.authority != packageName
        if (!ok) Log.w("ShareActivity", "refusing to share $uri")
        return ok
    }

    private fun share(files: dev.nickolaj.pocketnix.agent.GuestFiles, uri: Uri): String =
        withSize(this, uri) { name, size, input -> files.share(name, size, input) }
}
