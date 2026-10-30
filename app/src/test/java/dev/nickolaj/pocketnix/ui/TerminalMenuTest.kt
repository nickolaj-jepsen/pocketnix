package dev.nickolaj.pocketnix.ui

import dev.nickolaj.pocketnix.agent.Forward
import dev.nickolaj.pocketnix.forward.ForwardOffers
import dev.nickolaj.pocketnix.gui.MenuAction
import org.junit.Assert.assertEquals
import org.junit.Test

class TerminalMenuTest {
    @Test
    fun aTapOpensAHyperlinkUnlessTheProgramTracksTheMouse() {
        assertEquals(TapAction.Open("https://a.example"), tapAction("https://a.example", mouseTracking = false))
        assertEquals(TapAction.Keyboard, tapAction("https://a.example", mouseTracking = true))
        assertEquals(TapAction.Keyboard, tapAction(null, mouseTracking = false))
    }

    private fun menu(links: List<String> = emptyList(), selection: String? = null, opened: MutableList<String> = mutableListOf(), reset: () -> Unit = {}) = terminalMenu(links, selection, open = { opened += it }, share = {}, shareTranscript = {}, reset = reset)

    private fun List<MenuAction>.entry(label: String) = single { it.label == label }

    @Test
    fun entriesWithNothingToActOnAreDisabled() {
        val items = menu()
        assertEquals(false, items.entry("Open link…").enabled)
        assertEquals(false, items.entry("Share selection").enabled)
        assertEquals(true, items.entry("Share transcript").enabled)
        assertEquals(true, items.entry("Reset terminal").enabled)
    }

    @Test
    fun openLinkListsEachLinkAndOpensTheOneChosen() {
        val opened = mutableListOf<String>()
        val items = menu(links = listOf("https://b.example", "https://a.example"), selection = "picked", opened = opened)
        val page = items.entry("Open link…")
        assertEquals(true, page.enabled)
        assertEquals(listOf("https://b.example", "https://a.example"), page.children.map { it.label })
        page.children.single { it.label == "https://a.example" }.run()
        assertEquals(listOf("https://a.example"), opened)
        assertEquals(true, items.entry("Share selection").enabled)
    }

    @Test
    fun aThrowingActionDoesNotEscape() {
        menu(reset = { throw IllegalStateException("boom") }).entry("Reset terminal").run()
    }

    @Test
    fun onlyWebAndGuestFileLinksOpen() {
        assertEquals(LinkTarget.Web("https://x"), classifyLink("https://x", "box"))
        assertEquals(LinkTarget.GuestFile("/home/u/a b"), classifyLink("file://box/home/u/a%20b", "box"))
        assertEquals(LinkTarget.GuestFile("/etc/hosts"), classifyLink("file:///etc/hosts", "box"))
        assertEquals(LinkTarget.GuestFile("/tmp/x"), classifyLink("file://localhost/tmp/x", null))
        assertEquals(LinkTarget.OtherHost("other"), classifyLink("file://other/x", "box"))
        for (url in listOf("intent:#Intent;action=android.intent.action.CALL;end", "content://com.android.contacts/contacts", "android-app://com.example/https/x")) {
            assertEquals(url, true, classifyLink(url, "box") is LinkTarget.Refused)
        }
    }

    @Test
    fun localhostLinksFollowAListeningForward() {
        val web = Forward("web", 8000, 18000, lan = false)
        assertEquals("http://localhost:18000/a?b=c#d", rewriteLocalhost("http://localhost:8000/a?b=c#d", listOf(web)))
        assertEquals("http://127.0.0.1:18000/", rewriteLocalhost("http://127.0.0.1:8000/", listOf(web)))
        // Not listening (another app holds 18000), so not among the forwards given.
        assertEquals("http://localhost:8000/", rewriteLocalhost("http://localhost:8000/", emptyList()))
        assertEquals("http://localhost:8000/", rewriteLocalhost("http://localhost:8000/", listOf(web.copy(udp = true))))
        assertEquals("http://example.org:8000/", rewriteLocalhost("http://example.org:8000/", listOf(web)))
        assertEquals("http://localhost:8080/", rewriteLocalhost("http://localhost:8080/", listOf(Forward("same", 8080, 8080, lan = false))))
    }

