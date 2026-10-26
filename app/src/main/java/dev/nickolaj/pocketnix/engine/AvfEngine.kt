/*
 * Lifecycle and config building ported from Podroid's AvfEngine (GPL-2.0-or-later),
 * Copyright (C) 2024-2026 Podroid contributors.
 */
package dev.nickolaj.pocketnix.engine

import android.content.Context
import android.os.ParcelFileDescriptor
import android.system.Os
import android.system.OsConstants
import android.util.Log
import dev.nickolaj.pocketnix.agent.AgentDialer
import dev.nickolaj.pocketnix.agent.AgentStream
import dev.nickolaj.pocketnix.agent.Protocol
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import java.io.File
import java.io.InputStream
import java.io.OutputStream
import java.io.RandomAccessFile
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit

/**
 * Boots the app's disk image under AVF: AVF's u-boot finds the image's ESP
 * and runs its systemd-boot, so the kernel and command line are the image's.
 */
class AvfEngine(private val context: Context) : VmEngine {
    private val _state = MutableStateFlow<VmState>(VmState.Stopped)
    override val state: StateFlow<VmState> = _state.asStateFlow()
    override val ownsGuest = true

    // Lifecycle work and VM callbacks run here, one at a time.
    private val lifecycle = Executors.newSingleThreadScheduledExecutor { r -> Thread(r, "avf-lifecycle") }

    @Volatile private var vm: Any? = null
    // Bumped per launch so a previous VM's late callback can't touch the current one.
    private var generation = 0L
    @Volatile private var stopRequested = false
    private var consoleStreams: Pair<InputStream, OutputStream>? = null
    private var lastError: String? = null

    override val dialer: AgentDialer?
        get() = vm?.let { v ->
            AgentDialer {
                // Refused until the guest's agent listens; that's an IOException, not a crash.
                val pfd = try {
                    AvfReflect.connectVsock(v, Protocol.PORT)
                } catch (e: java.lang.reflect.InvocationTargetException) {
                    throw java.io.IOException("connectVsock: ${e.cause?.message}", e.cause)
                }
                VsockStream(pfd)
            }
        }

    override fun start(settings: VmSettings, console: ConsoleLog) {
        // Claimed here, not on the lifecycle thread: the foreground service
        // starting alongside stops itself if it still sees Stopped.
        synchronized(this) {
            if (_state.value == VmState.Starting || _state.value == VmState.Running) return
            stopRequested = false
            _state.value = VmState.Starting
        }
        lifecycle.execute { launch(settings, console) }
    }

    private fun launch(settings: VmSettings, console: ConsoleLog) {
        val gen = ++generation
        lastError = null
        try {
            val disk = DiskImage.file(context)
            if (!disk.exists()) throw IllegalStateException("No disk image yet: run `pocketnix provision`")
            DiskImage.grow(disk, settings.diskGb)

            val mgr = AvfReflect.manager(context)
            val caps = AvfReflect.capabilities(mgr)
            if (caps != 0 && caps and AvfReflect.CAPABILITY_NON_PROTECTED_VM == 0) {
                throw IllegalStateException("This device's hypervisor only runs protected VMs")
            }
            val config = buildConfig(disk, settings)
            val v = runCatching { AvfReflect.getOrCreate(mgr, VM_NAME, config).also { AvfReflect.setConfig(it, config) } }
                .getOrElse {
                    Log.w(TAG, "stored VM config incompatible (${it.message}); recreating")
                    AvfReflect.delete(mgr, VM_NAME)
                    AvfReflect.create(mgr, VM_NAME, config)
                }
            AvfReflect.setCallback(
                v, lifecycle,
                AvfReflect.newVmCallback(
                    onError = { code, msg ->
                        Log.e(TAG, "VM error $code: $msg")
                        lastError = "AVF error $code: ${msg ?: "no message"}"
                    },
                    onStopped = { reason -> onStopped(gen, reason, settings, console) },
                ),
            )

            val out = AvfReflect.consoleOutput(v)
            val inp = AvfReflect.consoleInput(v)
            consoleStreams = out to inp
            console.input = inp
            Thread({ pump(out, console) }, "avf-console").apply { isDaemon = true }.start()

            if (stopRequested) {
                cleanup(console)
                _state.value = VmState.Stopped
                return
            }
            AvfReflect.run(v)
            vm = v
            _state.value = VmState.Running
            Log.i(TAG, "VM running (${settings.memoryMb} MB, cpus=${settings.cpus}, network=${settings.network})")
        } catch (e: Throwable) {
            val cause = (e as? java.lang.reflect.InvocationTargetException)?.cause ?: e
            Log.e(TAG, "VM start failed", cause)
            cleanup(console)
            _state.value = VmState.Failed(cause.message ?: cause.javaClass.simpleName)
        }
    }

