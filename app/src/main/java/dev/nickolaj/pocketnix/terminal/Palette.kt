package dev.nickolaj.pocketnix.terminal

import dev.nickolaj.pocketnix.agent.Snippet

/** One command-palette row: a module snippet, or a shell history entry. */
data class PaletteEntry(val label: String, val command: String, val submit: Boolean, val snippet: Boolean)

object Palette {
    /**
     * The rows for [query]: matching snippets first, in the module's order,
     * then matching history (newest first) that no snippet already offers.
     * Every word of the query must appear, in any case and order.
     */
    fun entries(snippets: List<Snippet>, history: List<String>, query: String): List<PaletteEntry> {
        val words = query.lowercase().split(Regex("\\s+")).filter { it.isNotEmpty() }
        fun matches(vararg fields: String) = words.all { w -> fields.any { it.lowercase().contains(w) } }
        val fromSnippets = snippets.filter { matches(it.name, it.command) }
            .map { PaletteEntry(it.name, it.command, it.submit, snippet = true) }
        val offered = snippets.map { it.command }.toSet()
        val fromHistory = history.filter { it !in offered && matches(it) }
            .map { PaletteEntry(it, it, submit = false, snippet = false) }
        return fromSnippets + fromHistory
    }
}
