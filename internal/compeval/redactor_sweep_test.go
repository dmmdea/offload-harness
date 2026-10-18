package compeval

import (
	"strings"
	"testing"
	"time"
)

// A cookie span can swallow the key word of a credential that follows it on the line (it ends in front of a quote that
// opens no cookie value), so when the cookie class has substituted anything, the credential classes are also run over the
// text as it was written and the values they find are substituted wherever they still stand (sweepCredentials). The
// sweep holds three promises that the rest of the suite did not pin, and a cost that the third review found to be the
// square of the number of values.

// sweepKw names a credential key word in two pieces, so no literal in the tree is credential-shaped.
const sweepKw = spanKwToken

// TestSweepReplacesTheLongerValueWhole: when one swept value is the head of another (a 14-character value and the same 14
// characters followed by 22 more), the longer one goes first. Replaced the other way round, the head of the longer value
// becomes a placeholder, the longer value is no longer found, and its last 22 characters stay in the text with the gate
// clean.
func TestSweepReplacesTheLongerValueWhole(t *testing.T) {
	w1, w2 := tailFake(7301, 32, tailAlnum), tailFake(7302, 32, tailAlnum)
	a, s := tailFake(7303, 14, tailAlnum), tailFake(7304, 22, tailAlnum)
	for name, text := range map[string]string{
		"short first": spanCk + ": sid=" + w1 + " " + sweepKw + " = '" + a + "'\n" + spanCk + ": sid=" + w2 + " " + spanKwPass + " = '" + a + s + "'",
		"long first":  spanCk + ": sid=" + w2 + " " + spanKwPass + " = '" + a + s + "'\n" + spanCk + ": sid=" + w1 + " " + sweepKw + " = '" + a + "'",
	} {
		r := NewRedactor()
		out := r.Redact(text)
		if run, ok := anyRun(s, out, 8); ok {
			t.Errorf("%s: %q of the longer value is still in %q", name, run, out)
		}
		if run, ok := anyRun(a, out, 8); ok {
			t.Errorf("%s: %q of the shorter value is still in %q", name, run, out)
		}
		if res := r.Residual(out); len(res) != 0 {
			t.Errorf("%s: residual %v on %q", name, res, out)
		}
	}
}

// TestSweepRemembersWhatItReplaces: a value only the sweep replaced is remembered like any other, so a later turn that prints
// the bare value is caught by the value-tail gate of the same redactor.
func TestSweepRemembersWhatItReplaces(t *testing.T) {
	w, v := tailFake(7311, 32, tailAlnum), tailFake(7312, 26, tailAlnum)
	r := NewRedactor()
	first := r.Redact(spanCk + ": sid=" + w + " " + sweepKw + " = '" + v + "'")
	if _, ok := anyRun(v, first, 8); ok {
		t.Fatalf("the sweep left the value in %q", first)
	}
	second := "the earlier value was " + v + " in the log"
	if res := r.Residual(second); len(res) != 1 || res[0] != "value-tail" {
		t.Errorf("a later turn that prints the swept value: residual %v, want [value-tail]", res)
	}
}

// TestSweepCountsWhatItReplaces: the hits of the class are the places the sweep substituted.
func TestSweepCountsWhatItReplaces(t *testing.T) {
	w, v := tailFake(7321, 32, tailAlnum), tailFake(7322, 26, tailAlnum)
	r := NewRedactor()
	r.Redact(spanCk + ": sid=" + w + " " + sweepKw + " = '" + v + "'\n" + spanCk + ": sid=" + w + " " + sweepKw + " = '" + v + "'")
	if n := r.Hits()["api-key-assignment"]; n != 2 {
		t.Errorf("api-key-assignment hits = %d, want 2 (the value stood in two places)", n)
	}
}

// TestRedactorCountsACapitalEscapeAfterTheFirstCharacters: a capital N, R or U after a backslash is no escape of a line break or
// a character code (a Windows path, \Name), and the count that starts a match must take it as one character of the value.
// The class is case-insensitive, so the character class that excludes the lowercase escapes would exclude the capitals too
// unless it stays case-sensitive inside.
func TestRedactorCountsACapitalEscapeAfterTheFirstCharacters(t *testing.T) {
	v := tailFake(7331, 24, tailAlnum)
	for _, esc := range []string{`\N`, `\R`, `\U`} {
		text := spanCk + ": sid=ab" + esc + "xyzQ" + v
		reviewRedact(t, esc+" in a line", text, v)
		reviewRedact(t, esc+" in Args", reviewJSON(t, map[string]string{"cmd": text}), v)
	}
}

// sweepShape builds about n bytes of a text of one Cookie line and many lines that carry a distinct credential.
func sweepShape(kind string, n int) string {
	var b strings.Builder
	b.WriteString(spanCk + ": sid=" + tailFake(1, 24, tailAlnum) + "\n")
	for i := 2; b.Len() < n; i++ {
		switch kind {
		case "assignment":
			b.WriteString(sweepKw + "=" + tailFake(i, 12, tailAlnum) + "\n")
		case "bearer":
			b.WriteString("Bearer " + tailFake(i, 16, tailAlnum) + "\n")
		case "cookie and key word":
			b.WriteString(spanCk + ": a=bbbbbbbbbbbb; " + sweepKw + ": \"" + tailFake(i, 20, tailAlnum) + "\"\n")
		}
	}
	return b.String()
}

// TestRedactorSweepCostGrowsInStepWithTheText: one Cookie line in a text of many lines that carry a credential of their own
// made the sweep search the whole text once per value, so doubling the text took 4 times as long: 20 s for 1 MiB. The
// sweep is a single pass now. A full redaction of the shape the review used (one Cookie line and many assignment lines)
// stays under 2 s per MiB at 1 and 2 MiB, and doubling any of three shapes takes less than 3 times as long (linear is 2,
// the square is 4). It runs for several seconds, so it is left out of short runs; it is a timing test and belongs on a
// quiet runner.
func TestRedactorSweepCostGrowsInStepWithTheText(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	const mib = 1 << 20
	for _, kind := range []string{"assignment", "bearer", "cookie and key word"} {
		var secs [2]float64
		for i, size := range []int{mib, 2 * mib} {
			text := sweepShape(kind, size)
			r := NewRedactor()
			t0 := time.Now()
			out := r.Redact(text)
			secs[i] = time.Since(t0).Seconds()
			perMiB := secs[i] / (float64(len(text)) / mib)
			t.Logf("%s, %d bytes: %.2f s, %.2f s per MiB (%d bytes out)", kind, len(text), secs[i], perMiB, len(out))
			if kind == "assignment" && perMiB > 2.0 {
				t.Errorf("%s, %d bytes: %.2f s per MiB, want under 2", kind, len(text), perMiB)
			}
		}
		if ratio := secs[1] / secs[0]; ratio > 3.0 {
			t.Errorf("%s: 2 MiB took %.2f s and 1 MiB %.2f s, a factor of %.1f for twice the text (linear is 2, the square 4)", kind, secs[1], secs[0], ratio)
		}
	}
}
