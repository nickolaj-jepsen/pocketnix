package dev.nickolaj.pocketnix.managed

import dev.nickolaj.pocketnix.BuildConfig
import dev.nickolaj.pocketnix.Settings
import java.io.File
import java.io.IOException
import java.io.InputStream
import java.io.RandomAccessFile
import java.net.HttpURLConnection
import java.net.URL
import java.security.MessageDigest
import java.util.zip.GZIPInputStream

/**
 * This app's GitHub release: the base image and the package index it
 * downloads, named as .github/workflows/release.yml publishes them.
 */
object Release {
    /** The app's version without a build's suffix: the release whose files it uses. */
    val version: String = BuildConfig.VERSION_NAME.substringBefore('-')

    /** The version a managed guest's flake pins, or null for a build that isn't a release. */
    val pin: String? = BuildConfig.VERSION_NAME.takeIf { Regex("[0-9]+\\.[0-9]+\\.[0-9]+").matches(it) }

    fun baseImage() = "pocketnix-base-$version.img.gz"

    fun packageIndex() = "pocketnix-packages-$version.tsv.gz"

    /** Where the release's files are; debug builds may point elsewhere ([Settings.releaseUrl]). */
    fun url(settings: Settings, name: String): URL {
        val base = settings.releaseUrl.ifEmpty { "https://github.com/nickolaj-jepsen/pocketnix/releases/download/v$version/" }
        return URL(base.trimEnd('/') + "/" + name)
    }

    /**
     * Downloads [url] to [dst], continuing a partial [dst] where the server
     * allows it. [progress] gets bytes so far and the total (0 if unknown).
     */
    fun download(url: URL, dst: File, progress: (Long, Long) -> Unit = { _, _ -> }) {
        var have = if (dst.exists()) dst.length() else 0L
        val conn = (url.openConnection() as HttpURLConnection).apply {
            connectTimeout = TIMEOUT_MS
            readTimeout = TIMEOUT_MS
            if (have > 0) setRequestProperty("Range", "bytes=$have-")
        }
        try {
            val code = conn.responseCode
            when {
                code == HttpURLConnection.HTTP_PARTIAL -> {}

                // Asked for nothing new: it's whole.
                code == 416 && have > 0 -> return

                code == HttpURLConnection.HTTP_OK -> have = 0

                else -> throw IOException("$url: HTTP $code")
            }
            val total = conn.contentLengthLong.let { if (it < 0) 0 else it + have }
            RandomAccessFile(dst, "rw").use { out ->
                out.setLength(have)
                out.seek(have)
                conn.inputStream.use { input ->
                    val buf = ByteArray(BUFFER)
                    var done = have
                    while (true) {
                        val n = input.read(buf)
                        if (n < 0) break
                        out.write(buf, 0, n)
                        done += n
                        progress(done, total)
                    }
                }
            }
        } finally {
            conn.disconnect()
        }
    }

    /**
     * Checks [file] against the checksum at [sums] (sha256sum's format). A
     * file that doesn't match is deleted, since [download] would take it as
     * whole and never fetch it again.
     */
    fun verify(sums: URL, file: File) {
        val name = sums.path.substringAfterLast('/').removeSuffix(".sha256")
        val text = (sums.openConnection() as HttpURLConnection).run {
            connectTimeout = TIMEOUT_MS
            readTimeout = TIMEOUT_MS
            try {
                if (responseCode != HttpURLConnection.HTTP_OK) throw IOException("$sums: HTTP $responseCode")
                inputStream.bufferedReader().readText()
            } finally {
                disconnect()
            }
        }
        val want = text.trim().substringBefore(' ').lowercase()
        val md = MessageDigest.getInstance("SHA-256")
        file.inputStream().use { input ->
            val buf = ByteArray(BUFFER)
            while (true) {
                val n = input.read(buf)
                if (n < 0) break
                md.update(buf, 0, n)
            }
        }
        val got = md.digest().joinToString("") { "%02x".format(it) }
        if (got != want) {
            file.delete()
            throw IOException("$name doesn't match its checksum")
        }
    }

    /**
     * Unpacks the gzipped image [gz] into [dst], leaving zero blocks as holes
     * so the disk takes only the space its data needs. [progress] gets the
     * compressed bytes read and their total.
     */
    fun unpack(gz: File, dst: File, progress: (Long, Long) -> Unit = { _, _ -> }) {
        val total = gz.length()
        val counted = CountingInput(gz.inputStream())
        RandomAccessFile(dst, "rw").use { out ->
            out.setLength(0)
            GZIPInputStream(counted, BUFFER).use { input ->
                val buf = ByteArray(BUFFER)
                var at = 0L
                while (true) {
                    var n = 0
                    while (n < buf.size) {
                        val r = input.read(buf, n, buf.size - n)
                        if (r < 0) break
                        n += r
                    }
                    if (n == 0) break
                    if (buf.allZero(n)) out.seek(at + n) else out.write(buf, 0, n)
                    at += n
                    progress(counted.count, total)
                }
                out.setLength(at)
            }
        }
    }

    private fun ByteArray.allZero(n: Int): Boolean {
        for (i in 0 until n) if (this[i].toInt() != 0) return false
        return true
    }

    private class CountingInput(private val inner: InputStream) : InputStream() {
        var count = 0L
        override fun read(): Int = inner.read().also { if (it >= 0) count++ }
        override fun read(b: ByteArray, off: Int, len: Int): Int = inner.read(b, off, len).also { if (it > 0) count += it }
        override fun close() = inner.close()
    }

    private const val TIMEOUT_MS = 30_000
    private const val BUFFER = 1 shl 20
}
