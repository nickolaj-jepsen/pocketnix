package dev.nickolaj.pocketnix.provision

import android.content.ContentProvider
import android.content.ContentValues
import android.database.Cursor
import android.net.Uri
import android.os.Binder
import android.os.Bundle
import android.os.ParcelFileDescriptor
import android.os.Process
import android.system.Os
import android.system.OsConstants
import dev.nickolaj.pocketnix.engine.DiskImage
import dev.nickolaj.pocketnix.engine.VmState
import dev.nickolaj.pocketnix.vm
import java.io.File
import java.io.FileNotFoundException
import java.io.RandomAccessFile

/**
 * `pocketnix provision`'s way into app-private storage, for the adb shell
 * user only (`content call` / `content write`), so it works for release
 * builds, where `run-as` doesn't. The image is written to a part file:
 *
 * - call `status`: disk=<bool>, running=<bool>
 * - call `begin`, arg <bytes>: a fresh sparse part file of that size
 * - write `content://<authority>/disk?offset=<n>`: bytes at that offset
 * - call `commit`, arg `replace` to overwrite: the part file becomes the disk
 *
 * Errors come back as an `error` key rather than exceptions, which `content`
 * would only print as a stack trace.
 */
class ProvisionProvider : ContentProvider() {
    override fun onCreate() = true

    private val disk: File get() = DiskImage.file(context!!)
    private val part: File get() = File(disk.path + ".part")

    private fun running(): Boolean {
        val s = context!!.vm.state.value
        return s == VmState.Running || s == VmState.Starting
    }

    override fun call(method: String, arg: String?, extras: Bundle?): Bundle {
        requireAdbCaller("provision")
        val out = Bundle()
        try {
            when (method) {
                "status" -> {
                    out.putBoolean("disk", disk.exists())
                    out.putBoolean("running", running())
                }
                "begin" -> {
                    if (running()) error("the VM is running")
                    val size = arg?.toLongOrNull() ?: error("begin needs the image size")
                    part.delete()
                    RandomAccessFile(part, "rw").use { it.setLength(size) }
                }
                "commit" -> {
                    if (running()) error("the VM is running")
                    if (!part.exists()) error("nothing to commit; call begin first")
                    if (disk.exists() && arg != "replace") error("a disk image exists; commit with replace to destroy it")
                    // A write that failed on the phone (a full /data) may not have reached provision.
                    extras?.getString("sha256")?.let { want ->
                        val got = sha256(part)
                        if (got != want) error("the image on the phone doesn't match what was sent (sha256 $got); provision again")
                    }
                    if (!part.renameTo(disk)) error("couldn't move the image into place")
                }
                else -> error("unknown method $method")
            }
            out.putBoolean("ok", true)
        } catch (e: Exception) {
            out.putString("error", e.message ?: e.javaClass.simpleName)
        }
        return out
    }

    override fun openFile(uri: Uri, mode: String): ParcelFileDescriptor {
        requireAdbCaller("provision")
        if (uri.path != "/disk") throw FileNotFoundException(uri.toString())
        if (running()) throw IllegalStateException("the VM is running")
        if (!part.exists()) throw FileNotFoundException("call begin first")
        val offset = uri.getQueryParameter("offset")?.toLongOrNull() ?: 0L
        // No truncation: chunks land at their offsets in the pre-sized part file.
        val pfd = ParcelFileDescriptor.open(part, ParcelFileDescriptor.MODE_WRITE_ONLY)
        // The receiving side shares this file description, offset included.
        Os.lseek(pfd.fileDescriptor, offset, OsConstants.SEEK_SET)
        return pfd
    }

    override fun query(uri: Uri, projection: Array<out String>?, selection: String?, selectionArgs: Array<out String>?, sortOrder: String?): Cursor? = null
    override fun getType(uri: Uri): String? = null
    override fun insert(uri: Uri, values: ContentValues?): Uri? = null
    override fun delete(uri: Uri, selection: String?, selectionArgs: Array<out String>?): Int = 0
    override fun update(uri: Uri, values: ContentValues?, selection: String?, selectionArgs: Array<out String>?): Int = 0
}

/** Throws unless the binder call in progress comes from the adb shell (or root); [what] names the refused action. */
fun requireAdbCaller(what: String) {
    val uid = Binder.getCallingUid()
    if (uid != Process.SHELL_UID && uid != Process.ROOT_UID) throw SecurityException("only adb may $what")
}

private fun sha256(f: File): String {
    val md = java.security.MessageDigest.getInstance("SHA-256")
    f.inputStream().use { input ->
        val buf = ByteArray(1 shl 20)
        while (true) {
            val n = input.read(buf)
            if (n < 0) break
            md.update(buf, 0, n)
        }
    }
    return md.digest().joinToString("") { "%02x".format(it) }
}
