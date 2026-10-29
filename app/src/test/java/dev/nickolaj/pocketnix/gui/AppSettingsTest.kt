package dev.nickolaj.pocketnix.gui

import dev.nickolaj.pocketnix.agent.GuiAppSettings
import dev.nickolaj.pocketnix.agent.GuiConfig
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class AppSettingsTest {
    private class MemoryStore(val saved: MutableMap<String, AppOverride> = HashMap()) : OverrideStore {
        override fun load(): Map<String, AppOverride> = saved.toMap()

        override fun save(app: String, o: AppOverride?) {
            if (o == null) saved.remove(app) else saved[app] = o
        }
    }

    private val calc = "org.gnome.Calculator"
    private val foot = "foot"

    private fun config(defaults: GuiAppSettings = GuiAppSettings(null, null), vararg apps: Pair<String, GuiAppSettings>) = GuiConfig(enable = true, defaults = defaults, appDefaults = apps.toMap())

    @Test
    fun withoutConfigOrChangesAnAppIsPhoneAndTouch() {
        assertEquals(AppChoice(SizePreset.PHONE, InputMode.TOUCH, overridden = false), AppSettings(MemoryStore()).of(calc))
    }

    @Test
    fun anAppInheritsWhatItsConfigLeavesOutFromTheDefaults() {
        val s = AppSettings(MemoryStore())
        s.config(config(GuiAppSettings("desktop", "pointer"), calc to GuiAppSettings("compact", null)))
        assertEquals(AppChoice(SizePreset.COMPACT, InputMode.POINTER, overridden = false), s.of(calc))
        assertEquals(AppChoice(SizePreset.DESKTOP, InputMode.POINTER, overridden = false), s.of(foot))
    }

    @Test
    fun aValueTheAppDoesntKnowIsIgnored() {
        val s = AppSettings(MemoryStore())
        s.config(config(GuiAppSettings("compact", "trackpad"), calc to GuiAppSettings("huge", "stylus")))
        assertEquals(AppChoice(SizePreset.COMPACT, InputMode.TRACKPAD, overridden = false), s.of(calc))
    }

    @Test
    fun theUsersChangeWinsOverTheConfigForThatAppOnly() {
        val s = AppSettings(MemoryStore())
        s.config(config(GuiAppSettings("compact", null), calc to GuiAppSettings("desktop", "pointer")))
        s.setSize(calc, SizePreset.PHONE)
        assertEquals(AppChoice(SizePreset.PHONE, InputMode.POINTER, overridden = true), s.of(calc))
        assertEquals(AppChoice(SizePreset.COMPACT, InputMode.TOUCH, overridden = false), s.of(foot))
    }

    @Test
    fun resetGoesBackToTheConfig() {
        val s = AppSettings(MemoryStore())
        s.config(config(GuiAppSettings(null, null), calc to GuiAppSettings("desktop", null)))
        s.setSize(calc, SizePreset.COMPACT)
        s.setInput(calc, InputMode.TRACKPAD)
        s.reset(calc)
        assertEquals(AppChoice(SizePreset.DESKTOP, InputMode.TOUCH, overridden = false), s.of(calc))
    }

    @Test
    fun changesOutliveTheProcessAndAConfigChange() {
        val store = MemoryStore()
        AppSettings(store).setInput(foot, InputMode.POINTER)
        val again = AppSettings(store)
        again.config(config(GuiAppSettings(null, null), foot to GuiAppSettings("compact", "trackpad")))
        assertEquals(AppChoice(SizePreset.COMPACT, InputMode.POINTER, overridden = true), again.of(foot))
    }

    @Test
    fun aMissingConfigKeepsTheLastOne() {
        val s = AppSettings(MemoryStore())
        s.config(config(GuiAppSettings("desktop", null)))
        s.config(null)
        assertEquals(SizePreset.DESKTOP, s.of(foot).size)
    }

    @Test
    fun everyChangeIsAnnounced() {
        val s = AppSettings(MemoryStore())
        var changes = 0
        s.onChange = { changes++ }
        s.config(config(GuiAppSettings("compact", null)))
        s.config(config(GuiAppSettings("compact", null)))
        s.setSize(calc, SizePreset.DESKTOP)
        s.setSize(calc, SizePreset.DESKTOP)
        s.reset(calc)
        s.reset(calc)
        assertEquals("the config, a change and a reset; repeats change nothing", 3, changes)
    }

    @Test
    fun factorsNameOnlyTheAppsThatDifferFromTheDefault() {
        val s = AppSettings(MemoryStore())
        s.config(config(GuiAppSettings("compact", null), calc to GuiAppSettings("desktop", null), "same" to GuiAppSettings("compact", "pointer")))
        s.setSize(foot, SizePreset.PHONE)
        assertEquals(SizePreset.COMPACT.factor to mapOf(calc to SizePreset.DESKTOP.factor, foot to SizePreset.PHONE.factor), s.factors())
        s.reset(foot)
        s.setSize(calc, SizePreset.COMPACT)
        assertEquals(SizePreset.COMPACT.factor to emptyMap<String, Float>(), s.factors())
    }

    @Test
    fun overriddenIsTrueOnlyWhileAChangeIsStored() {
        val s = AppSettings(MemoryStore())
        assertFalse(s.of(calc).overridden)
        s.setInput(calc, InputMode.TOUCH)
        assertTrue("a change to what the config gives is still the user's", s.of(calc).overridden)
    }
}
