// Package mutfixture is the tiny package the mutation harness proves itself on
// (scripts/mutation-check-selftest.sh and its PowerShell twin). It is a module of
// its own, so no build or test of the repository ever sees it; the self-test copies
// it into a temp dir and mutates the copy, never this file.
package mutfixture

// Clamp limits v to the closed range [lo, hi]. It assumes lo <= hi.
func Clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Band names the size class of n: "negative", "zero", "small" (1 to 9) or "large".
func Band(n int) string {
	switch {
	case n < 0:
		return "negative"
	case n == 0:
		return "zero"
	case n < 10:
		return "small"
	}
	return "large"
}
