package com.termux.terminal;

import android.annotation.SuppressLint;
import android.os.Handler;
import android.os.Looper;
import android.os.Message;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.UUID;
import java.util.concurrent.LinkedBlockingQueue;
import java.util.concurrent.atomic.AtomicLong;

/**
 * A terminal session: a {@link TerminalBackend} coupled to a terminal emulator.
 * <p>
 * The backend starts when the size is first made known by a call to
 * {@link #updateSize(int, int, int, int)}. All terminal emulation and callback
 * methods run on the main thread.
 * <p>
 * The session may be ended with {@link #finishIfRunning()}.
 * <p>
 * NOTE: The terminal session may outlive the EmulatorView, so be careful with callbacks!
 */
public class TerminalSession extends TerminalOutput {

    private static final int MSG_NEW_INPUT = 1;
    private static final int MSG_PROCESS_EXITED = 4;

    public final String mHandle = UUID.randomUUID().toString();

    TerminalEmulator mEmulator;

    /**
     * A queue written to from a separate thread when the backend outputs, and read by main thread to process by
     * terminal emulator.
     */
    final ByteQueue mProcessToTerminalIOQueue = new ByteQueue(64 * 1024);
    /**
     * Input from the main thread (keys, pastes, emulator replies) for the writer thread to hand to the backend.
     * Unbounded up to {@link #MAX_PENDING_INPUT}, so the main thread never blocks on a backend that doesn't read
     * (a console before the guest's tty is up, a PTY tab before its Hello): a bounded queue blocked it into an ANR.
     */
    private final LinkedBlockingQueue<byte[]> mInputQueue = new LinkedBlockingQueue<>();
    private final AtomicLong mPendingInput = new AtomicLong();
    private static final byte[] END_OF_INPUT = new byte[0];
    private static final long MAX_PENDING_INPUT = 16 << 20;
    /** Buffer to write translate code points into utf8 before writing them */
    private final byte[] mUtf8InputBuffer = new byte[5];

    /** Callback which gets notified when a session finishes or changes title. */
    TerminalSessionClient mClient;

    private final TerminalBackend mBackend;
    private boolean mRunning = true;
    /** The exit status, only valid once not {@link #isRunning()}; null if unknown. */
    private Integer mExitStatus;

    /** Set by the application for user identification of session, not by terminal. */
    public String mSessionName;

    final Handler mMainThreadHandler = new MainThreadHandler();

    private final Integer mTranscriptRows;

    private static final String LOG_TAG = "TerminalSession";

    public TerminalSession(TerminalBackend backend, Integer transcriptRows, TerminalSessionClient client) {
        this.mBackend = backend;
        this.mTranscriptRows = transcriptRows;
        this.mClient = client;
    }

    /**
     * @param client The {@link TerminalSessionClient} interface implementation to allow
     *               for communication between {@link TerminalSession} and its client.
     */
    public void updateTerminalSessionClient(TerminalSessionClient client) {
        mClient = client;

        if (mEmulator != null)
            mEmulator.updateTerminalSessionClient(client);
    }

    /** Inform the backend of the new size and reflow or initialize the emulator. */
    public void updateSize(int columns, int rows, int cellWidthPixels, int cellHeightPixels) {
        if (mEmulator == null) {
            initializeEmulator(columns, rows, cellWidthPixels, cellHeightPixels);
        } else {
            if (isRunning()) mBackend.resize(columns, rows, cellWidthPixels, cellHeightPixels);
            mEmulator.resize(columns, rows, cellWidthPixels, cellHeightPixels);
        }
    }

    /** The terminal title as set through escape sequences or null if none set. */
    public String getTitle() {
        return (mEmulator == null) ? null : mEmulator.getTitle();
    }

