package dev.nickolaj.pocketnix

import dev.nickolaj.pocketnix.agent.ControlChannel
import dev.nickolaj.pocketnix.engine.VmState

/** The VM as every status surface tells it: the engine's state, the agent link and the app's own flags in one. */
sealed interface VmPhase {
    data object Stopped : VmPhase

    /** The engine boots the guest, or boots it again after a guest reboot. */
    data object Starting : VmPhase

    /** The guest runs and its agent hasn't answered yet. */
    data object Booting : VmPhase

    /** The agent answered; [host] is the guest's hostname, null until its config arrives or when it has none. */
    data class Running(val host: String?) : VmPhase

    /** Booting for longer than an agent takes: maybe a generation that won't boot. */
    data object AgentLate : VmPhase

    /** The agent answered but refused this app (e.g. a protocol mismatch); retrying won't help. */
    data class Refused(val reason: String) : VmPhase

    /** A stop was asked for and the guest isn't down yet. */
    data object Stopping : VmPhase

    data class Failed(val message: String) : VmPhase

    /** The guest neither runs nor is about to. */
    val off: Boolean get() = this == Stopped || this is Failed
}

/**
 * The phase of a VM in [state] with agent [link]. [stopping] wins over
 * everything; [state] is read before [link], since a link stays Up or Refused
 * for a while after the guest reboots. [agentLate] only matters while the
 * link is Down. An empty [hostname] counts as none.
 */
fun vmPhase(state: VmState, link: ControlChannel.Link, stopping: Boolean, agentLate: Boolean, hostname: String?): VmPhase = when {
    stopping -> VmPhase.Stopping
    state == VmState.Stopped -> VmPhase.Stopped
    state is VmState.Failed -> VmPhase.Failed(state.message)
    state == VmState.Starting -> VmPhase.Starting
    link is ControlChannel.Link.Up -> VmPhase.Running(hostname?.takeIf { it.isNotEmpty() })
    link is ControlChannel.Link.Refused -> VmPhase.Refused(link.reason)
    agentLate -> VmPhase.AgentLate
    else -> VmPhase.Booting
}
