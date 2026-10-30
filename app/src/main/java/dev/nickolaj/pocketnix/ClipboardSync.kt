package dev.nickolaj.pocketnix

import org.json.JSONObject

/**
 * Android's clipboard as the app sends it: its [text] ("" for a clip without
 * text), its MIME [types] and `ClipDescription.getTimestamp()`.
 */
data class ClipSnapshot(val text: String, val types: List<String>, val timestamp: Long) {
    val isText get() = types.any { it.startsWith("text/") }
}

/**
 * What the guest has of the clipboard, which decides whether Android's
 * clipboard, seen on focus, is news to it. Each transition replaces the whole
 * state, so it's safe to apply from any thread.
 */
data class ClipboardSync(private val sent: ClipSnapshot? = null, private val guestText: String? = null) {
    /**
     * Whether [clip] is news to the guest: neither the clip last sent nor a
     * guest's text copy coming back. A text clip matches by content and
     * types, any other by its timestamp too, so a second image of one type is
     * sent.
     */
    fun isNew(clip: ClipSnapshot): Boolean {
        if (clip.isText && clip.text == guestText) return false
        val last = sent ?: return true
        if (last.types != clip.types || last.text != clip.text) return true
        return !clip.isText && last.timestamp != clip.timestamp
    }

    /** The guest has [clip]. */
    fun sent(clip: ClipSnapshot) = ClipboardSync(sent = clip)

    /**
     * A guest copy is on Android's clipboard: its [text], or null for typed
     * data. Whatever the app sent before is no longer the guest's.
     */
    fun guestCopied(text: String?) = ClipboardSync(guestText = text)
}

/**
 * A guest copy pushed to the app (`guest-clipboard`, or `guest-clipboard-data`
 * for typed data, whose [text] is null), and whether it's [sensitive]: a
 * secret Android should keep out of its previews.
 */
data class GuestCopy(val text: String?, val sensitive: Boolean) {
    companion object {
        /** The guest copy in [msg], or null for any other push. */
        fun parse(msg: JSONObject): GuestCopy? = when (msg.optString("type")) {
            "guest-clipboard" -> GuestCopy(msg.optString("text"), msg.optBoolean("sensitive"))
            "guest-clipboard-data" -> GuestCopy(null, msg.optBoolean("sensitive"))
            else -> null
        }
    }
}
