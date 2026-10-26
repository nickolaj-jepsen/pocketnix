package dev.nickolaj.pocketnix.service

import dev.nickolaj.pocketnix.agent.Wake
import dev.nickolaj.pocketnix.agent.WakeHolder

/**
 * Who the phone stays awake for, or null to let it sleep: the agent's last
 * [wake], only while its stream is up (a hung guest's is dropped by the
 * keepalive), the VM runs and the phone's owner allows it.
 */
fun wakeHoldersOf(wake: Wake?, linkUp: Boolean, running: Boolean, allowed: Boolean): List<WakeHolder>? =
    wake?.takeIf { allowed && running && linkUp && it.held }?.holders

/** The service notification's line for [holders]. */
fun wakeText(holders: List<WakeHolder>): String =
    "Keeping the phone awake" + if (holders.isEmpty()) "" else ": " + holders.joinToString(", ") { h ->
        if (h.why.isEmpty()) h.who else "${h.who} — ${h.why}"
    }
