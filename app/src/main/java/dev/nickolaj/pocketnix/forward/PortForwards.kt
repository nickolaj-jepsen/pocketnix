package dev.nickolaj.pocketnix.forward

import android.util.Log
import dev.nickolaj.pocketnix.agent.AgentDialer
import dev.nickolaj.pocketnix.agent.Forward
import dev.nickolaj.pocketnix.agent.Protocol
import dev.nickolaj.pocketnix.agent.open
import java.io.Closeable
import java.io.InputStream
import java.io.OutputStream
import java.net.InetAddress
import java.net.ServerSocket
import java.net.Socket
import java.util.concurrent.Semaphore

/**
 * The module's port forwards: a listener on the phone per forward, each
 * accepted connection relayed to the guest over its own forward stream, and
 * each UDP client address over its own udp stream.
 */
class PortForwards(private val appVersion: String) {
    private val active = mutableMapOf<Forward, Closeable>()
    private val failed = mutableMapOf<Forward, String>()

    /**
     * Makes the listeners match [forwards]; an empty list (or null dialer)
     * closes them all. Returns why each forward that isn't listening failed,
     * e.g. a port another app holds: the phone's ports are shared.
     */
    @Synchronized
    fun apply(forwards: List<Forward>, dialer: AgentDialer?): List<String> {
        val want = if (dialer == null) emptySet() else forwards.toSet()
        for (f in active.keys - want) active.remove(f)?.close()
        failed.keys.retainAll(want)
        for (f in want - active.keys) {
            runCatching { if (f.udp) UdpForwarder(f, dialer!!, appVersion) else Listener(f, dialer!!) }
                .onSuccess {
                    active[f] = it
                    failed.remove(f)
                }
                .onFailure {
                    val port = if (f.udp) "udp port ${f.hostPort}" else "port ${f.hostPort}"
                    Log.w(TAG, "forward ${f.name} on $port: ${it.message}")
                    failed[f] = "forward ${f.name}: $port unavailable (${it.message})"
                }
        }
        return failed.values.toList()
    }

    private inner class Listener(private val f: Forward, private val dialer: AgentDialer) : Closeable {
        private val server = ServerSocket(f.hostPort, 16, InetAddress.getByName(if (f.lan) "0.0.0.0" else "127.0.0.1"))

        // Bounds threads and guest streams under a connection flood.
        private val slots = Semaphore(MAX_CONNECTIONS)

        init {
            server.reuseAddress = true
            Thread(::acceptLoop, "forward-${f.hostPort}").apply { isDaemon = true }.start()
            Log.i(TAG, "forward ${f.name}: ${server.inetAddress.hostAddress}:${f.hostPort} → guest :${f.guestPort}")
        }

        private fun acceptLoop() {
            while (true) {
                val client = try {
                    server.accept()
                } catch (_: Exception) {
                    return
                }
                if (!slots.tryAcquire()) {
                    client.close()
                    continue
                }
                Thread({
                    try {
                        relay(client)
                    } catch (e: Exception) {
                        // A client that reset before we got to it.
                        Log.w(TAG, "forward ${f.name}: ${e.message}")
                        runCatching { client.close() }
                    } finally {
                        slots.release()
                    }
                }, "forward-${f.hostPort}-conn").apply { isDaemon = true }.start()
            }
        }

        private fun relay(client: Socket) {
            val guest = try {
                dialer.open(Protocol.hello("forward", appVersion) { put("port", f.guestPort) }).first
            } catch (e: Exception) {
                Log.w(TAG, "forward ${f.name}: ${e.message}")
                client.close()
                return
            }
            client.tcpNoDelay = true
            val up = Thread({ copy(client.getInputStream(), guest.output) { guest.shutdownOutput() } }, "forward-up")
            up.isDaemon = true
            up.start()
            copy(guest.input, client.getOutputStream()) { runCatching { client.shutdownOutput() } }
            // Once the guest side is done, upload left in flight gets a moment to land.
            up.join(1_000)
            runCatching { client.close() }
            runCatching { guest.close() }
        }

        override fun close() {
            runCatching { server.close() }
        }
    }

    private companion object {
        const val TAG = "PortForwards"
        const val MAX_CONNECTIONS = 64
    }
}

/** Copies until EOF, then half-closes the far side with [onEof]; a failure ends both. */
private fun copy(from: InputStream, to: OutputStream, onEof: () -> Unit) {
    val buf = ByteArray(32 * 1024)
    try {
        while (true) {
            val n = from.read(buf)
            if (n < 0) break
            to.write(buf, 0, n)
            to.flush()
        }
        onEof()
    } catch (_: Exception) {
        runCatching { from.close() }
        runCatching { to.close() }
    }
}
