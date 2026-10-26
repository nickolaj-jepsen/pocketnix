package dev.nickolaj.pocketnix.terminal

/** How composed or pasted text reaches the terminal. */
object Paste {
    private val unsafe = Regex("[\u001b\u0080-\u009f]")
    private val newline = Regex("\r?\n")

    /**
     * Encodes [text] as a paste: ESC and C1 controls stripped (so text can't
     * end the bracket or inject sequences), newlines as CR, wrapped in
     * bracketed-paste markers when the terminal enabled them (DECSET 2004).
     * [submit] presses Enter after the paste.
     */
    fun encode(text: String, bracketed: Boolean, submit: Boolean): String {
        val clean = text.replace(unsafe, "").replace(newline, "\r")
        val body = if (bracketed) "\u001b[200~$clean\u001b[201~" else clean
        return if (submit) "$body\r" else body
    }
}
