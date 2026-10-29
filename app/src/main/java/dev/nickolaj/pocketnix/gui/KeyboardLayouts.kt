package dev.nickolaj.pocketnix.gui

import android.content.Context
import android.hardware.input.InputManager
import android.os.Handler
import android.os.Looper
import android.os.Process
import android.util.Log
import android.view.InputDevice
import android.view.inputmethod.InputMethodInfo
import android.view.inputmethod.InputMethodManager
import android.view.inputmethod.InputMethodSubtype
import java.util.Locale

/** An XKB layout and variant; "" for the compositor's default. */
data class XkbLayout(val layout: String, val variant: String = "") {
    companion object {
        val DEFAULT = XkbLayout("")

        /**
         * The XKB layout for an Android keyboard layout: by its descriptor's
         * name (AOSP's `keyboard_layout_<name>`), else by its language tag
         * and Android layout type (`qwerty`, `dvorak`, …); DEFAULT for neither.
         */
        fun of(descriptor: String?, languageTag: String?, layoutType: String?): XkbLayout {
            descriptor?.substringAfterLast('/')?.removePrefix("keyboard_layout_")?.let { names[it] }?.let { return it }
            val locale = languageTag?.takeIf { it.isNotEmpty() && it != "und" }?.let(Locale::forLanguageTag) ?: return DEFAULT
            val lang = locale.language
            val region = locale.country
            val type = layoutType.orEmpty()
            if (lang == "en" || lang.isEmpty()) {
                return when (type) {
                    "dvorak", "colemak", "workman" -> XkbLayout("us", type)
                    else -> if (region == "GB" || region == "IE") XkbLayout("gb") else XkbLayout("us")
                }
            }
            if (lang == "tr" && type == "turkish_f") return XkbLayout("tr", "f")
            regional["$lang-$region"]?.let { return it }
            return languages[lang] ?: DEFAULT
        }

        private val names = mapOf(
            "english_us" to XkbLayout("us"),
            "english_uk" to XkbLayout("gb"),
            "english_us_intl" to XkbLayout("us", "intl"),
            "english_us_colemak" to XkbLayout("us", "colemak"),
            "english_us_dvorak" to XkbLayout("us", "dvorak"),
            "english_us_workman" to XkbLayout("us", "workman"),
            "german" to XkbLayout("de"),
            "french" to XkbLayout("fr"),
            "french_ca" to XkbLayout("ca"),
            "russian" to XkbLayout("ru"),
            "russian_mac" to XkbLayout("ru", "mac"),
            "spanish" to XkbLayout("es"),
            "spanish_latin" to XkbLayout("latam"),
            "swiss_french" to XkbLayout("ch", "fr"),
            "swiss_german" to XkbLayout("ch"),
            "belgian" to XkbLayout("be"),
            "bulgarian" to XkbLayout("bg"),
            "bulgarian_phonetic" to XkbLayout("bg", "phonetic"),
            "italian" to XkbLayout("it"),
            "danish" to XkbLayout("dk"),
            "norwegian" to XkbLayout("no"),
            "swedish" to XkbLayout("se"),
            "finnish" to XkbLayout("fi"),
            "croatian" to XkbLayout("hr"),
            "czech" to XkbLayout("cz"),
            "czech_qwerty" to XkbLayout("cz", "qwerty"),
            "estonian" to XkbLayout("ee"),
            "hungarian" to XkbLayout("hu"),
            "icelandic" to XkbLayout("is"),
            "brazilian" to XkbLayout("br"),
            "portuguese" to XkbLayout("pt"),
            "slovak" to XkbLayout("sk"),
            "slovenian" to XkbLayout("si"),
            "turkish" to XkbLayout("tr"),
            "turkish_f" to XkbLayout("tr", "f"),
            "ukrainian" to XkbLayout("ua"),
            "arabic" to XkbLayout("ara"),
            "greek" to XkbLayout("gr"),
            "hebrew" to XkbLayout("il"),
            "lithuanian" to XkbLayout("lt"),
            "latvian" to XkbLayout("lv"),
            "persian" to XkbLayout("ir"),
            "azerbaijani" to XkbLayout("az"),
            "polish" to XkbLayout("pl"),
            "belarusian" to XkbLayout("by"),
            "mongolian" to XkbLayout("mn"),
            "georgian" to XkbLayout("ge"),
            "serbian_cyrillic" to XkbLayout("rs"),
            "serbian_latin" to XkbLayout("rs", "latin"),
            "macedonian_cyrillic" to XkbLayout("mk"),
            "montenegrin_cyrillic" to XkbLayout("me", "cyrillic"),
            "montenegrin_latin" to XkbLayout("me"),
            "dutch" to XkbLayout("nl"),
            "thai_kedmanee" to XkbLayout("th"),
            "thai_pattachote" to XkbLayout("th", "pat"),
        )

        private val regional = mapOf(
            "de-CH" to XkbLayout("ch"),
            "de-AT" to XkbLayout("at"),
            "fr-CA" to XkbLayout("ca"),
            "fr-CH" to XkbLayout("ch", "fr"),
            "fr-BE" to XkbLayout("be"),
            "nl-BE" to XkbLayout("be"),
            "pt-BR" to XkbLayout("br"),
            "es-419" to XkbLayout("latam"),
            "es-MX" to XkbLayout("latam"),
            "es-AR" to XkbLayout("latam"),
            "es-US" to XkbLayout("latam"),
        )

        private val languages = mapOf(
            "de" to "de", "fr" to "fr", "es" to "es", "it" to "it", "pt" to "pt", "nl" to "nl", "da" to "dk",
            "nb" to "no", "nn" to "no", "no" to "no", "sv" to "se", "fi" to "fi", "is" to "is", "et" to "ee",
            "lv" to "lv", "lt" to "lt", "pl" to "pl", "cs" to "cz", "sk" to "sk", "sl" to "si", "hr" to "hr",
            "sr" to "rs", "bs" to "ba", "mk" to "mk", "bg" to "bg", "ro" to "ro", "hu" to "hu", "tr" to "tr",
            "el" to "gr", "he" to "il", "iw" to "il", "ar" to "ara", "fa" to "ir", "ru" to "ru", "uk" to "ua",
            "be" to "by", "ka" to "ge", "hy" to "am", "az" to "az", "kk" to "kz", "mn" to "mn", "ja" to "jp",
            "ko" to "kr", "th" to "th", "vi" to "vn",
        ).mapValues { XkbLayout(it.value) }
    }
}

