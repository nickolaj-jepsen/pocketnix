/*
 * Ported from Podroid (GPL-2.0-or-later), Copyright (C) 2024-2026 Podroid contributors.
 */
package dev.nickolaj.pocketnix.engine

import android.content.Context
import android.os.ParcelFileDescriptor
import android.util.Log
import java.io.InputStream
import java.io.OutputStream
import java.lang.reflect.InvocationHandler
import java.lang.reflect.Proxy
import java.util.concurrent.Executor

/**
 * Reflective wrappers over android.system.virtualmachine: the API is
 * @SystemApi and VirtualMachineCustomImageConfig is @hide, so the app reaches
 * them through HiddenApiBypass (lifted at startup) instead of SDK stubs.
 * Handles stay untyped (`Any`) and go back through this object.
 */
object AvfReflect {
    private const val TAG = "AvfReflect"
    private const val PKG = "android.system.virtualmachine"
    private val MGR by lazy { Class.forName("$PKG.VirtualMachineManager") }
    private val CFG by lazy { Class.forName("$PKG.VirtualMachineConfig") }
    private val CFG_B by lazy { Class.forName("$PKG.VirtualMachineConfig\$Builder") }
    private val CUSTOM by lazy { Class.forName("$PKG.VirtualMachineCustomImageConfig") }
    private val CUSTOM_B by lazy { Class.forName("$PKG.VirtualMachineCustomImageConfig\$Builder") }
    private val DISK by lazy { Class.forName("$PKG.VirtualMachineCustomImageConfig\$Disk") }
    private val GPU by lazy { runCatching { Class.forName("$PKG.VirtualMachineCustomImageConfig\$GpuConfig") }.getOrNull() }
    private val GPU_B by lazy { runCatching { Class.forName("$PKG.VirtualMachineCustomImageConfig\$GpuConfig\$Builder") }.getOrNull() }

    // VirtualMachineConfig and VirtualMachineManager constants (stable SystemApi values).
    const val CPU_TOPOLOGY_ONE_CPU = 0
    const val CPU_TOPOLOGY_MATCH_HOST = 1
    const val DEBUG_LEVEL_FULL = 1
    const val CAPABILITY_NON_PROTECTED_VM = 2

    fun manager(ctx: Context): Any = Context::class.java.getMethod("getSystemService", Class::class.java).invoke(ctx.applicationContext, MGR)
        ?: error("no VirtualMachineManager on this device")

    fun capabilities(mgr: Any): Int = runCatching { invoke(mgr, "getCapabilities") as? Int ?: 0 }.getOrDefault(0)

    fun getOrCreate(mgr: Any, name: String, cfg: Any): Any = invoke(mgr, "getOrCreate", String::class.java to name, CFG to cfg) ?: error("getOrCreate returned null")

    fun create(mgr: Any, name: String, cfg: Any): Any = invoke(mgr, "create", String::class.java to name, CFG to cfg) ?: error("create returned null")

    fun delete(mgr: Any, name: String) {
        runCatching { invoke(mgr, "delete", String::class.java to name) }
    }

    /** Replaces an existing VM's config; throws when AVF finds it incompatible. */
    fun setConfig(vm: Any, cfg: Any) {
        // Declared on the runtime class on some builds, inherited on others.
        val m = runCatching { vm.javaClass.getDeclaredMethod("setConfig", CFG) }.getOrNull()
            ?: vm.javaClass.getMethod("setConfig", CFG)
        m.isAccessible = true
        m.invoke(vm, cfg)
    }

    fun newVmConfigBuilder(ctx: Context): Any = CFG_B.getDeclaredConstructor(Context::class.java).apply { isAccessible = true }.newInstance(ctx)

    fun newCustomBuilder(): Any = CUSTOM_B.getDeclaredConstructor().apply { isAccessible = true }.newInstance()

