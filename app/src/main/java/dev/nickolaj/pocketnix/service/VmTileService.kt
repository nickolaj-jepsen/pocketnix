package dev.nickolaj.pocketnix.service

import android.app.PendingIntent
import android.content.Intent
import android.service.quicksettings.Tile
import android.service.quicksettings.TileService
import dev.nickolaj.pocketnix.ControlActivity
import dev.nickolaj.pocketnix.agent.ControlChannel
import dev.nickolaj.pocketnix.engine.VmState
import dev.nickolaj.pocketnix.vm
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.launch

/** A Quick Settings tile that starts and stops the VM, showing its state. */
class VmTileService : TileService() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private var watch: Job? = null

    override fun onStartListening() {
        watch = scope.launch {
            combine(vm.state, vm.control.link, vm.stopping) { s, link, stopping -> Triple(s, link, stopping) }.collect { (s, link, stopping) ->
                val tile = qsTile ?: return@collect
                val running = s == VmState.Running || s == VmState.Starting
                tile.state = if (running) Tile.STATE_ACTIVE else Tile.STATE_INACTIVE
                tile.subtitle = when {
                    stopping -> "Stopping"
                    s == VmState.Starting -> "Starting"
                    s is VmState.Failed -> "Failed"
                    !running -> "Stopped"
                    link is ControlChannel.Link.Up -> vm.config.value?.hostname ?: "Running"
                    else -> "Booting"
                }
                tile.updateTile()
            }
        }
    }

    override fun onStopListening() {
        watch?.cancel()
        watch = null
    }

    override fun onClick() {
        val s = vm.state.value
        if (s == VmState.Running || s == VmState.Starting) {
            vm.stop()
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
