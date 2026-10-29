package dev.nickolaj.pocketnix.gui

import android.text.InputType
import android.view.inputmethod.EditorInfo
import org.junit.Assert.assertEquals
import org.junit.Test

class KeyboardLayoutsTest {
    @Test
    fun aSelectedLayoutGoesByItsAospName() {
        val prefix = "com.android.inputdevices/com.android.inputdevices.InputDeviceReceiver/keyboard_layout_"
        assertEquals(XkbLayout("dk"), XkbLayout.of(prefix + "danish", "da", "qwerty"))
        assertEquals(XkbLayout("ch", "fr"), XkbLayout.of(prefix + "swiss_french", null, null))
        assertEquals(XkbLayout("us", "dvorak"), XkbLayout.of(prefix + "english_us_dvorak", "en-US", "dvorak"))
    }

    @Test
    fun anUnknownLayoutGoesByItsLanguageAndType() {
        assertEquals(XkbLayout("de"), XkbLayout.of("vendor/layout/whatever", "de-DE", "qwertz"))
        assertEquals(XkbLayout("ch"), XkbLayout.of(null, "de-CH", "qwertz"))
        assertEquals(XkbLayout("gb"), XkbLayout.of(null, "en-GB", "qwerty"))
        assertEquals(XkbLayout("us", "colemak"), XkbLayout.of(null, "en-US", "colemak"))
        assertEquals(XkbLayout("tr", "f"), XkbLayout.of(null, "tr", "turkish_f"))
        assertEquals(XkbLayout("latam"), XkbLayout.of(null, "es-419", null))
        assertEquals(XkbLayout("no"), XkbLayout.of(null, "nb-NO", null))
    }

    @Test
    fun nothingKnownIsTheCompositorsDefault() {
        assertEquals(XkbLayout.DEFAULT, XkbLayout.of(null, null, null))
        assertEquals(XkbLayout.DEFAULT, XkbLayout.of(null, "und", "qwerty"))
        assertEquals(XkbLayout.DEFAULT, XkbLayout.of(null, "zz", null))
    }

    @Test
    fun aTextInputsContentTypeBecomesTheEditorsType() {
        val (email, _) = WindowIme.editorType("email", setOf("completion"))
        assertEquals(InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_EMAIL_ADDRESS or InputType.TYPE_TEXT_FLAG_AUTO_COMPLETE, email)
        val (pin, _) = WindowIme.editorType("pin", emptySet())
        assertEquals(InputType.TYPE_CLASS_NUMBER or InputType.TYPE_NUMBER_VARIATION_PASSWORD, pin)
        val (text, options) = WindowIme.editorType("normal", setOf("spellcheck", "auto-capitalization", "multiline", "sensitive-data"))
        assertEquals(
            InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_FLAG_AUTO_CORRECT or InputType.TYPE_TEXT_FLAG_CAP_SENTENCES or
                InputType.TYPE_TEXT_FLAG_MULTI_LINE or InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS,
            text
        )
        assertEquals(EditorInfo.IME_FLAG_NO_PERSONALIZED_LEARNING, options and EditorInfo.IME_FLAG_NO_PERSONALIZED_LEARNING)
        val (hidden, _) = WindowIme.editorType("normal", setOf("hidden-text"))
        assertEquals(InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD, hidden)
    }
}
