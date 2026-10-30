package dev.nickolaj.pocketnix.ui

import android.content.Context
import android.util.Log
import android.widget.Toast
import com.termux.terminal.TerminalBuffer
import dev.nickolaj.pocketnix.GuestBridge
import dev.nickolaj.pocketnix.agent.Forward
import dev.nickolaj.pocketnix.gui.MenuAction
import dev.nickolaj.pocketnix.gui.guard
import dev.nickolaj.pocketnix.terminal.Tab
import java.net.URI
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.launch

const val OPEN_LINK = "Open link…"
const val SHARE_SELECTION = "Share selection"
const val SHARE_TRANSCRIPT = "Share transcript"
const val RESET_TERMINAL = "Reset terminal"

// Guest output is untrusted: these bound the main-thread work and the menu a tab's text can cause.
// A longer plain URL isn't offered (browsers stop near this length too).
const val MAX_URL_CHARS = 2048

// Open link… shows only the newest this many, each label shortened to this.
const val MAX_LINKS = 50
const val MAX_LINK_LABEL = 80

/** What a tap on the terminal does. */
sealed interface TapAction {
    data class Open(val url: String) : TapAction

    data object Keyboard : TapAction
}

/**
 * A tap on an OSC 8 [hyperlink] opens it; any other tap brings up the
 * keyboard. With [mouseTracking] on the program already got the tap as a
 * click, so it opens nothing.
 */
fun tapAction(hyperlink: String?, mouseTracking: Boolean): TapAction = if (hyperlink != null && !mouseTracking) TapAction.Open(hyperlink) else TapAction.Keyboard

/** Where a link printed in a tab leads; guest output chooses it, so only these open. */
sealed interface LinkTarget {
    data class Web(val url: String) : LinkTarget

    /** A file on this guest, at the decoded absolute [path]. */
    data class GuestFile(val path: String) : LinkTarget

    /** A file on [host], another machine (a link printed over ssh). */
    data class OtherHost(val host: String) : LinkTarget

    /** A scheme Android could launch something with, or not a URL. */
    data class Refused(val scheme: String) : LinkTarget
}

/** Classifies [url] for opening; `file:` links are this guest's when their host is empty, `localhost` or [hostname]. */
fun classifyLink(url: String, hostname: String?): LinkTarget {
    val uri = runCatching { URI(url) }.getOrNull() ?: return LinkTarget.Refused("")
    return when (val scheme = uri.scheme?.lowercase().orEmpty()) {
        // Android matches schemes case-sensitively, so the launched URL carries the lowercased one.
        "http", "https" -> if (uri.isOpaque) LinkTarget.Refused(scheme) else LinkTarget.Web(scheme + url.substring(scheme.length))

        "file" -> {
            // The authority, not the host: java.net.URI leaves host null for names it can't parse (an underscore).
            val host = uri.rawAuthority.orEmpty()
            val path = uri.path
            when {
                path == null || !path.startsWith("/") -> LinkTarget.Refused(scheme)
                host.isEmpty() || host.equals("localhost", ignoreCase = true) || host.equals(hostname, ignoreCase = true) -> LinkTarget.GuestFile(path)
                else -> LinkTarget.OtherHost(host)
            }
        }

        else -> LinkTarget.Refused(scheme)
    }
}

/**
 * [url] pointed at the phone port of the listening TCP forward in [forwards]
 * whose guest port it names on `localhost` or `127.0.0.1`; unchanged without one.
 */
fun rewriteLocalhost(url: String, forwards: List<Forward>): String {
    val uri = runCatching { URI(url) }.getOrNull() ?: return url
    val host = uri.host?.lowercase() ?: return url
    if (host != "localhost" && host != "127.0.0.1") return url
    val port = if (uri.port != -1) {
        uri.port
    } else {
        when (uri.scheme?.lowercase()) {
            "http" -> 80
            "https" -> 443
            else -> return url
        }
    }
    val f = forwards.firstOrNull { !it.udp && it.guestPort == port } ?: return url
    if (f.hostPort == port) return url
    val authority = (uri.rawUserInfo?.let { "$it@" } ?: "") + uri.host + ":" + f.hostPort
    return "${uri.scheme}://$authority${uri.rawPath.orEmpty()}" +
        (uri.rawQuery?.let { "?$it" } ?: "") + (uri.rawFragment?.let { "#$it" } ?: "")
}

/** One row of a terminal buffer, and whether it wraps into the next. */
class BufferRow(val text: String, val wraps: Boolean)

/** A link and the cell it starts at, rows as [TerminalBuffer]'s external ones. */
data class LinkAt(val url: String, val row: Int, val col: Int)

// Up to a character URLs never hold unescaped (RFC 3986), such as the backslash of a printf escape.
// Unbounded, so a run longer than MAX_URL_CHARS is dropped whole, not offered from its middle.
private val URL = Regex("""(?:https?|file)://[^\s<>"'`{}|\\^]+""")

/** The plain-text URLs in [rows], the buffer's rows from [firstRow] on, with wrapped rows read as one line. */
fun plainLinks(rows: List<BufferRow>, firstRow: Int): List<LinkAt> {
    val out = mutableListOf<LinkAt>()
    var i = 0
    while (i < rows.size) {
        val start = i
        val line = StringBuilder()
        val offsets = mutableListOf<Int>()
        while (true) {
            offsets += line.length
            line.append(rows[i].text)
            if (!rows[i].wraps || i == rows.lastIndex) break
            i++
        }
        i++
        for (m in URL.findAll(line)) {
            if (m.value.length > MAX_URL_CHARS) continue
            val url = trimUrl(m.value)
            if (url.endsWith("//")) continue
            // The row the match starts in: the last whose offset is at or before it.
            val k = offsets.binarySearch(m.range.first).let { if (it >= 0) it else -it - 2 }
            out += LinkAt(url, firstRow + start + k, m.range.first - offsets[k])
        }
    }
    return out
}

