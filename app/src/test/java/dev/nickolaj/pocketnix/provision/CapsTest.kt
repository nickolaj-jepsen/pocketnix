package dev.nickolaj.pocketnix.provision

import dev.nickolaj.pocketnix.engine.Launched
import java.net.URLDecoder
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class CapsTest {
    private val boxed = Boolean::class.javaObjectType
    private val primitive = Boolean::class.javaPrimitiveType!!

    private val pixel = Caps(
        capabilities = 3,
        manager = true,
        gpuFlagTypes = List(5) { boxed },
        rawConfigBinder = true,
        autoMemoryBalloon = true,
        launched = Launched(gpu = "gfxstream", gpuAsked = true, gpuRefused = false, guestRam = true, virtioSnd = false),
        appVersion = "0.4.0-debug",
        memoryMb = 4096
    )

    // What `pocketnix`'s provider.call reads from `content call`'s output (its bundleField).
    private fun parse(pairs: List<Pair<String, String>>): Map<String, String> {
        val printed = "Result: Bundle[{" + (pairs + ("ok" to "true")).joinToString(", ") { (k, v) -> "$k=$v" } + "}]"
        return Regex("""(\w+)=([^,}]*)""").findAll(printed).associate { it.groupValues[1] to it.groupValues[2].trim() }
    }

    @Test
    fun theAnswerParsesBackToEveryKey() {
        val got = parse(capsAnswer(pixel))
        assertEquals("3", got["capabilities"])
        assertEquals("true", got["manager"])
        assertEquals("boxed", got["gpuConfig"])
        assertEquals("true", got["rawConfigBinder"])
        assertEquals("true", got["autoMemoryBalloon"])
        assertEquals("0.4.0-debug", got["appVersion"])
        assertEquals("4096", got["memoryMb"])
        assertEquals("true", got["ok"])
    }

    @Test
    fun theEngineLineIsUrlEncoded() {
        val engine = parse(capsAnswer(pixel))["engine"]!!
        assertFalse(engine, engine.contains(' ') || engine.contains('='))
        val decoded = URLDecoder.decode(engine, "UTF-8").split(' ')
        assertTrue(decoded.toString(), decoded.containsAll(listOf("engine", "gpu=gfxstream", "guestRam=true")))
        assertEquals("engine+launched%3Dnone", parse(capsAnswer(pixel.copy(launched = null)))["engine"])
    }

    @Test
    fun noValueHoldsACommaOrBrace() {
        val odd = pixel.copy(appVersion = "1.0,rc}2")
        for ((k, v) in capsAnswer(odd)) assertFalse("$k=$v", v.contains(',') || v.contains('}'))
        assertEquals("1.0,rc}2", URLDecoder.decode(parse(capsAnswer(odd))["appVersion"], "UTF-8"))
    }

    @Test
    fun gpuConfigNamesTheSettersBooleanType() {
        assertEquals("absent", gpuConfig(null))
        assertEquals("boxed", gpuConfig(listOf(boxed, boxed)))
        assertEquals("primitive", gpuConfig(listOf(primitive, primitive)))
        assertEquals("mixed", gpuConfig(listOf(boxed, primitive)))
        assertEquals("mixed", gpuConfig(listOf(boxed, null)))
        assertEquals("mixed", gpuConfig(emptyList()))
    }
}
