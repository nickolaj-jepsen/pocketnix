package com.termux.terminal;

import androidx.annotation.Nullable;

import java.io.IOException;

/**
 * Where a {@link TerminalSession}'s bytes come from and go to: a PTY stream to
 * a guest agent, a VM console, or a test double.
 */
public interface TerminalBackend {

    /**
     * Starts the session at its first known size. From then on the backend
     * delivers output through {@code sink}, from any thread, and calls
     * {@link Sink#onExit} exactly once when the session ends (including when
     * it fails to start).
     */
    void start(int columns, int rows, int cellWidthPixels, int cellHeightPixels, Sink sink);

    /** Sends terminal input. Called on the session's writer thread; may block. */
    void write(byte[] data, int offset, int count) throws IOException;

    /** The terminal's size changed. Called on the main thread; must not block. */
    void resize(int columns, int rows, int cellWidthPixels, int cellHeightPixels);

    /** Ends the session, idempotently; a running backend still reports {@link Sink#onExit}. */
    void close();

    interface Sink {
        /** Output to show. Blocks while the terminal is behind; false once the session is gone. */
        boolean onOutput(byte[] data, int offset, int count);

        /**
         * The session ended. {@code status} is the exit code, minus the signal
         * number that killed it, or null if unknown; {@code reason} is shown
         * instead of a status when not null.
         */
        void onExit(@Nullable Integer status, @Nullable String reason);
    }
}