/**
 * Android's physical keyboard layout, for the guest's keymap (gui-protocol.md,
 * Keyboard layout): that of the keyboard last typed on, or else of any
 * physical keyboard attached. [onChange] runs on the main thread.
 */
internal class KeyboardLayouts(private val context: Context, private val onChange: (XkbLayout) -> Unit) {
    private val inputManager = context.getSystemService(InputManager::class.java)
    private val main = Handler(Looper.getMainLooper())

    // The keyboard last typed on; -1 until one is.
    private var device = -1

    @Volatile var current = XkbLayout.DEFAULT
        private set

    init {
        // A layout chosen in the settings reloads the device's key map, which is a change.
        inputManager.registerInputDeviceListener(
            object : InputManager.InputDeviceListener {
                override fun onInputDeviceAdded(deviceId: Int) = update()
                override fun onInputDeviceRemoved(deviceId: Int) = update()
                override fun onInputDeviceChanged(deviceId: Int) = update()
            },
            main,
        )
        main.post { update() }
    }

    /** A key event came from [deviceId]. */
    fun typedOn(deviceId: Int) {
        if (deviceId == device) return
        device = deviceId
        update()
    }

    private fun update() = guard("keyboard layout") {
        val dev = InputDevice.getDevice(device)?.takeIf(::physical)
            ?: InputDevice.getDeviceIds().asSequence().mapNotNull(InputDevice::getDevice).firstOrNull(::physical)
        val l = dev?.let(::layout) ?: XkbLayout.DEFAULT
        if (l == current) return@guard
        Log.i(TAG, "keyboard layout ${dev?.name}: $l")
        current = l
        onChange(l)
    }

    private fun physical(d: InputDevice): Boolean =
        !d.isVirtual && d.keyboardType == InputDevice.KEYBOARD_TYPE_ALPHABETIC

    /** The layout Android uses for [d]: the one selected for it, else what it and the IME suggest. */
    private fun layout(d: InputDevice): XkbLayout {
        val imm = context.getSystemService(InputMethodManager::class.java)
        val subtype = imm.currentInputMethodSubtype
        val selected = runCatching { selected(d, imm.currentInputMethodInfo, subtype) }
            .onFailure { Log.d(TAG, "no selected layout for ${d.name}: $it") }.getOrNull()
        if (selected != null) {
            val (descriptor, tag, type) = selected
            XkbLayout.of(descriptor, tag, type).takeIf { it != XkbLayout.DEFAULT }?.let { return it }
        }
        // What the keyboard says of itself (its HID country code), hidden API too.
        val tag = runCatching { InputDevice::class.java.getMethod("getKeyboardLanguageTag").invoke(d) as String? }.getOrNull()
        val type = runCatching { InputDevice::class.java.getMethod("getKeyboardLayoutType").invoke(d) as String? }.getOrNull()
        XkbLayout.of(null, tag, type).takeIf { it != XkbLayout.DEFAULT }?.let { return it }
        return XkbLayout.of(null, subtype?.physicalKeyboardHintLanguageTag?.toLanguageTag(), subtype?.physicalKeyboardHintLayoutType)
    }

    /**
     * The layout selected for [d] in the settings, through InputManager's
     * hidden API (exempted in PocketnixApp): its descriptor, language tag and
     * layout type. Null when Android reports none.
     */
    private fun selected(d: InputDevice, ime: InputMethodInfo?, subtype: InputMethodSubtype?): Triple<String, String?, String?>? {
        val identifier = InputDevice::class.java.getMethod("getIdentifier").invoke(d)
        val idClass = Class.forName("android.hardware.input.InputDeviceIdentifier")
        val im = InputManager::class.java
        val result = runCatching {
            im.getMethod("getKeyboardLayoutForInputDevice", idClass, Int::class.javaPrimitiveType, InputMethodInfo::class.java, InputMethodSubtype::class.java)
                .invoke(inputManager, identifier, Process.myUid() / PER_USER_RANGE, ime, subtype)
        }.getOrElse {
            im.getMethod("getCurrentKeyboardLayoutForInputDevice", idClass).invoke(inputManager, identifier)
        }
        // A String until Android 15, then a KeyboardLayoutSelectionResult.
        val descriptor = when (result) {
            null -> null
            is String -> result
            else -> result.javaClass.getMethod("getLayoutDescriptor").invoke(result) as String?
        } ?: return null
        val layout = im.getMethod("getKeyboardLayout", String::class.java).invoke(inputManager, descriptor) ?: return Triple(descriptor, null, null)
        val locales = runCatching { layout.javaClass.getMethod("getLocales").invoke(layout) as android.os.LocaleList? }.getOrNull()
        val type = runCatching { layout.javaClass.getMethod("getLayoutType").invoke(layout) as String? }.getOrNull()
        return Triple(descriptor, locales?.takeIf { !it.isEmpty }?.get(0)?.toLanguageTag(), type)
    }

    private companion object {
        const val TAG = "KeyboardLayouts"
        const val PER_USER_RANGE = 100000
    }
}