    /**
     * Set the terminal emulator's window size and start terminal emulation.
     *
     * @param columns The number of columns in the terminal window.
     * @param rows    The number of rows in the terminal window.
     */
    public void initializeEmulator(int columns, int rows, int cellWidthPixels, int cellHeightPixels) {
        mEmulator = new TerminalEmulator(this, columns, rows, cellWidthPixels, cellHeightPixels, mTranscriptRows, mClient);

        new Thread("TermSessionOutputWriter[" + mHandle + "]") {
            @Override
            public void run() {
                try {
                    while (true) {
                        byte[] chunk = mInputQueue.take();
                        if (chunk == END_OF_INPUT) return;
                        mPendingInput.addAndGet(-chunk.length);
                        mBackend.write(chunk, 0, chunk.length);
                    }
                } catch (IOException | InterruptedException e) {
                    // The backend reports the end through onExit.
                }
            }
        }.start();

        mBackend.start(columns, rows, cellWidthPixels, cellHeightPixels, new TerminalBackend.Sink() {
            @Override
            public boolean onOutput(byte[] data, int offset, int count) {
                // ByteQueue rejects empty writes.
                if (count == 0) return isRunning();
                if (!mProcessToTerminalIOQueue.write(data, offset, count)) return false;
                // One pending message is enough: the main thread drains up to 64 KB at a time.
                if (!mMainThreadHandler.hasMessages(MSG_NEW_INPUT)) {
                    mMainThreadHandler.sendEmptyMessage(MSG_NEW_INPUT);
                }
                return true;
            }

            @Override
            public void onExit(Integer status, String reason) {
                mMainThreadHandler.sendMessage(mMainThreadHandler.obtainMessage(MSG_PROCESS_EXITED, new Exit(status, reason)));
            }
        });
    }

    private static final class Exit {
        final Integer status;
        final String reason;

        Exit(Integer status, String reason) {
            this.status = status;
            this.reason = reason;
        }
    }

    /** Write data to the backend. */
    @Override
    public void write(byte[] data, int offset, int count) {
        if (count <= 0 || !isRunning()) return;
        if (mPendingInput.addAndGet(count) > MAX_PENDING_INPUT) {
            mPendingInput.addAndGet(-count);
            Logger.logWarn(mClient, LOG_TAG, "dropping " + count + " bytes of input: the session isn't reading");
            return;
        }
        mInputQueue.offer(Arrays.copyOfRange(data, offset, offset + count));
    }

