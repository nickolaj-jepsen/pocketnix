package dev.nickolaj.pocketnix.service

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log
import dev.nickolaj.pocketnix.engine.DiskImage
import dev.nickolaj.pocketnix.vm

/**
 * Opt-in start on phone boot (Settings). BOOT_COMPLETED arrives after the
 * first unlock, when the disk image in app storage is readable, and may
 * start a foreground service of this type.
 */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED) return
        val vm = context.vm
        if (!vm.settings.startOnBoot || vm.settings.tcpBackend || !DiskImage.file(context).exists()) return
        Log.i("BootReceiver", "starting the VM on boot")
        vm.start()
    }
}
