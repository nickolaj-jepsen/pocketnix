package dev.nickolaj.pocketnix.files

import android.app.Activity
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Bundle
import android.util.Log
import android.widget.Toast
import dev.nickolaj.pocketnix.gui.Helper
import dev.nickolaj.pocketnix.vm
import org.json.JSONObject

/**
 * Shows Android's document picker for a guest's pick or save (a
 * [Transfers] push) and hands the result to the guest. Invisible; reports
 * with a toast.
 */
class PickerActivity : Activity(), Helper {
    private val ref get() = intent.getStringExtra(EXTRA_REF).orEmpty()
    private val pick get() = intent.getStringExtra(EXTRA_KIND) == "pick"
    private var answered = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        answered = savedInstanceState?.getBoolean(STATE_ANSWERED) ?: false
        // Recreated (a rotation): the picker is up already, and its result comes here.
        if (savedInstanceState != null) return
        val doc = if (pick) {
            val types = intent.getStringArrayExtra(EXTRA_TYPES).orEmpty()
            Intent(Intent.ACTION_OPEN_DOCUMENT)
                .setType(types.singleOrNull() ?: "*/*")
                .putExtra(Intent.EXTRA_ALLOW_MULTIPLE, intent.getBooleanExtra(EXTRA_MULTIPLE, false))
                .apply { if (types.size > 1) putExtra(Intent.EXTRA_MIME_TYPES, types) }
        } else {
            Intent(Intent.ACTION_CREATE_DOCUMENT)
                .setType(intent.getStringExtra(EXTRA_MIME).orEmpty().ifEmpty { "application/octet-stream" })
                .putExtra(Intent.EXTRA_TITLE, intent.getStringExtra(EXTRA_NAME))
        }.addCategory(Intent.CATEGORY_OPENABLE)
        runCatching { startActivityForResult(doc, REQUEST) }.onFailure {
            Log.w(TAG, "no document picker: ${it.message}")
            finish()
        }
    }

    override fun onSaveInstanceState(outState: Bundle) {
        super.onSaveInstanceState(outState)
        outState.putBoolean(STATE_ANSWERED, answered)
    }

    @Deprecated("Deprecated in Java")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode != REQUEST) return
        answered = true
        val uris = data?.clipData?.let { c -> (0 until c.itemCount).map { c.getItemAt(it).uri } } ?: listOfNotNull(data?.data)
        if (resultCode != RESULT_OK || uris.isEmpty()) {
            cancel()
            finish()
            return
        }
        val transfers = vm.transfers
        val ref = ref
        val pick = pick
        Thread({
            val result = runCatching { if (pick) transfers.picked(ref, uris) else transfers.saveTo(ref, uris.first()) }
            result.exceptionOrNull()?.let { Log.w(TAG, "transfer $ref: ${it.message}") }
            runOnUiThread {
                val msg = result.fold(
                    { if (pick) "Sent ${uris.size} file${if (uris.size == 1) "" else "s"} to the guest" else "Saved" },
                    { e -> "${if (pick) "Pick" else "Save"} failed: ${e.message}" },
                )
                Toast.makeText(this, msg, Toast.LENGTH_LONG).show()
                finish()
            }
        }, "transfer").start()
    }

    override fun onDestroy() {
        // Gone without a result (the task was swiped away): the guest needn't wait out its timeout.
        if (isFinishing && !answered) cancel()
        super.onDestroy()
    }

    private fun cancel() {
        answered = true
        val transfers = vm.transfers
        val ref = ref
        Thread({ runCatching { transfers.cancel(ref) }.onFailure { Log.w(TAG, "cancel $ref: ${it.message}") } }, "transfer").start()
    }

    companion object {
        private const val TAG = "PickerActivity"
        private const val REQUEST = 1
        private const val STATE_ANSWERED = "answered"
        private const val EXTRA_KIND = "kind"
        private const val EXTRA_REF = "ref"
        private const val EXTRA_TYPES = "types"
        private const val EXTRA_MULTIPLE = "multiple"
        private const val EXTRA_NAME = "name"
        private const val EXTRA_MIME = "mime"

        /** Starts the picker for a pick or save push. */
        fun intent(context: Context, msg: JSONObject): Intent {
            val types = msg.optJSONArray("types")?.let { a -> Array(a.length()) { a.getString(it) } } ?: emptyArray()
            return Intent(context, PickerActivity::class.java)
                // Each request is its own activity; the data keeps their PendingIntents apart.
                .setData(Uri.fromParts("pocketnix-transfer", msg.optString("ref"), null))
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                .putExtra(EXTRA_KIND, msg.optString("type"))
                .putExtra(EXTRA_REF, msg.optString("ref"))
                .putExtra(EXTRA_TYPES, types)
                .putExtra(EXTRA_MULTIPLE, msg.optBoolean("multiple"))
                .putExtra(EXTRA_NAME, msg.optString("name"))
                .putExtra(EXTRA_MIME, msg.optString("mime"))
        }
    }
}
