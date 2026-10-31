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

/** The set-once hardware the app UI owns (docs/configuration.md). */
data class VmSettings(
    val memoryMb: Int,
    /** 1, or 0 for one vCPU per host core. */
    val cpus: Int,
    val diskGb: Int,
    val network: Boolean,
    /** The phone's GPU through gfxstream, where AVF offers it. */
    val gpu: Boolean = false,
    /** A virtio-snd speaker (no microphone), played through AAudio. */
    val audio: Boolean = false,
    /**
     * Guest RAM in the app's memory ([dev.nickolaj.pocketnix.gui.GuestRam]).
     * AVF's sound device can't run on it, so [audio] wins.
     */
    val guestRam: Boolean = true
)

/** What a launch gave the VM, which may be less than its [VmSettings] asked for. */
data class Launched(
    /** The VM's GPU: `gfxstream`, `2d` or `none`. */
    val gpu: String,
    /** [VmSettings.gpu]. */
    val gpuAsked: Boolean,
    /** A GPU launch failed or ended before its agent answered, so this process's later launches go without. */
    val gpuRefused: Boolean,
    /** The VM runs on guest RAM the app maps ([VmSettings.guestRam]). */
    val guestRam: Boolean,
    /** [VmSettings.audio]'s virtio-snd. */
    val virtioSnd: Boolean
)

/** The debug dump's line for [launched], null before a launch or for an engine that boots nothing. */
fun engineLine(launched: Launched?): String = launched?.run {
    "engine gpu=$gpu gpuAsked=$gpuAsked gpuRefused=$gpuRefused guestRam=$guestRam virtioSnd=$virtioSnd"
} ?: "engine launched=none"

/** What runs the guest: AVF on the phone, or (debug builds) an agent on another machine. */
interface VmEngine {
    val state: StateFlow<VmState>

    /** Dials the guest agent; non-null while [state] is Running. */
    val dialer: AgentDialer?

    /** False for an agent the app doesn't own, which a stop must not power off. */
    val ownsGuest: Boolean

    /** Boots the guest with [settings], read at every launch (a reboot's too); the hvc0 console goes to [console]. */
    fun start(settings: () -> VmSettings, console: ConsoleLog)

    /** The last launch's, null before one. */
    val launched: Launched? get() = null

    /** Stops the VM at once, without asking the guest. */
    fun forceStop()
}
