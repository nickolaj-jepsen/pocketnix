package dev.nickolaj.pocketnix.gui

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp

/** One window-menu entry: an action, or with [children] a page of them. */
class MenuAction(val label: String, val enabled: Boolean = true, val children: List<MenuAction> = emptyList(), val run: () -> Unit = {})

const val MOVE_TO_TAB = "Move to a terminal tab"
const val MOVE_OUT = "Move to its own window"

/**
 * Window [id]'s menu (docs/gui.md, Window controls): its app's settings and
 * zoom, then [extra] and Close. Read when the menu opens; settings apply to
 * every open window of the app at once.
 */
fun windowMenu(gui: GuiSession, id: Int, extra: List<MenuAction>): List<MenuAction> {
    val app = gui.appKey(id)
    val choice = gui.appSettings.of(app)
    val surface = gui.surfaceOf(id)
    return buildList {
        add(
            MenuAction("Size: ${choice.size.label}", children = SizePreset.entries.map { p ->
                MenuAction(checked(p == choice.size, p.label)) { gui.appSettings.setSize(app, p) }
            }),
        )
        add(
            MenuAction("Input: ${choice.input.label}", children = InputMode.entries.map { m ->
                // A client without wl_touch gets Pointer mode instead (gui-protocol.md, Touch).
                val label = if (m == InputMode.TOUCH && !gui.hasTouch(id)) "${m.label} (this app has no touch)" else m.label
                MenuAction(checked(m == choice.input, label)) { gui.appSettings.setInput(app, m) }
            }),
        )
        // Pinch and pan are pocketnix's in the other modes; in Touch mode they're the app's unless this is on.
        if (surface != null && choice.input == InputMode.TOUCH) {
            add(MenuAction(checked(surface.zooming, "Zoom")) { surface.zooming = !surface.zooming })
        }
        if (surface != null && surface.zoom.zoomed) {
            add(MenuAction("Reset zoom") {
                surface.zooming = false
                surface.resetZoom()
            })
        }
        if (choice.overridden) add(MenuAction("Reset to config") { gui.appSettings.reset(app) })
        addAll(extra)
        add(MenuAction("Close") { gui.requestClose(id) })
    }
}

private fun checked(on: Boolean, label: String) = if (on) "✓ $label" else label

/**
 * A dropdown of [items], whose entries with children open as a page of
 * their own. [items] is read each time it opens.
 */
@Composable
fun WindowMenu(expanded: Boolean, onDismiss: () -> Unit, items: () -> List<MenuAction>) {
    // The page shown, as the path of labels from the top; reset whenever the menu closes.
    var page by remember(expanded) { mutableStateOf<List<String>>(emptyList()) }
    val top = remember(expanded) { if (expanded) items() else emptyList() }
    DropdownMenu(expanded = expanded, onDismissRequest = onDismiss) {
        var shown = top
        for (label in page) shown = shown.firstOrNull { it.label == label }?.children ?: shown
        if (page.isNotEmpty()) {
            DropdownMenuItem(text = { Text("‹ ${page.last()}") }, onClick = { page = page.dropLast(1) })
        }
        for (a in shown) {
            DropdownMenuItem(
                text = { Text(if (a.children.isNotEmpty()) "${a.label} ›" else a.label) },
                enabled = a.enabled,
                onClick = {
                    if (a.children.isNotEmpty()) {
                        page = page + a.label
                    } else {
                        onDismiss()
                        guard("window menu: ${a.label}") { a.run() }
                    }
                },
            )
        }
    }
}

/**
 * The slim bar along the bottom of a window on the phone: the keyboard
 * button, the title and the menu. While [typingRow] is set (the extra keys
 * row, while typing), it shows that in their place.
 */
@Composable
fun WindowBar(
    title: String,
    onKeyboard: (() -> Unit)?,
    typingRow: (@Composable () -> Unit)?,
    menu: () -> List<MenuAction>,
    onMenu: (open: Boolean) -> Unit = {},
) {
    if (typingRow != null) {
        Box(Modifier.fillMaxWidth().background(BAR_BACKGROUND)) { typingRow() }
        return
    }
    var open by remember { mutableStateOf(false) }
    Row(
        Modifier.fillMaxWidth().height(BAR_HEIGHT.dp).background(BAR_BACKGROUND),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        if (onKeyboard != null) BarButton("⌨", "Keyboard") { guard("window bar: keyboard") { onKeyboard() } }
        Text(
            title,
            color = BAR_TEXT,
            fontSize = 13.sp,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
            modifier = Modifier.weight(1f).padding(start = if (onKeyboard != null) 2.dp else 16.dp, end = 8.dp),
        )
        Box {
            BarButton("⋮", "Window menu") {
                open = true
                onMenu(true)
            }
            WindowMenu(open, onDismiss = { open = false; onMenu(false) }, items = menu)
        }
    }
}

/** A glyph with a touch target the bar's full height. */
@Composable
private fun BarButton(glyph: String, description: String, onClick: () -> Unit) {
    Box(
        Modifier.height(BAR_HEIGHT.dp).semantics { contentDescription = description }.clickable(onClick = onClick).padding(horizontal = 16.dp),
        contentAlignment = Alignment.Center,
    ) {
        Text(glyph, color = BAR_TEXT, fontSize = 18.sp)
    }
}

const val BAR_HEIGHT = 36
private val BAR_BACKGROUND = Color(0xff1e1e1e)
internal val BAR_TEXT = Color(0xffdddddd)
