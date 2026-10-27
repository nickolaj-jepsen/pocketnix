package dev.nickolaj.pocketnix.gui

/**
 * The compositor's windows and surfaces as the app mirrors them, across
 * connections to the same compositor instance. Applies the protocol's
 * messages and checks them; rendering reads the result. Not thread-safe.
 */
class GuiState(private val unpacker: Unpacker) {
    class Window(val id: Int, var toplevel: GuiMessage.Toplevel) {
        /** Announced again by this connection's replay. */
        internal var replayed = true

        /** The newest commit seq applied to this window on this connection. */
        var lastSeq = 0
            internal set
    }

    class Surface(val id: Int, var window: Int, var role: String, var parent: Int) {
        val content = SurfaceContent()
        val damage = DamageHistory()
        internal var replayed = true

        /** Placement from its last commit, in window pixels; see COMMIT in gui-protocol.md. */
        var x = 0
            internal set
        var y = 0
            internal set
        var z = 0
            internal set
        var dstWidth = 0
            internal set
        var dstHeight = 0
            internal set
        var srcX = 0
            internal set
        var srcY = 0
            internal set
        var srcWidth = 0
            internal set
        var srcHeight = 0
            internal set
        var transform = 0
            internal set

        /** Whether it has content to show. */
        val mapped: Boolean get() = content.pixels != null
    }

    /** The compositor process this state belongs to (`hello.instance`). */
    var instance: String? = null
        private set
    val windows = LinkedHashMap<Int, Window>()
    val surfaces = HashMap<Int, Surface>()

    /** Called for every commit carrying a hash: the compositor's and the app's. */
    var onHash: ((surface: Int, seq: Int, expected: Long, actual: Long) -> Unit)? = null

    /**
     * Starts a connection: everything is replayed from here. Returns the
     * windows dropped because [hello] is another compositor instance, whose
     * clients died with the old one.
     */
    fun hello(hello: GuiMessage.Hello): List<Window> {
        val dropped = if (instance != null && instance != hello.instance) {
            windows.values.toList().also {
                windows.clear()
                surfaces.clear()
            }
        } else {
            emptyList()
        }
        instance = hello.instance
        for (w in windows.values) {
            w.replayed = false
            w.lastSeq = 0
        }
        // Replay brings FULL commits; content from the last connection is only shown until then.
        for (s in surfaces.values) {
            s.replayed = false
            s.content.clear()
        }
        return dropped
    }

    /** Returns true for a window that's new. */
    fun toplevel(t: GuiMessage.Toplevel): Boolean {
        val w = windows[t.id]
        if (w != null) {
            w.toplevel = t
            w.replayed = true
            return false
        }
        windows[t.id] = Window(t.id, t)
        return true
    }

    /** Forgets the window and its surfaces; returns the surfaces dropped. */
    fun toplevelRemoved(id: Int): List<Surface> {
        windows.remove(id) ?: return emptyList()
        val gone = surfaces.values.filter { it.window == id }
        for (s in gone) surfaces.remove(s.id)
        return gone
    }

    /** Returns the surface's previous window when it moved from another, else null. */
    fun surface(m: GuiMessage.Surface): Int? {
        if (m.window !in windows) throw GuiProtocolException("surface ${m.id.u} joins unknown window ${m.window.u}")
        val s = surfaces[m.id]
        if (s == null) {
            surfaces[m.id] = Surface(m.id, m.window, m.role, m.parent)
            return null
        }
        val old = s.window
        s.window = m.window
        s.role = m.role
        s.parent = m.parent
        s.replayed = true
        return old.takeIf { it != m.window }
    }

    fun surfaceRemoved(id: Int): Surface? = surfaces.remove(id)

    /** Ends the replay: drops what it didn't announce, and returns the windows and surfaces dropped. */
    fun ready(): Pair<List<Window>, List<Surface>> {
        val gone = windows.values.filter { !it.replayed }
        for (w in gone) windows.remove(w.id)
        val goneSurfaces = surfaces.values.filter { !it.replayed || it.window !in windows }
        for (s in goneSurfaces) surfaces.remove(s.id)
        return gone to goneSurfaces
    }

    /** Applies a group's commits in order; throws [GuiProtocolException] for one that doesn't apply. */
    fun apply(group: GuiMessage.Group) {
        for (c in group.commits) {
            val s = surfaces[c.surface] ?: throw GuiProtocolException("commit ${c.seq.u} for unknown surface ${c.surface.u}")
            if (s.window != c.window) throw GuiProtocolException("commit ${c.seq.u} names window ${c.window.u}; surface ${s.id.u} is in ${s.window.u}")
            val w = windows[c.window] ?: throw GuiProtocolException("commit ${c.seq.u} for unknown window ${c.window.u}")
            when {
                c.hidden -> {
                    s.content.clear()
                    s.damage.record(null)
                }
                c.buffer -> {
                    s.content.apply(c, unpacker)
                    s.damage.record(if (c.full || c.scrolled) null else c.rects)
                }
            }
            s.x = c.x
            s.y = c.y
            s.z = c.z
            s.dstWidth = c.dstWidth
            s.dstHeight = c.dstHeight
            s.srcX = c.srcX
            s.srcY = c.srcY
            s.srcWidth = c.srcWidth
            s.srcHeight = c.srcHeight
            s.transform = c.transform
            w.lastSeq = c.seq
            if (c.hashed && c.buffer) onHash?.invoke(s.id, c.seq, c.hash, s.content.hash())
        }
    }
}
