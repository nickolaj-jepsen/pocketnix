package dev.nickolaj.pocketnix

import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.os.ParcelFileDescriptor
import android.os.Process
import android.os.ResultReceiver
import java.io.File
import java.io.FileDescriptor

/**
 * `cmd activity memory-limiter` as this app rather than the adb shell, over the
 * activity service's shell transaction. ActivityManagerService's onShellCommand
 * skips Binder's check for an adb caller, and this command checks no permission:
 * this asks whether a phone lets that through. A debug build's experiment on its
 * own uid: nothing may depend on it, as it works only through a missing check.
 */
internal object SelfLimiter {
    /** The limiter's status, or throws what the phone refused it with. */
    fun status(): String = run("status")

    /** Has the limiter leave this app's uid alone until the phone reboots. */
    fun ignoreSelf(): String = run("ignore", Process.myUid().toString())

    private fun run(vararg args: String): String {
        exemptHiddenApis()
        val activity = Class.forName("android.os.ServiceManager")
            .getMethod("getService", String::class.java)
            .invoke(null, "activity") as IBinder
        val shellCommand = IBinder::class.java.getMethod(
            "shellCommand",
            FileDescriptor::class.java,
            FileDescriptor::class.java,
            FileDescriptor::class.java,
            Array<String>::class.java,
            Class.forName("android.os.ShellCallback"),
            ResultReceiver::class.java
        )
        val (read, write) = ParcelFileDescriptor.createPipe()
        val noInput = ParcelFileDescriptor.open(File("/dev/null"), ParcelFileDescriptor.MODE_READ_ONLY)
        val receiver = ResultReceiver(Handler(Looper.getMainLooper()))
        noInput.use {
            write.use {
                shellCommand.invoke(
                    activity,
                    noInput.fileDescriptor,
                    write.fileDescriptor,
                    write.fileDescriptor,
                    arrayOf("memory-limiter", *args),
                    null,
                    receiver
                )
            }
        }
        return ParcelFileDescriptor.AutoCloseInputStream(read).bufferedReader().use { it.readText() }
    }
}
