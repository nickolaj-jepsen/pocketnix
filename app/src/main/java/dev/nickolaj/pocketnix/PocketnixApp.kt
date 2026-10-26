package dev.nickolaj.pocketnix

import android.app.Application
import android.util.Log
import org.lsposed.hiddenapibypass.HiddenApiBypass

class PocketnixApp : Application() {
    // Lazy: the provision provider can be called before onCreate runs.
    val vm: Vm by lazy { Vm(this) }

    override fun onCreate() {
        super.onCreate()
        // The AVF API is @SystemApi and VirtualMachineCustomImageConfig @hide;
        // this exempts them for this process only.
        runCatching {
            HiddenApiBypass.addHiddenApiExemptions("Landroid/system/virtualmachine/", "Landroid/system/virtualizationservice/")
        }.onFailure { Log.w("PocketnixApp", "hidden API exemption failed", it) }
    }
}

val android.content.Context.vm: Vm get() = (applicationContext as PocketnixApp).vm
