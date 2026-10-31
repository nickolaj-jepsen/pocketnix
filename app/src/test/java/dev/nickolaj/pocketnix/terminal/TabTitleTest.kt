package dev.nickolaj.pocketnix.terminal

import org.junit.Assert.assertEquals
import org.junit.Test

class TabTitleTest {
    @Test
    fun theUsersTitleWinsOverTheShellsOverTheProfile() {
        assertEquals("work", tabTitle("work", "vim notes.md", "shell"))
        assertEquals("vim notes.md", tabTitle("", "vim notes.md", "shell"))
        assertEquals("shell", tabTitle("", null, "shell"))
    }

    @Test
    fun anEmptyOrBlankTitleCountsAsUnset() {
        assertEquals("vim notes.md", tabTitle(" ", "vim notes.md", "shell"))
        assertEquals("shell", tabTitle("", "  ", "shell"))
    }

    @Test
    fun aCommandTabIsNamedForItsProgramAndAProfileTabForItsProfile() {
        assertEquals("hello", Tab.Kind.Pty("shell", "/nix/store/x-hello/bin/hello").name)
        assertEquals("dev", Tab.Kind.Pty("dev").name)
    }
}