    fun setProtectedVm(b: Any, value: Boolean) = invoke(b, "setProtectedVm", Boolean::class.javaPrimitiveType!! to value).let {}
    fun setMemoryBytes(b: Any, bytes: Long) = invoke(b, "setMemoryBytes", Long::class.javaPrimitiveType!! to bytes).let {}
    fun setCpuTopology(b: Any, topology: Int) = invoke(b, "setCpuTopology", Int::class.javaPrimitiveType!! to topology).let {}
    fun setDebugLevel(b: Any, level: Int) = invoke(b, "setDebugLevel", Int::class.javaPrimitiveType!! to level).let {}
    fun setConsoleInputDevice(b: Any, device: String) = invoke(b, "setConsoleInputDevice", String::class.java to device).let {}
    fun setVmOutputCaptured(b: Any, value: Boolean) = invoke(b, "setVmOutputCaptured", Boolean::class.javaPrimitiveType!! to value).let {}

    /** false keeps crosvm from inheriting the activity's fds (SurfaceFlinger fences → SELinux denials). */
    fun setConnectVmConsole(b: Any, value: Boolean) = invoke(b, "setConnectVmConsole", Boolean::class.javaPrimitiveType!! to value).let {}

    fun setVmConsoleInputSupported(b: Any, value: Boolean) {
        // Absent on older revisions.
        runCatching { invoke(b, "setVmConsoleInputSupported", Boolean::class.javaPrimitiveType!! to value) }
    }

    /**
     * A virtio-balloon with free page reporting, plus AVF's own inflation on
     * onTrimMemory. virtmgr still drops it unless the device sets
     * `hypervisor.memory_reclaim.supported`. Returns whether the API has it.
     */
    fun useAutoMemoryBalloon(b: Any, value: Boolean): Boolean = runCatching { invoke(b, "useAutoMemoryBalloon", Boolean::class.javaPrimitiveType!! to value) }
        .onFailure { Log.w(TAG, "no useAutoMemoryBalloon on this AVF API") }
        .isSuccess

    fun setCustomImageConfig(b: Any, cfg: Any) = invoke(b, "setCustomImageConfig", CUSTOM to cfg).let {}
    fun build(b: Any): Any = invoke(b, "build")!!

    fun setName(b: Any, name: String) = invoke(b, "setName", String::class.java to name).let {}
    fun setBootloaderPath(b: Any, p: String) = invoke(b, "setBootloaderPath", String::class.java to p).let {}

    fun addDisk(b: Any, path: String, writable: Boolean) {
        val factory = DISK.getDeclaredMethod(if (writable) "RWDisk" else "RODisk", String::class.java).apply { isAccessible = true }
        val disk = factory.invoke(null, path) ?: error("Disk($path) returned null")
        b.javaClass.getDeclaredMethod("addDisk", DISK).apply { isAccessible = true }.invoke(b, disk)
    }

    fun setNetworkSupported(b: Any, value: Boolean) {
        val ok = runCatching { invoke(b, "useNetwork", Boolean::class.javaPrimitiveType!! to value) }.isSuccess ||
            runCatching { invoke(b, "setNetworkSupported", Boolean::class.javaPrimitiveType!! to value) }.isSuccess
        if (!ok) Log.w(TAG, "no useNetwork/setNetworkSupported on this AVF API")
    }

    /**
     * Declares a GPU backend for the guest, and returns whether it took effect.
     *
     * "2d" is what a networked VM asks for: stock Pixel virtmgr runs GPU-less
     * VMs on crosvm_minimal, which lacks `--net` and aborts a networked VM.
     * Absent GpuConfig classes mean a revision that runs full crosvm anyway.
     */
    fun setGpuConfig(b: Any, backend: String): Boolean = runCatching {
        val gpuB = (GPU_B ?: return true).getDeclaredConstructor().apply { isAccessible = true }.newInstance()
        if (!set(gpuB, "setBackend", backend)) error("GpuConfig.Builder.setBackend($backend) is missing or failed")
        val cfg = invoke(gpuB, "build") ?: error("GpuConfig.Builder.build() returned null")
        invoke(b, "setGpuConfig", GPU!! to cfg)
        Log.i(TAG, "gpu config: $backend")
        true
    }.getOrElse {
        Log.w(TAG, "setGpuConfig($backend) failed; virtmgr may pick crosvm_minimal (no virtio-net)", it)
        false
    }

