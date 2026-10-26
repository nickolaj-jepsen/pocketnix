/*
 * Flow handling ported from Podroid's VsockUdpForwarder (GPL-2.0-or-later),
 * Copyright (C) 2024-2026 Podroid contributors.
 */
package dev.nickolaj.pocketnix.forward

import android.util.Log
import dev.nickolaj.pocketnix.agent.AgentDialer
import dev.nickolaj.pocketnix.agent.AgentStream
import dev.nickolaj.pocketnix.agent.Datagrams
import dev.nickolaj.pocketnix.agent.Forward
import dev.nickolaj.pocketnix.agent.Protocol
import dev.nickolaj.pocketnix.agent.open
import java.io.Closeable
import java.net.DatagramPacket
import java.net.DatagramSocket
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.SocketAddress
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.atomic.AtomicBoolean

/**
 * A UDP forward: one socket on the phone, and per client address ("flow") one
 * udp stream to the agent, which relays to the guest port. Flows end after
 * [idleMs] without a datagram either way, and at most [maxFlows] exist: UDP
 * sources are trivially forged on a LAN.
 */
internal class UdpForwarder(
    private val f: Forward,
    private val dialer: AgentDialer,
    private val appVersion: String,
    private val idleMs: Long = 120_000,
    private val maxFlows: Int = 64,
) : Closeable {
    private val socket = DatagramSocket(null).apply {
        // Unlike TCP's, a UDP SO_REUSEADDR would let two apps share the port.
        reuseAddress = false
        bind(InetSocketAddress(InetAddress.getByName(if (f.lan) "0.0.0.0" else "127.0.0.1"), f.hostPort))
    }
    private val flows = ConcurrentHashMap<SocketAddress, Flow>()
    @Volatile private var closed = false

    init {
        Thread(::receiveLoop, "udp-${f.hostPort}").apply { isDaemon = true }.start()
        Thread(::reapLoop, "udp-${f.hostPort}-reaper").apply { isDaemon = true }.start()
        Log.i(TAG, "forward ${f.name}: udp ${socket.localAddress.hostAddress}:${f.hostPort} → guest :${f.guestPort}")
    }

    private fun receiveLoop() {
        val buf = ByteArray(Datagrams.MAX_PAYLOAD)
        while (!closed) {
            val pkt = DatagramPacket(buf, buf.size)
            try {
                socket.receive(pkt)
            } catch (_: Exception) {
                if (closed) return
                continue
            }
            val src = pkt.socketAddress
            val flow = flows[src] ?: openFlow(src) ?: continue
            flow.send(pkt.data.copyOfRange(pkt.offset, pkt.offset + pkt.length))
        }
    }

    private fun openFlow(src: SocketAddress): Flow? {
        if (flows.size >= maxFlows) {
            Log.w(TAG, "forward ${f.name}: $maxFlows flows already; dropping $src")
            return null
        }
        // Only this thread adds flows, so no other can have raced it here.
        return Flow(src).also { flows[src] = it }
    }

    private fun reapLoop() {
        while (!closed) {
            try {
                Thread.sleep((idleMs / 4).coerceAtMost(REAP_INTERVAL_MS))
            } catch (_: InterruptedException) {
                return
            }
            val now = System.currentTimeMillis()
            flows.values.filter { now - it.lastActive > idleMs }.forEach { it.close() }
        }
    }

    override fun close() {
        closed = true
        runCatching { socket.close() }
        flows.values.toList().forEach { it.close() }
    }

    /**
     * One client address's stream. Datagrams queue while it dials, so a slow
     * dial delays no other flow; past [QUEUE] waiting, they drop, as UDP may.
     */
    private inner class Flow(private val src: SocketAddress) {
        @Volatile var lastActive = System.currentTimeMillis()
        private val queue = LinkedBlockingQueue<ByteArray>(QUEUE)
        @Volatile private var stream: AgentStream? = null
        private val ended = AtomicBoolean(false)
        private val done get() = ended.get()
        private val writer = Thread(::run, "udp-${f.hostPort}-flow").apply { isDaemon = true }

        init {
            writer.start()
        }

        fun send(payload: ByteArray) {
            lastActive = System.currentTimeMillis()
            queue.offer(payload)
        }

        private fun run() {
            try {
                val s = dialer.open(Protocol.hello("udp", appVersion) { put("port", f.guestPort) }).first
                stream = s
                // Closed while dialing: close() found no stream to close.
                if (done) {
                    runCatching { s.close() }
                    return
                }
                Thread({ replies(s) }, "udp-${f.hostPort}-replies").apply { isDaemon = true }.start()
                while (!done) {
                    val p = queue.take()
                    s.output.write(Datagrams.encode(p))
                    s.output.flush()
                }
            } catch (_: InterruptedException) {
            } catch (e: Exception) {
                if (!done) Log.w(TAG, "forward ${f.name}: $src: ${e.message}")
            } finally {
                close()
            }
        }

        private fun replies(s: AgentStream) {
            val buf = ByteArray(Datagrams.MAX_PAYLOAD)
            try {
                while (true) {
                    val n = Datagrams.read(s.input, buf)
                    if (n < 0) break
                    lastActive = System.currentTimeMillis()
                    socket.send(DatagramPacket(buf, n, src))
                }
            } catch (_: Exception) {
                // The stream ended (closed, reaped, or the agent went away).
            } finally {
                close()
            }
        }

        fun close() {
            if (!ended.compareAndSet(false, true)) return
            flows.remove(src, this)
            writer.interrupt()
            stream?.let { runCatching { it.close() } }
        }
    }

    private companion object {
        const val TAG = "PortForwards"
        const val QUEUE = 64
        const val REAP_INTERVAL_MS = 15_000L
    }
}
