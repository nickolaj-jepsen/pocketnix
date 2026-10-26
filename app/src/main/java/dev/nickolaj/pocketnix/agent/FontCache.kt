package dev.nickolaj.pocketnix.agent

import java.io.DataInputStream
import java.io.File
import java.io.IOException
import java.security.MessageDigest

/** Font files fetched from the agent, cached by content hash so a reconnect doesn't refetch. */
class FontCache(private val dir: File, private val appVersion: String) {
    /** Returns the cached file for [sha256], fetching it over a font stream when missing. */
    fun get(dialer: AgentDialer, sha256: String): File {
        require(sha256.matches(Regex("[0-9a-f]{64}"))) { "bad font hash $sha256" }
        val file = File(dir, "$sha256.ttf")
        if (file.exists()) return file
        val (s, hello) = dialer.open(Protocol.hello("font", appVersion))
        val bytes = s.use {
            val size = hello.optLong("size")
            if (size <= 0 || size > MAX_FONT) throw IOException("font size $size")
            ByteArray(size.toInt()).also { DataInputStream(s.input).readFully(it) }
        }
        val got = MessageDigest.getInstance("SHA-256").digest(bytes).joinToString("") { "%02x".format(it) }
        if (got != sha256) throw IOException("font hash $got, config says $sha256")
        dir.mkdirs()
        // Old fonts are a few hundred KB each; keep only the current one.
        dir.listFiles()?.forEach { it.delete() }
        val tmp = File(dir, "$sha256.part")
        tmp.writeBytes(bytes)
        if (!tmp.renameTo(file)) throw IOException("couldn't store the font")
        return file
    }

    private companion object {
        const val MAX_FONT = 64L shl 20
    }
}
