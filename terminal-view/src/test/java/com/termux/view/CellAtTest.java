package com.termux.view;

import static org.junit.Assert.assertArrayEquals;
import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNull;

import com.termux.terminal.TerminalBuffer;
import com.termux.terminal.TerminalEmulator;
import com.termux.terminal.TerminalOutput;

import java.nio.charset.StandardCharsets;

import org.junit.Test;

public class CellAtTest {
    // A 10-pixel cell whose first line's baseline sits at 12 pixels.
    private static final float WIDTH = 10f;
    private static final int ASCENT = 2;
    private static final int SPACING = 10;

    @Test
    public void scrolledBackRowIsNegative() {
        // Scrolled up 2 rows: the top line of the view is history row -2.
        assertArrayEquals(new int[]{3, -2}, TerminalView.cellAt(35f, 5f, WIDTH, ASCENT, SPACING, -2, 10, 3, 2));
        assertArrayEquals(new int[]{0, 0}, TerminalView.cellAt(5f, 25f, WIDTH, ASCENT, SPACING, -2, 10, 3, 2));
    }

    @Test
    public void outsideTheBufferIsNull() {
        assertNull(TerminalView.cellAt(105f, 5f, WIDTH, ASCENT, SPACING, 0, 10, 3, 0));
        assertNull(TerminalView.cellAt(5f, 35f, WIDTH, ASCENT, SPACING, 0, 10, 3, 0));
        assertNull(TerminalView.cellAt(-5f, 5f, WIDTH, ASCENT, SPACING, 0, 10, 3, 0));
        // Further up than the history goes.
        assertNull(TerminalView.cellAt(5f, 5f, WIDTH, ASCENT, SPACING, -3, 10, 3, 2));
    }

    @Test
    public void linkInScrollbackIsFound() {
        TerminalEmulator emulator = new TerminalEmulator(new NullOutput(), 10, 3, 10, 10, 10, null);
        byte[] out = "\033]8;;https://a.example\033\\link\033]8;;\033\\\r\n\r\n\r\n\r\n".getBytes(StandardCharsets.UTF_8);
        emulator.append(out, out.length);
        TerminalBuffer screen = emulator.getScreen();
        // The link scrolled 2 rows into history; the view scrolled back to it.
        int[] cell = TerminalView.cellAt(15f, 5f, WIDTH, ASCENT, SPACING, -2, 10, 3, screen.getActiveTranscriptRows());
        assertEquals("https://a.example", screen.getHyperlinkAt(cell[1], cell[0]));
    }

    private static final class NullOutput extends TerminalOutput {
        @Override public void write(byte[] data, int offset, int count) {}
        @Override public void titleChanged(String oldTitle, String newTitle) {}
        @Override public void onCopyTextToClipboard(String text) {}
        @Override public void onPasteTextFromClipboard() {}
        @Override public void onBell() {}
        @Override public void onColorsChanged() {}
    }
}
