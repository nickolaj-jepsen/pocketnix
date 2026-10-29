package dev.nickolaj.pocketnix.agent

import java.io.Closeable
import java.io.InputStream
import java.io.OutputStream
import java.net.ServerSocket
import java.util.concurrent.atomic.AtomicInteger
import org.json.JSONObject

/** One accepted connection after its Hello: the app's Hello, and the raw streams. */
class FakeConn(val hello: JSONObject, val input: InputStream, val output: OutputStream) {
    fun send(line: String) {
        output.write((line + "\n").toByteArray())
        output.flush()
    }

    fun send(msg: JSONObject) = send(msg.toString())

    fun readJson(): JSONObject? = runCatching { JSONObject(Protocol.readLine(input)) }.getOrNull()
}

/** A TCP stand-in for the agent: answers every Hello, then hands the connection to [serve]. */
class FakeAgent(private val serve: (FakeConn) -> Unit) : Closeable {
    private val server = ServerSocket(0)
    val connections = AtomicInteger()
    val dialer = TcpDialer("127.0.0.1", server.localPort)

    init {
        Thread {
            while (true) {
                val sock = runCatching { server.accept() }.getOrNull() ?: break
                connections.incrementAndGet()
                Thread {
                    sock.use {
                        runCatching {
                            val input = sock.getInputStream()
                            val hello = JSONObject(Protocol.readLine(input))
                            Protocol.writeJson(sock.getOutputStream(), JSONObject().put("type", "hello").put("version", 1).put("agent", "fake"))
                            serve(FakeConn(hello, input, sock.getOutputStream()))
                        }
                    }
                }.apply { isDaemon = true }.start()
            }
        }.apply { isDaemon = true }.start()
    }

    override fun close() = server.close()
}
