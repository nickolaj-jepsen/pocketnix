package dev.nickolaj.pocketnix.agent

import org.json.JSONObject
import java.io.BufferedInputStream
import java.io.Closeable
import java.io.FilterInputStream
import java.io.InputStream
import java.io.OutputStream
import java.net.InetSocketAddress
import java.net.Socket

/** One connection to the agent. [close] must wake a thread blocked reading [input]. */
interface AgentStream : Closeable {
    val input: InputStream
    val output: OutputStream

    /** Signals end of input to the agent, keeping [input] readable. */
    fun shutdownOutput()
}

/** Opens connections to the agent: vsock on the phone, TCP to a desktop agent in debug builds. */
fun interface AgentDialer {
    fun dial(): AgentStream
}

/**
 * Dials and exchanges Hellos; returns the stream ready for its own traffic and
 * the agent's Hello. Its input is buffered from the first byte, so reading
 * lines and frames costs no syscall per byte.
 */
fun AgentDialer.open(hello: JSONObject): Pair<AgentStream, JSONObject> {
    val s = BufferedStream(dial())
    try {
        Protocol.writeJson(s.output, hello)
        return s to Protocol.readHello(s.input)
    } catch (e: Throwable) {
        s.close()
        throw e
    }
}

private class BufferedStream(s: AgentStream) : AgentStream by s {
    // BufferedInputStream asks available() after a short read, and an app may not FIONREAD a vsock fd.
    override val input: InputStream = BufferedInputStream(
        object : FilterInputStream(s.input) {
            override fun available() = 0
        },
        64 * 1024,
    )
}

class TcpDialer(private val host: String, private val port: Int) : AgentDialer {
    override fun dial(): AgentStream {
        val sock = Socket()
        sock.connect(InetSocketAddress(host, port), 5_000)
        sock.tcpNoDelay = true
        return SocketStream(sock)
    }
}

class SocketStream(private val sock: Socket) : AgentStream {
    override val input: InputStream = sock.getInputStream()
    override val output: OutputStream = sock.getOutputStream()
    override fun shutdownOutput() = runCatching { sock.shutdownOutput() }.let {}
    override fun close() = sock.close()
}
