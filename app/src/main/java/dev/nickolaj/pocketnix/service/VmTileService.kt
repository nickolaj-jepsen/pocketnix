package dev.nickolaj.pocketnix.service

import android.app.PendingIntent
import android.content.Intent
import android.service.quicksettings.Tile
import android.service.quicksettings.TileService
import dev.nickolaj.pocketnix.ControlActivity
import dev.nickolaj.pocketnix.VmPhase
import dev.nickolaj.pocketnix.engine.VmState
import dev.nickolaj.pocketnix.gui.StopActivity
import dev.nickolaj.pocketnix.vm
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch

/** A Quick Settings tile that starts and stops the VM, showing its state. */
class VmTileService : TileService() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private var watch: Job? = null

    override fun onStartListening() {
        watch = scope.launch {
            vm.phase.collect { phase ->
                val tile = qsTile ?: return@collect
                tile.state = if (phase.off) Tile.STATE_INACTIVE else Tile.STATE_ACTIVE
                tile.subtitle = tileSubtitle(phase)
                tile.updateTile()
            }
        }
    }

    override fun onStopListening() {
        watch?.cancel()
        watch = null
    }

    override fun onClick() {
        // Quick Settings works on the lock screen; starting or stopping the VM shouldn't.
        if (isLocked) unlockAndRun(::toggle) else toggle()
    }

    private fun toggle() {
        val s = vm.state.value
        if (s == VmState.Running || s == VmState.Starting) {
            // Open GUI windows close with it: that asks first.
            if (vm.gui.windowTitles().isEmpty()) {
                vm.stop()
            } else {
                startActivityAndCollapse(PendingIntent.getActivity(this, 1, StopActivity.intent(this), PendingIntent.FLAG_IMMUTABLE))
            }
            return
        }
        // Through an activity: a foreground service may only start from the foreground.
        val start = Intent(this, ControlActivity::class.java)
            .setAction(ControlActivity.ACTION_START)
            .putExtra(ControlActivity.EXTRA_OPEN, false)
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        startActivityAndCollapse(PendingIntent.getActivity(this, 0, start, PendingIntent.FLAG_IMMUTABLE))
    }

    override fun onDestroy() {
        scope.cancel()
        super.onDestroy()
    }
}

/** The tile's subtitle for [phase]: one short line under the label. */
internal fun tileSubtitle(phase: VmPhase): String = when (phase) {
    VmPhase.Stopped -> "Stopped"
    is VmPhase.Failed -> "Failed"
    VmPhase.Starting -> "Starting"
    VmPhase.Booting -> "Booting"
    VmPhase.AgentLate -> "No agent yet"
    is VmPhase.Running -> phase.host ?: "Running"
    is VmPhase.Refused -> "Refused: ${phase.reason}"
    VmPhase.Stopping -> "Stopping"
}