// Prose punctuation after a URL, and a closing bracket it didn't open; one pass, as guest output picks the length.
private fun trimUrl(s: String): String {
    var parens = s.count { it == '(' } - s.count { it == ')' }
    var brackets = s.count { it == '[' } - s.count { it == ']' }
    var end = s.length
    while (end > 0) {
        when (s[end - 1]) {
            '.', ',', ';', ':', '!', '?' -> {}
            ')' -> if (parens < 0) parens++ else break
            ']' -> if (brackets < 0) brackets++ else break
            else -> break
        }
        end--
    }
    return s.substring(0, end)
}

/**
 * The Open link… list: [links] newest (furthest down) first, each URL once,
 * at most [MAX_LINKS], leaving out any longer than [MAX_URL_CHARS].
 */
fun linkList(links: List<LinkAt>): List<String> = links.filter { it.url.length <= MAX_URL_CHARS }
    .sortedWith(compareByDescending<LinkAt> { it.row }.thenByDescending { it.col }).map { it.url }.distinct().take(MAX_LINKS)

/** [url] shortened in the middle to [MAX_LINK_LABEL] characters, for a menu entry. */
fun linkLabel(url: String): String = if (url.length <= MAX_LINK_LABEL) url else url.take(MAX_LINK_LABEL / 2) + "…" + url.takeLast(MAX_LINK_LABEL - MAX_LINK_LABEL / 2 - 1)

/**
 * The terminal's context menu: [links] as Open link…'s page, and sharing the
 * [selection] More… stored. Every action runs inside [guard]: a throw on the
 * main thread would kill the app and the VM.
 */
fun terminalMenu(
    links: List<String>,
    selection: String?,
    open: (String) -> Unit,
    share: (String) -> Unit,
    shareTranscript: () -> Unit,
    reset: () -> Unit
): List<MenuAction> = listOf(
    MenuAction(OPEN_LINK, enabled = links.isNotEmpty(), children = links.map { url -> MenuAction(linkLabel(url)) { guard("opening $url") { open(url) } } }),
    MenuAction(SHARE_SELECTION, enabled = !selection.isNullOrEmpty()) { guard("sharing the selection") { selection?.let(share) } },
    MenuAction(SHARE_TRANSCRIPT) { guard("sharing the transcript") { shareTranscript() } },
    MenuAction(RESET_TERMINAL) { guard("resetting the terminal") { reset() } }
)

/**
 * The terminal's link and share actions, on the main thread, where the
 * emulator lives. [listening] is the forwards a localhost link may go
 * through; [resolve] asks the agent for a guest path's document.
 */
class TerminalActions(
    private val context: Context,
    private val scope: CoroutineScope,
    private val bridge: GuestBridge,
    private val hostname: () -> String?,
    private val listening: () -> List<Forward>,
    private val resolve: suspend (String) -> String
) {
    /** Opens a link printed in a tab, if it is one Android may: see [classifyLink]. */
    fun open(url: String) = guard("opening $url") {
        when (val target = classifyLink(url, hostname()?.ifEmpty { null })) {
            is LinkTarget.Web -> bridge.openUrl(rewriteLocalhost(target.url, listening()), appVisible = true, chosen = true)

            is LinkTarget.GuestFile -> scope.launch {
                val rel = runCatching { resolve(target.path) }
                    .onFailure { Log.w(TAG, "resolving ${target.path}: ${it.message}") }
                    .getOrElse {
                        toast("Can't open ${target.path}: ${it.message}")
                        return@launch
                    }
                guard("opening ${target.path}") { bridge.openDocument(rel, appVisible = true, chosen = true) }
            }

            is LinkTarget.OtherHost -> toast("$url is a file on ${target.host}, not this guest")

            is LinkTarget.Refused -> toast("Not opening ${target.scheme.ifEmpty { "this" }} link")
        }
    }

    /**
     * [tab]'s Open link… list; inside a full-screen program, its screen's
     * links come first. Empty when reading the emulator fails: the menu is
     * built during composition, where a throw would kill the app and the VM.
     */
    fun links(tab: Tab): List<String> = guard("listing links", emptyList()) {
        val emulator = tab.session.emulator ?: return@guard emptyList()
        val main = emulator.mainBuffer
        val buffers = if (emulator.screen !== main) listOf(emulator.screen, main) else listOf(main)
        buffers.flatMap { b ->
            val first = -b.activeTranscriptRows
            val rows = (first until emulator.rows).map { r -> BufferRow(b.getSelectedText(0, r, emulator.columns, r, false), b.getLineWrap(r)) }
            linkList(plainLinks(rows, first) + b.hyperlinks.map { LinkAt(it.url, it.row, it.col) })
        }.distinct().take(MAX_LINKS)
    }

    /** [tab]'s context menu, with the [selection] More… stored. */
    fun menu(tab: Tab, selection: String?): List<MenuAction> = terminalMenu(
        links(tab),
        selection,
        open = ::open,
        share = { bridge.shareText(it, appVisible = true, name = "selection") },
        shareTranscript = { shareTranscript(tab) },
        reset = { tab.session.reset() }
    )

    /** Shares [tab]'s scrollback, the shell's even while a full-screen program shows. */
    fun shareTranscript(tab: Tab) = guard("sharing the transcript") {
        val text = tab.session.emulator?.mainBuffer?.transcriptText ?: return@guard
        bridge.shareText(text, appVisible = true, name = "transcript")
    }

    private fun toast(text: String) = Toast.makeText(context, text, Toast.LENGTH_LONG).show()

    private companion object {
        const val TAG = "TerminalActions"
    }
}