    /**
     * A speaker and no microphone (the microphone needs RECORD_AUDIO). virtmgr
     * then runs `crosvm_vu_snd` with the AAudio backend; fixed at VM start.
     * False when this revision has no AudioConfig or refuses it.
     */
    fun setAudioConfig(b: Any): Boolean = runCatching {
        val cfgCls = Class.forName("$PKG.VirtualMachineCustomImageConfig\$AudioConfig")
        val audioB = Class.forName("$PKG.VirtualMachineCustomImageConfig\$AudioConfig\$Builder")
            .getDeclaredConstructor().apply { isAccessible = true }.newInstance()
        if (!set(audioB, "setUseSpeaker", true)) error("AudioConfig.Builder.setUseSpeaker is missing or failed")
        set(audioB, "setUseMicrophone", false)
        val cfg = invoke(audioB, "build") ?: error("AudioConfig.Builder.build() returned null")
        invoke(b, "setAudioConfig", cfgCls to cfg)
        Log.i(TAG, "audio config: speaker")
        true
    }.getOrElse {
        Log.w(TAG, "setAudioConfig failed; the guest gets no sound device", it)
        false
    }

    /** A VirtualMachineCallback proxy; the interface is @SystemApi, so it can't be implemented directly. */
    fun newVmCallback(onError: (Int, String?) -> Unit, onStopped: (Int) -> Unit): Any {
        val cls = Class.forName("$PKG.VirtualMachineCallback")
        val handler = InvocationHandler { _, method, args ->
            when (method.name) {
                // onError(vm, errorCode, message)
                "onError" -> onError(args?.getOrNull(1) as? Int ?: -1, args?.getOrNull(2) as? String)

                // onStopped(vm, reason); onDied has the reason at index 1 too.
                "onStopped", "onDied" -> onStopped(args?.getOrNull(1) as? Int ?: -1)
            }
            null
        }
        return Proxy.newProxyInstance(cls.classLoader, arrayOf(cls), handler)
    }

    fun setCallback(vm: Any, executor: Executor, callback: Any) {
        val cbCls = Class.forName("$PKG.VirtualMachineCallback")
        vm.javaClass.getDeclaredMethod("setCallback", Executor::class.java, cbCls).apply { isAccessible = true }
            .invoke(vm, executor, callback)
    }

    fun run(vm: Any) = invoke(vm, "run").let {}

    /** Asks the framework to kill the VM; throws when it refuses. */
    fun stop(vm: Any) = invoke(vm, "stop").let {}

    /** A connected AF_VSOCK socket to [port] on the guest (host → guest only). */
    fun connectVsock(vm: Any, port: Long): ParcelFileDescriptor = vm.javaClass.getDeclaredMethod("connectVsock", Long::class.javaPrimitiveType!!).apply { isAccessible = true }
        .invoke(vm, port) as? ParcelFileDescriptor ?: error("connectVsock($port) returned null")

    fun consoleOutput(vm: Any): InputStream = invoke(vm, "getConsoleOutput") as? InputStream ?: error("no console output")
    fun consoleInput(vm: Any): OutputStream = invoke(vm, "getConsoleInput") as? OutputStream ?: error("no console input")

    /**
     * Calls [name] with [value], tolerating the parameter being boxed or a
     * primitive: AVF's config builders take `Boolean` on GrapheneOS 17 and
     * `boolean` on other revisions. False when no such method exists or it
     * throws, so one optional setter can't sink the rest of the config.
     */
    private fun set(target: Any, name: String, value: Any): Boolean {
        val types = when (value) {
            is Boolean -> listOf(Boolean::class.javaObjectType, Boolean::class.javaPrimitiveType!!)
            is String -> listOf(String::class.java)
            else -> emptyList()
        }
        for (t in types) {
            val m = runCatching { target.javaClass.getDeclaredMethod(name, t) }.getOrNull() ?: continue
            return runCatching {
                m.isAccessible = true
                m.invoke(target, value)
            }.onFailure { Log.w(TAG, "$name($value) failed", it) }.isSuccess
        }
        return false
    }

    private fun invoke(target: Any, name: String, vararg typedArgs: Pair<Class<*>, Any?>): Any? {
        val m = target.javaClass.getDeclaredMethod(name, *typedArgs.map { it.first }.toTypedArray()).apply { isAccessible = true }
        return m.invoke(target, *typedArgs.map { it.second }.toTypedArray())
    }
}
