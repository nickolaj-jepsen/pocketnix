package dev.nickolaj.pocketnix.files

import android.Manifest
import android.app.Notification
import android.app.NotificationManager
import android.content.Context
import android.content.pm.PackageManager
import android.util.Log
import dev.nickolaj.pocketnix.R
import dev.nickolaj.pocketnix.agent.GuestFiles
import java.io.File
import java.util.UUID

/**
 * Saves through the file provider's copy fallback (agents without ranged
 * access) that couldn't reach the guest (the VM stopped, the link dropped):
 * kept in app storage and uploaded when the agent is back, rather than lost.
 */
object PendingUploads {
    private const val TAG = "PendingUploads"
    private const val NOTIFICATION = 99_999

    private fun dir(context: Context) = File(context.filesDir, "pending-uploads").apply { mkdirs() }

    /** Keeps [copy] (moved, not copied) for a later upload to [path]. */
    fun keep(context: Context, path: String, copy: File) {
        val id = UUID.randomUUID().toString()
        val kept = File(dir(context), id)
        if (!copy.renameTo(kept)) copy.copyTo(kept, overwrite = true).also { copy.delete() }
        File(dir(context), "$id.path").writeText(path)
        notify(context, "Couldn't save ${path.substringAfterLast('/')} in the guest", "Kept a copy; it's saved when the VM is back.")
    }

    /** Uploads what's pending; returns how many went through. Blocks. */
    fun retry(context: Context, files: GuestFiles): Int {
        var done = 0
        for (target in dir(context).listFiles { f -> f.name.endsWith(".path") }.orEmpty()) {
            val data = File(target.path.removeSuffix(".path"))
            val path = target.readText()
            try {
                data.inputStream().use { input -> files.write(path, data.length()).use { input.copyTo(it, GuestFiles.COPY_BUFFER) } }
                data.delete()
                target.delete()
                done++
            } catch (e: Exception) {
                Log.w(TAG, "upload $path: ${e.message}")
            }
        }
        if (done > 0) notify(context, "Saved $done file${if (done > 1) "s" else ""} in the guest", "Edits kept while the VM was away are in place now.")
        return done
    }

    /** Posts the file provider's save notice (one at a time). */
    fun notify(context: Context, title: String, text: String) {
        if (context.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) return
        val n = Notification.Builder(context, "guest")
            .setSmallIcon(R.drawable.ic_pocketnix)
            .setContentTitle(title)
            .setContentText(text)
            .build()
        context.getSystemService(NotificationManager::class.java).notify(NOTIFICATION, n)
    }
}
