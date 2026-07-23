package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// Skeleton pruning — the middle rung of the compaction ladder. The existing
// ladder jumps straight from "full tool body" to "bare size marker", so the
// first time a long run crosses its budget, every older tool result loses ALL
// its information at once. This rung sits between the two: an older verbose
// body is reduced to a SKELETON — the head and tail windows plus any signal
// lines (errors, failures, warnings, test summaries) buried in the middle,
// with elided runs replaced by counted markers — so the model keeps what it
// most often needs from an old result (what went wrong, how it ended) at a
// fraction of the tokens. Bare markers and whole-turn drops remain the
// fall-through when skeletons alone cannot reach the budget.
//
// Deliberately DETERMINISTIC and model-free: pruning runs on the loop's
// critical path, and the local cascade costs ~4s warm / ~11s cold per call
// (measured 2026-07-23 on the 16GB box) — a per-body model call would
// serialize every over-budget step. A rules-based skeleton costs microseconds,
// produces identical output on every re-compaction (KV-prefix friendly and
// idempotent via the disclosure prefix), and is fully unit-testable. A
// model-refined skeleton (or a lossless structural compactor for homogeneous
// JSON) can slot into this same seam later if measurement earns it.

// skeletonHeadLines/skeletonTailLines are the windows always kept: openings
// carry the command/result framing, endings carry the summary/exit state.
const skeletonHeadLines = 8
const skeletonTailLines = 4

// skeletonMaxSignalLines caps mid-body signal keeps so a pathological body
// (every line matches) cannot produce a skeleton as large as the original.
const skeletonMaxSignalLines = 24

// skeletonMinChars: below this a body is not worth skeletonizing — the bare
// marker (step 3) or simply leaving it are both fine, and the disclosure
// prefix would be a meaningful fraction of the "savings".
const skeletonMinChars = 600

// signalLine marks lines worth keeping from the middle of a verbose tool
// body: error/failure/warning vocabulary plus common test-summary shapes.
// Case-insensitive and deliberately recall-leaning — a stray prose match makes
// a slightly fatter (still far smaller) skeleton, and skeletonMaxSignalLines
// bounds the damage either way.
var signalLine = regexp.MustCompile(`(?i)\b(error|fail(ed|ure)?|panic|fatal|warn(ing)?|exception|denied|refused|missing|not found|cannot|unable|invalid|timeout)\b|^ok\s|^--- |\b\d+ pass(ed)?\b|\b\d+ fail(ed)?\b`)

// skeletonPrefix opens every skeleton: disclosure to the model (content was
// pruned, and how much) and the idempotence marker for isSkeletonized.
const skeletonPrefix = "[skeleton — pruned from "

// skeletonize reduces a verbose tool-result body to its skeleton. Returns
// (skeleton, true) when pruning is worthwhile, or (content, false) unchanged
// when the body is too small, already a skeleton or a bare marker, or the
// skeleton would not actually be smaller.
func skeletonize(content string) (string, bool) {
	if len(content) < skeletonMinChars || isSkeletonized(content) || isElided(content) {
		return content, false
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= skeletonHeadLines+skeletonTailLines+2 {
		return content, false // nearly everything is head/tail — nothing to elide.
	}

	keep := make([]bool, len(lines))
	for i := 0; i < skeletonHeadLines; i++ {
		keep[i] = true
	}
	for i := len(lines) - skeletonTailLines; i < len(lines); i++ {
		keep[i] = true
	}
	signal := 0
	for i := skeletonHeadLines; i < len(lines)-skeletonTailLines && signal < skeletonMaxSignalLines; i++ {
		if signalLine.MatchString(lines[i]) {
			keep[i] = true
			signal++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s%d chars]\n", skeletonPrefix, len(content))
	run := 0
	flush := func() {
		if run > 0 {
			fmt.Fprintf(&b, "[... %d lines elided ...]\n", run)
			run = 0
		}
	}
	for i, l := range lines {
		if keep[i] {
			flush()
			b.WriteString(l)
			b.WriteByte('\n')
		} else {
			run++
		}
	}
	flush()

	out := strings.TrimSuffix(b.String(), "\n")
	if len(out) >= len(content) {
		return content, false // no gain — leave it for the marker rung.
	}
	return out, true
}

// skeletonOriginalSize parses the original body size out of a skeleton's
// disclosure prefix, so the bare-marker rung can report the TRUE pre-skeleton
// size when it further elides a skeleton (a marker naming the skeleton's own
// length would understate what was dropped ~10x).
func skeletonOriginalSize(content string) (int, bool) {
	if !isSkeletonized(content) {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(content[len(skeletonPrefix):], "%d chars]", &n); err != nil {
		return 0, false
	}
	return n, true
}

// isSkeletonized reports whether a body is already a skeleton, so
// re-compaction never re-prunes (the line ledger would be double-counted) and
// step 3 can still replace a skeleton with a bare marker under harder
// pressure. Pure-ASCII prefix — rune-safe by construction, like isElided.
func isSkeletonized(content string) bool {
	return len(content) >= len(skeletonPrefix) && content[:len(skeletonPrefix)] == skeletonPrefix
}
