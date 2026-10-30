package dev.nickolaj.pocketnix.forward

import dev.nickolaj.pocketnix.agent.Forward

/**
 * The guest's offerable listeners (the agent's `port-listeners` snapshot) and
 * what the user made of each, for one guest boot: the agent restarts and
 * keeps no offer state, while the app lives exactly as long as the VM.
 *
 * Immutable: each change returns a new value. Times are milliseconds of a
 * monotonic clock, passed in.
 */
data class ForwardOffers(
    val offers: Map<Int, Offer> = emptyMap(),
    /** The last snapshot's ports, less module forwards'. */
    val listening: Set<Int> = emptySet(),
    /** The snapshot's `lan`: session forwards bind every address, not 127.0.0.1 (B3). */
    val lan: Boolean = false
) {
    enum class State { OFFERED, DISMISSED, ALLOWED }

    data class Offer(
        val port: Int,
        val state: State,
        /** An allowed forward's phone port. */
        val phonePort: Int? = null,
        /** When an allowed port's listener left; null while it listens. */
        val goneSince: Long? = null,
        /** Why the last Allow didn't get a phone port listening, for the re-offer. */
        val failure: String? = null
    )

    /** The posts and cancels that take the offer notifications from one value to the next. */
    data class Notices(val post: List<Offer>, val cancel: List<Int>)

    /**
     * Takes in a snapshot of [ports]. A new port is offered; an offered one
     * that left is withdrawn; a dismissed one stays dismissed. An allowed one
     * that left keeps its forward for [GRACE_MS], and one that came back in
     * time keeps it without asking again. A port that is now a module
     * forward's is dropped whatever its state (D78).
     */
    fun observe(ports: Collection<Int>, lan: Boolean, module: List<Forward>, now: Long): ForwardOffers {
        val moduleGuest = moduleGuestPorts(module)
        val up = ports.toSet() - moduleGuest
        val next = mutableMapOf<Int, Offer>()
        for ((port, o) in expire(now).offers) {
            if (port in moduleGuest) continue
            when (o.state) {
                State.OFFERED -> if (port in up) next[port] = o
                State.DISMISSED -> next[port] = o
                State.ALLOWED -> next[port] = o.copy(goneSince = if (port in up) null else o.goneSince ?: now)
            }
        }
        for (port in up) if (port !in next) next[port] = Offer(port, State.OFFERED)
        return ForwardOffers(next, up, lan)
    }

    /** Allows [port] on [phonePort]: a known port that listens and isn't allowed yet; anything else is unchanged. */
    fun allow(port: Int, phonePort: Int): ForwardOffers {
        val o = offers[port] ?: return this
        if (o.state == State.ALLOWED || port !in listening) return this
        return with(Offer(port, State.ALLOWED, phonePort = phonePort))
    }

    /** Dismisses an offered [port] for the rest of the boot. An allowed one has no manual revoke (D75). */
    fun dismiss(port: Int): ForwardOffers {
        val o = offers[port]?.takeIf { it.state == State.OFFERED } ?: return this
        return with(o.copy(state = State.DISMISSED, failure = null))
    }

    /** An Allow found no phone port: [port] stays offered, saying why. */
    fun allowFailed(port: Int, message: String): ForwardOffers {
        val o = offers[port]?.takeIf { it.state != State.ALLOWED } ?: return this
        return with(o.copy(state = State.OFFERED, failure = message))
    }

    /**
     * [port]'s forward couldn't listen on [phonePort] (another app took it
     * after the probe): it goes back to an offer that says why, and its
     * forward goes, so nothing retries it until the next Allow.
     */
    fun bindFailed(port: Int, phonePort: Int, message: String): ForwardOffers {
        val o = offers[port]?.takeIf { it.state == State.ALLOWED && it.phonePort == phonePort } ?: return this
        if (port !in listening) return copy(offers = offers - port)
        return with(Offer(port, State.OFFERED, failure = message))
    }

    /** Drops the allowed ports gone for [GRACE_MS] by [now]; a port that comes back later is offered anew. */
    fun expire(now: Long): ForwardOffers {
        val gone = offers.values.filter { o -> o.goneSince?.let { now - it >= GRACE_MS } == true }
        return if (gone.isEmpty()) this else copy(offers = offers - gone.map { it.port }.toSet())
    }

    /** When [expire] next drops something, or null. */
    fun nextExpiry(): Long? = offers.values.mapNotNull { it.goneSince }.minOrNull()?.plus(GRACE_MS)

    /**
     * The session forwards, for [PortForwards.apply] beside the [module]
     * ones: equal values across calls, so a re-apply keeps them listening.
     * None reaches a module forward's guest port.
     */
    fun forwards(module: List<Forward>): List<Forward> {
        val moduleGuest = moduleGuestPorts(module)
        return offers.values.filter { it.state == State.ALLOWED && it.port !in moduleGuest }.sortedBy { it.port }
            .map { Forward("offered ${it.port}", it.port, it.phonePort!!, lan) }
    }

    /** The phone ports a new session forward must not pick: [module] TCP forwards' and allowed ones'. */
    fun takenPhonePorts(module: List<Forward>): Set<Int> = module.filter { !it.udp }.map { it.hostPort }.toSet() + offers.values.mapNotNull { it.phonePort }

    /** The notifications to post (new offers, or ones with a new failure) and to cancel, going from [before] to this. */
    fun noticesSince(before: ForwardOffers): Notices {
        val post = offers.values.filter { o ->
            val b = before.offers[o.port]
            o.state == State.OFFERED && (b?.state != State.OFFERED || b.failure != o.failure)
        }
        val cancel = before.offers.values.filter { it.state == State.OFFERED && offers[it.port]?.state != State.OFFERED }.map { it.port }
        return Notices(post.sortedBy { it.port }, cancel.sorted())
    }

    /** One `port=<guest> state=<offered|dismissed|allowed> phone=<port|->` line per known port, ascending. */
    fun describe(): List<String> = offers.values.sortedBy { it.port }.map { "port=${it.port} state=${it.state.name.lowercase()} phone=${it.phonePort ?: "-"}" }

    private fun with(o: Offer) = copy(offers = offers + (o.port to o))

    companion object {
        /** How long an allowed port's forward outlives its listener. */
        const val GRACE_MS = 30_000L

        private fun moduleGuestPorts(module: List<Forward>) = module.filter { !it.udp }.map { it.guestPort }.toSet()
    }
}
