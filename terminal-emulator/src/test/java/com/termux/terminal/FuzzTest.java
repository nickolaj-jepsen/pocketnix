package com.termux.terminal;

import java.nio.charset.StandardCharsets;
import java.util.Random;

/**
 * Guest output is untrusted, and an exception in the emulator kills the app
 * (and with it the VM): random bytes and escape sequences must never throw.
 */
public class FuzzTest extends TerminalTestCase {

	private static final String[] INTRODUCERS = {"\033]", "\033[", "\033P", "\033_", "\033", "\033[?", "\033[>", "\033]8;;", "\033]52;c;"};
	private static final String PIECES = "0123456789;:?$\"'!#%&()*+,-./<=>@ABCDEFGHJKLMPSTXZ[\\]^_`abcdfghlmnqrstuxy{|}~\007\033 ";

	public void testRandomBytes() {
		Random r = new Random(1);
		for (int round = 0; round < 300; round++) {
			withTerminalSized(1 + r.nextInt(40), 1 + r.nextInt(20));
			byte[] b = new byte[1 + r.nextInt(4096)];
			r.nextBytes(b);
			mTerminal.append(b, b.length);
		}
	}

	public void testRandomEscapeSequences() {
		Random r = new Random(2);
		for (int round = 0; round < 3000; round++) {
			withTerminalSized(1 + r.nextInt(80), 1 + r.nextInt(30));
			StringBuilder s = new StringBuilder();
			for (int seq = 0; seq < 8; seq++) {
				s.append(INTRODUCERS[r.nextInt(INTRODUCERS.length)]);
				// Numbers out of every range the parser expects.
				if (r.nextBoolean()) s.append(r.nextInt(100000) - (r.nextInt(10) == 0 ? 50000 : 0));
				int n = r.nextInt(12);
				for (int i = 0; i < n; i++) s.append(PIECES.charAt(r.nextInt(PIECES.length())));
				if (r.nextBoolean()) s.append(r.nextBoolean() ? "\007" : "\033\\");
			}
			byte[] b = s.toString().getBytes(StandardCharsets.UTF_8);
			mTerminal.append(b, b.length);
		}
	}

	/** The crashes found by review: OSC 104 with an index past the palette, or not a number. */
	public void testOsc104WithBadIndices() {
		withTerminalSized(10, 10);
		for (String osc : new String[]{"\033]104;300\007", "\033]104;x\007", "\033]104;-1\007", "\033]104;1;;x;2000\007", "\033]4;999;#ffffff\007"}) {
			byte[] b = osc.getBytes(StandardCharsets.UTF_8);
			mTerminal.append(b, b.length);
		}
	}
}
