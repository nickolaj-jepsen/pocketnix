package dev.nickolaj.pocketnix

import android.app.Application
import android.util.Log
import dev.nickolaj.pocketnix.gui.Foreground
import dev.nickolaj.pocketnix.gui.GuiSession
import org.lsposed.hiddenapibypass.HiddenApiBypass

class PocketnixApp : Application() {
    // Lazy: the provision provider can be called before onCreate runs.
    val vm: Vm by lazy { Vm(this) }

    override fun onCreate() {
        super.onCreate()
        // The VM dies with this process: a worker thread's stray exception is logged, not fatal;
        // only the main thread's crashes, as the UI is gone by then anyway.
        val crash = Thread.getDefaultUncaughtExceptionHandler()
        Thread.setDefaultUncaughtExceptionHandler { t, e ->
            if (t === mainLooper.thread) crash?.uncaughtException(t, e) else Log.e("PocketnixApp", "uncaught in ${t.name}; the thread ends, the app stays", e)
        }
        // The AVF API is @SystemApi and VirtualMachineCustomImageConfig @hide, and a
        // keyboard's selected layout is InputManager's hidden API; this exempts them
        // for this process only. Each call replaces the list: name them all here.
        runCatching {
            HiddenApiBypass.addHiddenApiExemptions(
                "Landroid/system/virtualmachine/",
                "Landroid/system/virtualizationservice/",
                "Landroid/hardware/input/",
                "Landroid/view/InputDevice;"
            )
        }.onFailure { Log.w("PocketnixApp", "hidden API exemption failed", it) }
        registerActivityLifecycleCallbacks(Foreground)
        // Every entry point runs this, so a stale guest window's card never shows a window that's gone.
        GuiSession.sweep(this)
    }
}

val android.content.Context.vm: Vm get() = (applicationContext as PocketnixApp).vm
