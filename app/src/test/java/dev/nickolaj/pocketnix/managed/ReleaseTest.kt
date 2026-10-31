package dev.nickolaj.pocketnix.managed

import com.sun.net.httpserver.HttpServer
import java.io.File
import java.io.IOException
import java.net.InetSocketAddress
import java.net.URL
import java.util.zip.GZIPOutputStream
import kotlin.random.Random
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertThrows
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

class ReleaseTest {
    @get:Rule val tmp = TemporaryFolder()

    private val mib = 1 shl 20
    private var server: HttpServer? = null

    @After
    fun stop() {
        server?.stop(0)
    }

    /** Serves [body] at /file, honouring a Range request's start, and [sums] at /file.sha256; counts the file's bytes it sent. */
    private fun serve(body: ByteArray, sums: String = ""): Pair<URL, IntArray> {
        val sent = intArrayOf(0)
        val s = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0)
        s.createContext("/file.sha256") { ex ->
            val b = sums.toByteArray()
            ex.sendResponseHeaders(200, b.size.toLong())
            ex.responseBody.use { it.write(b) }
        }
        s.createContext("/file") { ex ->
            val from = ex.requestHeaders.getFirst("Range")?.removePrefix("bytes=")?.removeSuffix("-")?.toInt() ?: 0
            ex.sendResponseHeaders(if (from > 0) 206 else 200, (body.size - from).toLong())
            ex.responseBody.use { it.write(body, from, body.size - from) }
            sent[0] += body.size - from
        }
        s.start()
        server = s
        return URL("http://127.0.0.1:${s.address.port}/file") to sent
    }

    @Test
    fun downloadContinuesAPartialFile() {
        val body = Random(1).nextBytes(3 * mib)
        val (url, sent) = serve(body)
        val dst = File(tmp.root, "image.part")
        dst.writeBytes(body.copyOf(mib))
        Release.download(url, dst)
        assertArrayEquals(body, dst.readBytes())
        assertEquals(2 * mib, sent[0])
    }

    @Test
    fun aFileThatFailsItsChecksumIsFetchedAgain() {
        val body = Random(4).nextBytes(mib)
        val (url, sent) = serve(body, "${"0".repeat(64)}  file\n")
        val dst = File(tmp.root, "image.part")
        Release.download(url, dst)
        assertThrows(IOException::class.java) { Release.verify(URL("$url.sha256"), dst) }
        Release.download(url, dst)
        assertEquals(2 * mib, sent[0])
    }

    @Test
    fun unpackKeepsTheImage() {
        // Data, a zero block, data, and a zero tail that only the length keeps.
        val image = Random(2).nextBytes(mib) + ByteArray(mib) + Random(3).nextBytes(mib / 2) + ByteArray(mib)
        val gz = File(tmp.root, "image.gz")
        GZIPOutputStream(gz.outputStream()).use { it.write(image) }
        val dst = File(tmp.root, "disk.img")
        Release.unpack(gz, dst)
        assertEquals(image.size.toLong(), dst.length())
        assertArrayEquals(image, dst.readBytes())
    }
}