    /** Write the Unicode code point to the terminal encoded in UTF-8. */
    public void writeCodePoint(boolean prependEscape, int codePoint) {
        if (codePoint > 1114111 || (codePoint >= 0xD800 && codePoint <= 0xDFFF)) {
            // 1114111 (= 2**16 + 1024**2 - 1) is the highest code point, [0xD800,0xDFFF] is the surrogate range.
            throw new IllegalArgumentException("Invalid code point: " + codePoint);
        }

        int bufferPosition = 0;
        if (prependEscape) mUtf8InputBuffer[bufferPosition++] = 27;

        if (codePoint <= /* 7 bits */0b1111111) {
            mUtf8InputBuffer[bufferPosition++] = (byte) codePoint;
        } else if (codePoint <= /* 11 bits */0b11111111111) {
            /* 110xxxxx leading byte with leading 5 bits */
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b11000000 | (codePoint >> 6));
            /* 10xxxxxx continuation byte with following 6 bits */
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b10000000 | (codePoint & 0b111111));
        } else if (codePoint <= /* 16 bits */0b1111111111111111) {
            /* 1110xxxx leading byte with leading 4 bits */
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b11100000 | (codePoint >> 12));
            /* 10xxxxxx continuation byte with following 6 bits */
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b10000000 | ((codePoint >> 6) & 0b111111));
            /* 10xxxxxx continuation byte with following 6 bits */
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b10000000 | (codePoint & 0b111111));
        } else { /* We have checked codePoint <= 1114111 above, so we have max 21 bits = 0b111111111111111111111 */
            /* 11110xxx leading byte with leading 3 bits */
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b11110000 | (codePoint >> 18));
            /* 10xxxxxx continuation byte with following 6 bits */
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b10000000 | ((codePoint >> 12) & 0b111111));
            /* 10xxxxxx continuation byte with following 6 bits */
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b10000000 | ((codePoint >> 6) & 0b111111));
            /* 10xxxxxx continuation byte with following 6 bits */
            mUtf8InputBuffer[bufferPosition++] = (byte) (0b10000000 | (codePoint & 0b111111));
        }
        write(mUtf8InputBuffer, 0, bufferPosition);
    }

    public TerminalEmulator getEmulator() {
        return mEmulator;
    }

    /** Notify the {@link #mClient} that the screen has changed. */
    protected void notifyScreenUpdate() {
        mClient.onTextChanged(this);
    }

    /** Reset state for terminal emulator state. */
    public void reset() {
        mEmulator.reset();
        notifyScreenUpdate();
    }

    /** End this session through its backend. */
    public void finishIfRunning() {
        if (isRunning()) mBackend.close();
    }

    /** Cleanup resources when the backend ends. */
    void cleanupResources(Integer exitStatus) {
        synchronized (this) {
            mRunning = false;
            mExitStatus = exitStatus;
        }

        // Stop the writer thread and wake a backend blocked on output.
        mInputQueue.offer(END_OF_INPUT);
        mProcessToTerminalIOQueue.close();
        mBackend.close();
    }

    @Override
    public void titleChanged(String oldTitle, String newTitle) {
        mClient.onTitleChanged(this);
    }

    public synchronized boolean isRunning() {
        return mRunning;
    }

    /** Only valid if not {@link #isRunning()}; null if the backend didn't know. */
    public synchronized Integer getExitStatus() {
        return mExitStatus;
    }

    @Override
    public void onCopyTextToClipboard(String text) {
        mClient.onCopyTextToClipboard(this, text);
    }

    @Override
    public void onPasteTextFromClipboard() {
        mClient.onPasteTextFromClipboard(this);
    }

    @Override
    public void onBell() {
        mClient.onBell(this);
    }

    @Override
    public void onNotify(String title, String body) {
        mClient.onNotify(this, title, body);
    }

    @Override
    public void onColorsChanged() {
        mClient.onColorsChanged(this);
    }

    @SuppressLint("HandlerLeak")
    class MainThreadHandler extends Handler {

        final byte[] mReceiveBuffer = new byte[64 * 1024];

        MainThreadHandler() {
            super(Looper.getMainLooper());
        }

        @Override
        public void handleMessage(Message msg) {
            int bytesRead = mProcessToTerminalIOQueue.read(mReceiveBuffer, false);
            if (bytesRead > 0) {
                // Guest output is untrusted: an emulator bug must cost a garbled batch, not the
                // app, whose death is a hard power-off of the VM.
                try {
                    mEmulator.append(mReceiveBuffer, bytesRead);
                } catch (RuntimeException e) {
                    Logger.logStackTraceWithMessage(mClient, LOG_TAG, "terminal output the emulator couldn't handle", e);
                }
                // Podroid: DECSET 2026 — if the app is mid-frame (e.g. btop's atomic
                // update), skip the invalidate. Cells are still mutated in the buffer;
                // the close bracket arriving in a later batch will clear the flag and
                // the next handleMessage will paint the completed frame in one shot.
                if (!mEmulator.isSynchronizedOutputActive()) {
                    notifyScreenUpdate();
                }
            }

            if (msg.what == MSG_PROCESS_EXITED) {
                if (!isRunning()) return;
                Exit exit = (Exit) msg.obj;
                // Output delivered before the exit may still be queued.
                while ((bytesRead = mProcessToTerminalIOQueue.read(mReceiveBuffer, false)) > 0) {
                    mEmulator.append(mReceiveBuffer, bytesRead);
                }
                cleanupResources(exit.status);

                String exitDescription = "\r\n[" + describeExit(exit.status, exit.reason) + " - press Enter]";
                byte[] bytesToWrite = exitDescription.getBytes(StandardCharsets.UTF_8);
                mEmulator.append(bytesToWrite, bytesToWrite.length);
                notifyScreenUpdate();

                mClient.onSessionFinished(TerminalSession.this);
            }
        }

    }

    static String describeExit(Integer status, String reason) {
        if (reason != null) return reason;
        String description = "Process completed";
        if (status != null && status > 0) {
            description += " (code " + status + ")";
        } else if (status != null && status < 0) {
            description += " (signal " + (-status) + ")";
        }
        return description;
    }

}
