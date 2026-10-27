package dev.nickolaj.pocketnix.engine

import dev.nickolaj.pocketnix.agent.AgentDialer
import kotlinx.coroutines.flow.StateFlow

sealed interface VmState {
    data object Stopped : VmState
    data object Starting : VmState
    /** The hypervisor runs the guest; the agent may not be up yet. */
    data object Running : VmState
    data class Failed(val message: String) : VmState
}

/** The set-once hardware the app UI owns (design.md, Configuration). */
data class VmSettings(
    val memoryMb: Int,
    /** 1, or 0 for one vCPU per host core. */
    val cpus: Int,
    val diskGb: Int,
    val network: Boolean,
    /** A name AvfEngine.gpuSpec knows; empty means the default (2d when networked). */
    val gpu: String = "",
)

/** What runs the guest: AVF on the phone, or (debug builds) an agent on another machine. */
interface VmEngine {
    val state: StateFlow<VmState>

    /** Dials the guest agent; non-null while [state] is Running. */
    val dialer: AgentDialer?

    /** False for an agent the app doesn't own, which a stop must not power off. */
    val ownsGuest: Boolean

    /** Boots the guest with [settings], read at every launch (a reboot's too); the hvc0 console goes to [console]. */
    fun start(settings: () -> VmSettings, console: ConsoleLog)

    /** Stops the VM at once, without asking the guest. */
    fun forceStop()
}
