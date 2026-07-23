package pipeline

import "github.com/dmmdea/offload-harness/internal/gcf"

// compactForBudget losslessly shrinks an over-budget input before the lossy
// context-budget trim gets to cut it. Only when the flag is on AND the input
// actually exceeds maxChars (an in-budget input is never touched — no bytes
// change on the happy path), eligible JSON arrays in the input are re-encoded
// columnar via internal/gcf (round-trip proven). Whatever still overflows is
// trimmed by the caller as before — this pass only converts would-be-truncated
// content into content that fits at full fidelity.
func compactForBudget(input string, maxChars int, enabled bool) string {
	if !enabled || maxChars <= 0 || len(input) <= maxChars || gcf.IsCompacted(input) {
		return input
	}
	if c, ok := gcf.Compact(input); ok {
		return c
	}
	return input
}
