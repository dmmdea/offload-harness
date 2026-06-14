// Package contextbudget trims oversized input before it reaches the local
// model, keeping requests inside the model's context window.
package contextbudget

import "strings"

// Trim caps input to maxChars. When it must cut, it keeps the head and tail
// (where summaries/structure usually live) and inserts an elision marker.
// Returns the possibly-trimmed text and whether trimming occurred.
func Trim(input string, maxChars int) (string, bool) {
	if maxChars <= 0 || len(input) <= maxChars {
		return input, false
	}
	const marker = "\n\n[...content elided to fit local model context...]\n\n"
	keep := maxChars - len(marker)
	if keep < 200 {
		// Degenerate cap; just hard-cut the head.
		return input[:maxChars], true
	}
	head := keep * 2 / 3
	tail := keep - head
	return input[:head] + marker + input[len(input)-tail:], true
}

// IsTrivial reports whether input is too small/empty to bother offloading.
func IsTrivial(input string) bool {
	return len(strings.TrimSpace(input)) < 8
}
