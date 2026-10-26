package dev.nickolaj.pocketnix.engine

import dev.nickolaj.pocketnix.agent.AgentDialer
import dev.nickolaj.pocketnix.agent.TcpDialer
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * Debug builds: "the VM" is an agent on another machine, reached over TCP
 * (`pocketnix-agent --listen tcp:0.0.0.0:9100 --no-power`; 10.0.2.2 is the
 * host from the Android emulator). It has no console, and Stop only
 * disconnects.
 */
class TcpEngine(private val host: String, private val port: Int) : VmEngine {
    private val _state = MutableStateFlow<VmState>(VmState.Stopped)
    override val state: StateFlow<VmState> = _state.asStateFlow()
    override val ownsGuest = false

    override val dialer: AgentDialer?
        get() = if (_state.value == VmState.Running) TcpDialer(host, port) else null

    override fun start(settings: VmSettings, console: ConsoleLog) {
        console.append("\r\n[pocketnix: TCP agent backend, $host:$port; there is no console]\r\n")
        _state.value = VmState.Running
    }

    override fun forceStop() {
        _state.value = VmState.Stopped
    }
}
