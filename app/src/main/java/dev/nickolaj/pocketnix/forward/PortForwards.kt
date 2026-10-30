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
 * The port forwards, the module's and those allowed from [ForwardOffers]: a
 * listener on the phone per forward, each accepted connection relayed to the
 * guest over its own forward stream, and each UDP client address over its own
 * udp stream.
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

    /** The TCP forwards whose phone port is listening now: a failed one's port belongs to another app. */
    @Synchronized
    fun listening(): List<Forward> = active.keys.filter { !it.udp }

    /** Why [f], given to the last [apply], isn't listening; null when it is. */
    @Synchronized
    fun failure(f: Forward): String? = failed[f]

    private inner class Listener(private val f: Forward, private val dialer: AgentDialer) : Closeable {
        private val server = ServerSocket(f.hostPort, 16, bindAddress(f.lan))

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

/** Where a forward's phone listener binds: 127.0.0.1, or every address with [lan]. */
fun bindAddress(lan: Boolean): InetAddress = InetAddress.getByName(if (lan) "0.0.0.0" else "127.0.0.1")

/**
 * The phone port for a forward of [guestPort]: [guestPort] itself when it is
 * free on [bindAddress], else the next free one above it, never one in
 * [taken] or below [MIN_PHONE_PORT]. Null when none is free. Another app can
 * still take the port before the forward binds it.
 */
fun pickPhonePort(guestPort: Int, lan: Boolean, taken: Set<Int>): Int? {
    val address = bindAddress(lan)
    return (maxOf(guestPort, MIN_PHONE_PORT)..MAX_PORT).firstOrNull { port ->
        port !in taken && runCatching { ServerSocket(port, 1, address).close() }.isSuccess
    }
}

/** Android lets an app listen only from here up. */
const val MIN_PHONE_PORT = 1024
private const val MAX_PORT = 65535

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
