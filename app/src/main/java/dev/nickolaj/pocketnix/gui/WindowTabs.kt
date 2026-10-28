package dev.nickolaj.pocketnix.gui

/** A guest window shown as a tab in the terminal screen: [tab] is the agent id of the terminal tab it follows. */
data class WindowTab(val id: Int, val tab: String, val title: String)

/** One entry of the terminal screen's tab bar. */
sealed interface TabStripItem<out T> {
    data class Terminal<T>(val tab: T) : TabStripItem<T>
    data class Window(val window: WindowTab) : TabStripItem<Nothing>
}

/**
 * The tab bar: each terminal of [terminals] followed by the windows that
 * name its [agentId], then the windows whose terminal the bar doesn't have
 * (it closed, or the id is from before an agent restart). Windows keep
 * [windows]' order.
 */
fun <T> tabStrip(terminals: List<T>, agentId: (T) -> String, windows: List<WindowTab>): List<TabStripItem<T>> {
    val placed = HashSet<Int>()
    val strip = ArrayList<TabStripItem<T>>(terminals.size + windows.size)
    for (t in terminals) {
        strip += TabStripItem.Terminal(t)
        val id = agentId(t)
        if (id.isEmpty()) continue
        for (w in windows) if (w.tab == id && placed.add(w.id)) strip += TabStripItem.Window(w)
    }
    for (w in windows) if (placed.add(w.id)) strip += TabStripItem.Window(w)
    return strip
}

/** What shows a guest window: its own task's [GuiWindowActivity], or a terminal tab's [TabWindowView]. */
interface WindowHost {
    val windowId: Int

    /** Stop showing the window, without asking the client to close: the guest unmapped it, or it moved. */
    fun dismiss()

    /** The toplevel's title, app id or app-list entry changed. */
    fun toplevelChanged()

    fun cursor(name: String)

    /** The client asked to be minimized. */
    fun minimize()
}
