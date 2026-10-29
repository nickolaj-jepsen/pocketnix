package dev.nickolaj.pocketnix.gui

import dev.nickolaj.pocketnix.agent.FakeAgent
import dev.nickolaj.pocketnix.agent.Frames
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit
import org.json.JSONObject
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class GuiLinkTest {
    private val events = LinkedBlockingQueue<Any>()

    private val listener = object : GuiLink.Listener {
        override fun connected(hello: GuiMessage.Hello) {
            events.add(hello)
        }

        override fun message(msg: GuiMessage) {
            events.add(msg)
        }

        override fun disconnected(error: Throwable?) {
            events.add("disconnected")
        }
    }

    private fun next(): Any = events.poll(5, TimeUnit.SECONDS) ?: throw AssertionError("nothing from the link")

    @Test
    fun saysHelloAndReconnectsAfterADrop() {
        val hellos = LinkedBlockingQueue<JSONObject>()
        val acks = LinkedBlockingQueue<ByteArray>()
        val agent = FakeAgent { conn ->
            hellos.add(conn.hello)
            val h = Frames.read(conn.input)!!
            assertEquals(Gui.JSON, h.type)
            val appHello = JSONObject(String(h.payload))
            hellos.add(appHello)
            conn.output.write(CompositorFrames().hello("i-1").toplevel(4, "foot").ready().bytes())
            conn.output.flush()
            // The link's first message after the replay, then a drop.
            Frames.read(conn.input)?.let { acks.add(Frames.encode(it.type, it.payload)) }
        }
        agent.use {
            val link = GuiLink("pocketnix test", listener)
            link.start(agent.dialer, hashes = true)
            try {
                for (round in 1..2) {
                    assertEquals("gui", hellos.poll(5, TimeUnit.SECONDS)!!.getString("stream"))
                    val app = hellos.poll(5, TimeUnit.SECONDS)!!
                    assertEquals("hello", app.getString("type"))
                    assertEquals(Gui.VERSION, app.getInt("version"))
                    assertEquals(Gui.MIN_VERSION, app.getInt("minVersion"))
                    assertTrue(app.getBoolean("hashes"))

                    assertEquals(GuiMessage.Hello(2, "test", "i-1"), next())
                    assertEquals(4, (next() as GuiMessage.Toplevel).id)
                    assertEquals(GuiMessage.Ready, next())
                    link.send(Gui.presented(4, round))
                    assertArrayEquals(Gui.presented(4, round), acks.poll(5, TimeUnit.SECONDS))
                    assertEquals("disconnected", next())
                }
            } finally {
                link.stop()
            }
        }
    }

    @Test
    fun aRefusalIsRetried() {
        var refused = 0
        val agent = FakeAgent { conn ->
            Frames.read(conn.input)
            if (refused++ == 0) {
                conn.output.write(Gui.json(JSONObject().put("type", "error").put("message", "busy")))
            } else {
                conn.output.write(CompositorFrames().hello().ready().bytes())
            }
            conn.output.flush()
            Thread.sleep(200)
        }
        agent.use {
            val link = GuiLink("pocketnix test", listener)
            link.start(agent.dialer)
            try {
                assertTrue(next() is GuiMessage.Hello)
                assertEquals(GuiMessage.Ready, next())
                assertTrue(agent.connections.get() >= 2)
            } finally {
                link.stop()
            }
        }
    }
}
