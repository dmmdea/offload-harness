package agent

import (
	"strings"
	"testing"
)

// TestDegenerateRunCatchesTheNaNShape pins the floor from both sides. Twenty
// repeated non-whitespace bytes is the NaN shape the 2026-09-16/17 vLLM seat
// produced (`<tool_call>!!!!!!!!!!!!!!!!!!!!…`); nineteen is still punctuation
// somebody could legitimately type, and must not defer a contract.
func TestDegenerateRunCatchesTheNaNShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		wantB   byte
		wantRun int
	}{
		{"twenty bangs is a run", strings.Repeat("!", 20), '!', 20},
		{"nineteen bangs is not", strings.Repeat("!", 19), 0, 0},
		{"the live shape, marker and all", "<tool_call>" + strings.Repeat("!", 40), '!', 40},
		{"a long run anywhere in the text", "here is the answer: " + strings.Repeat("0", 64) + " done", '0', 64},
		{"ordinary prose has none", "The answer is 42. It is in notes.md, on the first line — read it and say DONE.", 0, 0},
		{"a markdown rule is not a run", "intro\n\n" + strings.Repeat("-", 12) + "\n\nbody", 0, 0},
		{"empty content has none", "", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, n := DegenerateRun(tc.in)
			if b != tc.wantB || n != tc.wantRun {
				t.Fatalf("DegenerateRun = (%q, %d), want (%q, %d)", string(b), n, string(tc.wantB), tc.wantRun)
			}
		})
	}
}

// TestDegenerateRunIgnoresWhitespaceRuns: indentation, blank-line padding and
// the space runs in an ASCII table are ordinary output. Counting them would
// defer healthy contracts, which is strictly worse than missing a broken seat
// (the loop still catches that one, late).
func TestDegenerateRunIgnoresWhitespaceRuns(t *testing.T) {
	for _, in := range []string{
		strings.Repeat(" ", 200),
		strings.Repeat("\n", 64),
		strings.Repeat("\t", 40),
		"col" + strings.Repeat(" ", 80) + "value",
	} {
		if b, n := DegenerateRun(in); n != 0 {
			t.Fatalf("whitespace run flagged as degenerate: (%q, %d)", string(b), n)
		}
	}
}

// TestDegenerateRunResetsAcrossWhitespace: two 15-byte runs split by a newline
// are two runs, not one of 30 — the scan must not carry a run across the gap it
// just refused to count.
func TestDegenerateRunResetsAcrossWhitespace(t *testing.T) {
	in := strings.Repeat("!", 15) + "\n" + strings.Repeat("!", 15)
	if b, n := DegenerateRun(in); n != 0 {
		t.Fatalf("DegenerateRun = (%q, %d), want none — a run does not span whitespace", string(b), n)
	}
}
