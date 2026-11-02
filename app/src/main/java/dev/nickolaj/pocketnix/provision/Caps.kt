package dev.nickolaj.pocketnix.provision

import dev.nickolaj.pocketnix.engine.Launched
import dev.nickolaj.pocketnix.engine.engineLine
import java.net.URLEncoder

/** What [ProvisionProvider]'s `caps` method reports about this phone's AVF and this app. */
data class Caps(
    /** `VirtualMachineManager.getCapabilities`, 0 when it failed as well as when it says none. */
    val capabilities: Int,
    /** The phone has a VirtualMachineManager. */
    val manager: Boolean,
    /** [dev.nickolaj.pocketnix.engine.AvfReflect.Probe.gpuFlagTypes]. */
    val gpuFlagTypes: List<Class<*>?>?,
    val rawConfigBinder: Boolean,
    val autoMemoryBalloon: Boolean,
    /** This process's last launch, null before one. */
    val launched: Launched?,
    val appVersion: String,
    /** The memory the settings give the VM at its next start. */
    val memoryMb: Int
)

/**
 * [caps] as the `caps` answer's key=value pairs. No value holds a comma or a
 * `}`, where `pocketnix`'s bundle parser ends a value: `engine` and
 * `appVersion` are URL-encoded.
 */
fun capsAnswer(caps: Caps): List<Pair<String, String>> = listOf(
    "capabilities" to caps.capabilities.toString(),
    "manager" to caps.manager.toString(),
    "gpuConfig" to gpuConfig(caps.gpuFlagTypes),
    "rawConfigBinder" to caps.rawConfigBinder.toString(),
    "autoMemoryBalloon" to caps.autoMemoryBalloon.toString(),
    "engine" to URLEncoder.encode(engineLine(caps.launched), "UTF-8"),
    "appVersion" to URLEncoder.encode(caps.appVersion, "UTF-8"),
    "memoryMb" to caps.memoryMb.toString()
)

/**
 * `caps`' gpuConfig for [types] ([Caps.gpuFlagTypes]): `absent` without a
 * GpuConfig, `boxed` or `primitive` when every renderer flag's setter takes
 * that Boolean, else `mixed` (they differ, or some are missing).
 */
fun gpuConfig(types: List<Class<*>?>?): String = when {
    types == null -> "absent"
    types.isNotEmpty() && types.all { it == Boolean::class.javaObjectType } -> "boxed"
    types.isNotEmpty() && types.all { it == Boolean::class.javaPrimitiveType } -> "primitive"
    else -> "mixed"
}