    private fun buildConfig(disk: File, settings: VmSettings): Any {
        val custom = AvfReflect.newCustomBuilder()
        AvfReflect.setName(custom, VM_NAME)
        AvfReflect.setBootloaderPath(custom, UBOOT)
        AvfReflect.addDisk(custom, disk.absolutePath, writable = true)
        AvfReflect.setNetworkSupported(custom, settings.network)
        // Only the networked VM needs full crosvm (see setGpuConfig).
        if (settings.network) AvfReflect.setGpuConfig(custom)

        val b = AvfReflect.newVmConfigBuilder(context)
        AvfReflect.setProtectedVm(b, false)
        AvfReflect.setMemoryBytes(b, settings.memoryMb.toLong() shl 20)
        AvfReflect.setCpuTopology(b, if (settings.cpus == 1) AvfReflect.CPU_TOPOLOGY_ONE_CPU else AvfReflect.CPU_TOPOLOGY_MATCH_HOST)
        // Console input needs the full debug level.
        AvfReflect.setDebugLevel(b, AvfReflect.DEBUG_LEVEL_FULL)
        // virtio-console runs at memory speed; the PL011 UART manages ~14 KB/s.
        AvfReflect.setConsoleInputDevice(b, "hvc0")
        AvfReflect.setConnectVmConsole(b, false)
        AvfReflect.setVmOutputCaptured(b, true)
        AvfReflect.setVmConsoleInputSupported(b, true)
        AvfReflect.setCustomImageConfig(b, AvfReflect.build(custom))
        return AvfReflect.build(b)
    }

    private fun pump(out: InputStream, console: ConsoleLog) {
        val buf = ByteArray(16 * 1024)
        try {
            while (true) {
                val n = out.read(buf)
                if (n < 0) break
                console.append(buf, 0, n)
            }
        } catch (_: Exception) {
            // The VM stopped and its console closed.
        }
    }

    // On the lifecycle thread (the callback executor).
    private fun onStopped(gen: Long, reason: Int, settings: VmSettings, console: ConsoleLog) {
        if (gen != generation) return
        Log.i(TAG, "VM stopped: ${AvfReasons.stopReason(reason)}")
        cleanup(console)
        when {
            // A guest `reboot` (e.g. into a new generation) stops crosvm; boot it again.
            reason == AvfReasons.STOP_REASON_REBOOT && !stopRequested -> {
                console.append("\r\n[pocketnix: the guest rebooted; starting it again]\r\n")
                _state.value = VmState.Starting
                launch(settings, console)
            }
            stopRequested || reason == AvfReasons.STOP_REASON_SHUTDOWN -> _state.value = VmState.Stopped
            else -> _state.value = VmState.Failed(lastError ?: "The VM stopped: ${AvfReasons.stopReason(reason)}")
        }
    }

    override fun forceStop() {
        stopRequested = true
        lifecycle.execute {
            val v = vm
            if (v == null) {
                if (_state.value != VmState.Starting) _state.value = VmState.Stopped
                return@execute
            }
            runCatching { AvfReflect.stop(v) }.onFailure { Log.w(TAG, "AVF refused stop()", it) }
            val gen = generation
            // onStopped normally follows; don't strand the UI in Running if it doesn't.
            lifecycle.schedule({
                if (gen == generation && vm != null) {
                    Log.w(TAG, "no onStopped after stop(); assuming stopped")
                    generation++
                    vm = null
                    _state.value = VmState.Stopped
                }
            }, 10, TimeUnit.SECONDS)
        }
    }

    private fun cleanup(console: ConsoleLog) {
        vm = null
        console.input = null
        consoleStreams?.let { (o, i) ->
            runCatching { i.close() }
            runCatching { o.close() }
        }
        consoleStreams = null
    }

    private companion object {
        const val TAG = "AvfEngine"
        const val VM_NAME = "pocketnix"
        /** AVF's EFI loader: it finds the image's ESP and runs the image's boot loader. */
        const val UBOOT = "/apex/com.android.virt/etc/u-boot.bin"
    }
}

/** The guest's disk: one raw GPT image in app-private storage (virtmgr can't open FUSE paths). */
object DiskImage {
    fun file(context: Context): File = File(context.filesDir, "disk.img")

    /** Grows the image file to [gb] GiB; the guest grows its root partition on boot. Never shrinks. */
    fun grow(file: File, gb: Int) {
        val want = gb.toLong() shl 30
        if (file.length() < want) RandomAccessFile(file, "rw").use { it.setLength(want) }
    }
}

/** A connected vsock socket; shutdown wakes threads blocked reading it, as close alone may not. */
class VsockStream(private val pfd: ParcelFileDescriptor) : AgentStream {
    private val outPfd = pfd.dup()
    override val input: InputStream = ParcelFileDescriptor.AutoCloseInputStream(pfd)
    override val output: OutputStream = ParcelFileDescriptor.AutoCloseOutputStream(outPfd)
    @Volatile private var closed = false

    override fun shutdownOutput() {
        runCatching { Os.shutdown(outPfd.fileDescriptor, OsConstants.SHUT_WR) }
    }

    @Synchronized
    override fun close() {
        if (closed) return
        closed = true
        runCatching { Os.shutdown(pfd.fileDescriptor, OsConstants.SHUT_RDWR) }
        runCatching { input.close() }
        runCatching { output.close() }
    }
}
