package dev.nickolaj.pocketnix.gui

import android.app.Activity
import android.app.AlertDialog
import android.content.Context
import android.content.Intent
import android.os.Bundle
import dev.nickolaj.pocketnix.vm

/**
 * Stops the VM for the Quick Settings tile and the notification's Stop
 * action, asking first while GUI windows are open, since they close with it.
 */
class StopActivity : Activity(), Helper {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val windows = vm.gui.windowTitles()
        if (windows.isEmpty()) {
            vm.stop()
            finish()
            return
        }
        AlertDialog.Builder(this)
            .setMessage(stopQuestion(windows))
            .setPositiveButton("Stop") { _, _ -> guard("stop") { vm.stop() } }
            .setNegativeButton(android.R.string.cancel, null)
            .setOnDismissListener { finish() }
            .show()
    }

    companion object {
        fun intent(context: Context): Intent = Intent(context, StopActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
    }
}

/** The question before stopping the VM, naming the open windows [titles]. */
fun stopQuestion(titles: List<String>): String =
    "Stop the VM? The guest shuts down and every tab ends. ${closingWindows(titles)}".trim()

/** "Its windows close too: A, B." for [titles], or "" with none. */
fun closingWindows(titles: List<String>): String =
    if (titles.isEmpty()) "" else "Its windows close too: ${titles.joinToString(", ")}."
