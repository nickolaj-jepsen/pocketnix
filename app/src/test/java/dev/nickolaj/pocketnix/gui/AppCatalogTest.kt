package dev.nickolaj.pocketnix.gui

import java.io.File
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

class AppCatalogTest {
    @get:Rule val tmp = TemporaryFolder()

    private val foot = AppEntry("foot", "Foot", genericName = "Terminal", keywords = listOf("shell", "prompt"), icon = "kfoot")
    private val calc = AppEntry("org.gnome.Calculator", "Calculator", icon = "kcalc")
    private val hidden = AppEntry("org.gnome.Settings-panel", "Panel", icon = "kpanel", noDisplay = true)

    private fun catalog() = AppCatalog(File(tmp.root, "gui"))

    @Test
    fun keepsTheListAndRecentAppsAcrossRestarts() {
        catalog().apply {
            update(listOf(calc, foot))
            launched("foot")
            launched("org.gnome.Calculator")
        }
        val again = catalog()
        assertEquals(listOf(foot, calc), again.apps)
        assertEquals(listOf(calc, foot), again.recent)
        assertEquals(calc, again.app("org.gnome.Calculator"))
    }

    @Test
    fun saysWhatANewListChanged() {
        val c = catalog()
        assertEquals(AppCatalog.Change(emptySet(), setOf("foot", "org.gnome.Calculator"), true), c.update(listOf(foot, calc)))
        assertFalse(c.update(listOf(calc, foot)).changed)
        assertEquals(AppCatalog.Change(setOf("foot"), emptySet(), true), c.update(listOf(calc)))
    }

    @Test
    fun recentAppsAreDistinctBoundedAndStillListed() {
        val c = catalog()
        val many = (1..12).map { AppEntry("app$it", "App $it") }
        c.update(many)
        for (a in many) c.launched(a.id)
        c.launched("app3")
        assertEquals("app3", c.recent.first().id)
        assertEquals(c.recent.map { it.id }.distinct(), c.recent.map { it.id })
        assertTrue(c.recent.size < many.size)
        c.update(many.filter { it.id != "app3" })
        assertFalse(c.recent.any { it.id == "app3" })
    }

    @Test
    fun asksForTheIconsItLacksAtEachSize() {
        val c = catalog()
        c.update(listOf(foot, calc, hidden))
        // A hidden entry's icon isn't drawn by the launcher.
        assertEquals(setOf("kfoot" to 48, "kfoot" to 96, "kcalc" to 48, "kcalc" to 96), c.missingIcons(listOf(48, 96)).toSet())
        c.storeIcon("kfoot", 48, byteArrayOf(1, 2, 3))
        // An empty answer: the guest has no icon there, so it isn't asked for again.
        c.storeIcon("kcalc", 48, ByteArray(0))
        assertEquals(setOf("kfoot" to 96, "kcalc" to 96), c.missingIcons(listOf(48, 96)).toSet())
        assertArrayEquals(byteArrayOf(1, 2, 3), c.icon("kfoot", 48)!!.readBytes())
        assertNull(c.icon("kcalc", 48))
        assertNull(c.icon("kfoot", 96))
        // A display density change asks for new sizes.
        assertEquals(setOf("kfoot" to 32, "kcalc" to 32), c.missingIcons(listOf(32)).toSet())
    }

    @Test
    fun dropsIconsNoEntryNames() {
        val c = catalog()
        c.update(listOf(foot, calc))
        c.storeIcon("kfoot", 48, byteArrayOf(1))
        c.storeIcon("kcalc", 48, byteArrayOf(2))
        c.storeIcon("kother", 48, byteArrayOf(3))
        assertNull(c.icon("kother", 48))
        c.update(listOf(calc))
        assertNull(c.icon("kfoot", 48))
        assertArrayEquals(byteArrayOf(2), c.icon("kcalc", 48)!!.readBytes())
        // An app that comes back asks again.
        c.update(listOf(calc, foot))
        assertTrue("kfoot" to 48 in c.missingIcons(listOf(48)))
    }

    @Test
    fun anUnreadableCacheStartsEmpty() {
        File(tmp.root, "gui").mkdirs()
        File(tmp.root, "gui/apps.json").writeText("{not json")
        assertEquals(emptyList<AppEntry>(), catalog().apps)
    }

    @Test
    fun searchMatchesEveryWordAnywhere() {
        assertTrue(foot.matches("term"))
        assertTrue(foot.matches("SHELL foot"))
        assertTrue(foot.matches(""))
        assertFalse(foot.matches("shell calc"))
        assertTrue(calc.matches("gnome"))
    }

    @Test
    fun entriesRoundTripAndBadKeysCountAsNone() {
        val e = foot.copy(actions = listOf(AppAction("new", "New", "knew")))
        assertEquals(e, AppEntry.parse(e.toJson()))
        assertEquals("", AppEntry.parse(e.toJson().put("icon", "../../etc"))!!.icon)
        assertNull(AppEntry.parse(e.toJson().put("id", "a/b")))
    }
}