    @Test
    fun aLocalhostLinkToAnOfferedListenerAllowsThenOpensOnTheChosenPort() {
        val url = "http://localhost:8765/"
        val offers = ForwardOffers().observe(listOf(8765), lan = false, module = emptyList(), now = 0)
        val tap = webOpen(url, emptyList(), offers.listening)
        assertEquals(WebOpen.AllowThenOpen(8765, url), tap)
        val allowed = offers.allow(8765, 18765)
        assertEquals("http://localhost:18765/", rewriteLocalhost((tap as WebOpen.AllowThenOpen).url, allowed.forwards(emptyList())))
        // Once its forward listens, the next tap just goes through it.
        assertEquals(WebOpen.Now("http://localhost:18765/"), webOpen(url, allowed.forwards(emptyList()), allowed.listening))
    }

    @Test
    fun otherLocalhostLinksOpenWithoutAllowing() {
        val offerable = setOf(8765)
        assertEquals(WebOpen.Now("http://localhost:9000/"), webOpen("http://localhost:9000/", emptyList(), offerable))
        assertEquals(WebOpen.Now("http://example.org:8765/"), webOpen("http://example.org:8765/", emptyList(), offerable))
        assertEquals(WebOpen.Now("http://localhost:18000/"), webOpen("http://localhost:8000/", listOf(Forward("web", 8000, 18000, lan = false)), setOf(8000)))
    }

    @Test
    fun linksAreNewestFirstAndOnce() {
        val rows = listOf(
            BufferRow("old https://plain.example.", wraps = false),
            BufferRow("printf 'https://plain.example\\n'", wraps = false),
            BufferRow("link (see https://wrapped.exa", wraps = true),
            BufferRow("mple/a_(b))", wraps = false),
            BufferRow("x https://plain.example", wraps = false)
        )
        val hyperlinks = listOf(LinkAt("https://osc.example", row = -4, col = 0), LinkAt("file://box/tmp/f", row = 0, col = 0))
        assertEquals(
            listOf("https://plain.example", "file://box/tmp/f", "https://wrapped.example/a_(b)", "https://osc.example"),
            linkList(plainLinks(rows, firstRow = -4) + hyperlinks)
        )
    }

    @Test
    fun aWebLinkLaunchesWithItsSchemeLowercased() {
        assertEquals(LinkTarget.Web("https://x.example/Path"), classifyLink("HTTPS://x.example/Path", "box"))
    }

    @Test(timeout = 2_000)
    fun aRunawayUrlIsDroppedQuickly() {
        val rows = listOf(BufferRow("http://a" + ")".repeat(300_000), wraps = false), BufferRow("https://ok.example)", wraps = false))
        assertEquals(listOf("https://ok.example"), plainLinks(rows, firstRow = 0).map { it.url })
    }

    @Test(timeout = 2_000)
    fun manyBracketTailsTrimInLinearTime() {
        // At the cap, so each reaches trimUrl: quadratic trimming takes seconds here.
        val tail = "http://a" + ")".repeat(MAX_URL_CHARS - "http://a".length)
        val rows = List(400) { BufferRow(tail, wraps = false) }
        assertEquals(setOf("http://a"), plainLinks(rows, firstRow = 0).map { it.url }.toSet())
    }

    @Test
    fun anOverlongRunOffersNothingFromItsMiddle() {
        val rows = listOf(BufferRow("https://t.example/?r=" + "a".repeat(MAX_URL_CHARS) + "https://x.example", wraps = false))
        assertEquals(emptyList<String>(), plainLinks(rows, firstRow = 0).map { it.url })
    }

    @Test
    fun anOverlongHyperlinkIsLeftOut() {
        val links = listOf(LinkAt("https://x.example/" + "a".repeat(MAX_URL_CHARS), row = 1, col = 0), LinkAt("https://ok.example", row = 0, col = 0))
        assertEquals(listOf("https://ok.example"), linkList(links))
    }

    @Test
    fun openLinkKeepsTheNewestAndShortensLongLabels() {
        val links = (1..60).map { LinkAt("https://x.example/$it", row = it, col = 0) }
        val list = linkList(links)
        assertEquals(MAX_LINKS, list.size)
        assertEquals("https://x.example/60", list.first())

        val long = "https://x.example/" + "a".repeat(300)
        val opened = mutableListOf<String>()
        val entry = menu(links = listOf(long), opened = opened).entry("Open link…").children.single()
        assertEquals(true, entry.label.length <= MAX_LINK_LABEL)
        entry.run()
        assertEquals(listOf(long), opened)
    }
}
