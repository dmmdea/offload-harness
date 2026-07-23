package agent

import (
	"fmt"
	"strings"
	"testing"
)

// buildToolOutput fabricates a verbose tool-result body: n numbered filler
// lines with the given signal lines spliced in at fixed positions (deep in the
// middle, past the head window).
func buildToolOutput(n int, signals ...string) string {
	lines := make([]string, 0, n+len(signals))
	for i := 0; i < n; i++ {
		lines = append(lines, fmt.Sprintf("line %03d: unremarkable filler content for a verbose tool result", i))
		if i == n/2 {
			lines = append(lines, signals...)
		}
	}
	return strings.Join(lines, "\n")
}

// TestSkeletonizeKeepsHeadTailAndSignal: the skeleton must keep the head
// window, the tail window, and mid-body signal lines (errors/failures), elide
// the rest into counted run markers, and disclose itself with the skeleton
// prefix — at a fraction of the original size.
func TestSkeletonizeKeepsHeadTailAndSignal(t *testing.T) {
	orig := buildToolOutput(200,
		"ERROR: connection refused by upstream",
		"--- FAIL: TestSomething (0.03s)",
	)
	sk, ok := skeletonize(orig)
	if !ok {
		t.Fatalf("skeletonize declined a %d-char verbose body", len(orig))
	}
	if !isSkeletonized(sk) {
		t.Errorf("skeleton does not carry the disclosure prefix: %q", firstLine(sk))
	}
	if !strings.Contains(sk, "line 000:") {
		t.Errorf("head window lost")
	}
	if !strings.Contains(sk, "line 199:") {
		t.Errorf("tail window lost")
	}
	if !strings.Contains(sk, "ERROR: connection refused by upstream") {
		t.Errorf("mid-body error signal line lost")
	}
	if !strings.Contains(sk, "--- FAIL: TestSomething") {
		t.Errorf("mid-body test-failure signal line lost")
	}
	if !strings.Contains(sk, "lines elided") {
		t.Errorf("no elision run markers present")
	}
	if len(sk) > len(orig)/3 {
		t.Errorf("skeleton too large: %d of %d chars — pruning gained too little", len(sk), len(orig))
	}
}

// TestSkeletonizeElisionCountsAccount: kept lines plus the counts in the
// elision markers must account for every original line — the markers are an
// honest ledger, not decoration.
func TestSkeletonizeElisionCountsAccount(t *testing.T) {
	orig := buildToolOutput(150, "panic: runtime error: index out of range")
	origLines := len(strings.Split(orig, "\n"))
	sk, ok := skeletonize(orig)
	if !ok {
		t.Fatal("skeletonize declined")
	}
	kept, elided := 0, 0
	for _, l := range strings.Split(sk, "\n")[1:] { // [0] is the disclosure prefix
		var n int
		if c, err := fmt.Sscanf(l, "[... %d lines elided ...]", &n); err == nil && c == 1 {
			elided += n
		} else {
			kept++
		}
	}
	if kept+elided != origLines {
		t.Errorf("line ledger broken: kept %d + elided %d != original %d", kept, elided, origLines)
	}
}

// TestSkeletonizeDeclinesSmallOrStructuredInput: bodies too small to pay for a
// skeleton, and bodies already skeletonized or marker-elided, are declined
// unchanged — idempotence and no-gain protection.
func TestSkeletonizeDeclinesSmallOrStructuredInput(t *testing.T) {
	if _, ok := skeletonize("short output\nok\n"); ok {
		t.Errorf("skeletonize accepted a trivially small body")
	}
	orig := buildToolOutput(200, "ERROR: x")
	sk, ok := skeletonize(orig)
	if !ok {
		t.Fatal("setup: skeletonize declined the verbose body")
	}
	if again, ok2 := skeletonize(sk); ok2 || again != sk {
		t.Errorf("skeletonize is not idempotent: re-accepted its own output")
	}
	marker := elisionMarker(4000)
	if _, ok3 := skeletonize(marker); ok3 {
		t.Errorf("skeletonize accepted an already-elided marker body")
	}
}

// TestSkeletonizeDeterministic: same input, same output, every time — a
// re-compaction must not thrash the transcript (or the KV cache) by producing
// a different skeleton.
func TestSkeletonizeDeterministic(t *testing.T) {
	orig := buildToolOutput(300, "WARN: deprecated flag", "ERROR: nope")
	a, okA := skeletonize(orig)
	b, okB := skeletonize(orig)
	if !okA || !okB || a != b {
		t.Errorf("skeletonize not deterministic (okA=%v okB=%v, equal=%v)", okA, okB, a == b)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
