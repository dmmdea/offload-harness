// Package contextbudget trims oversized input before it reaches the local
// model, keeping requests inside the model's context window.
package contextbudget

import (
	"strings"
	"unicode/utf8"
)

// Trim caps input to maxChars. When it must cut, it keeps the head and tail
// (where summaries/structure usually live) and inserts an elision marker.
// Both cut points back off to a rune boundary (LO-13: byte-offset cuts split
// multibyte runes — Spanish á/ñ at a boundary produced mojibake mid-prompt).
// Returns the possibly-trimmed text and whether trimming occurred.
func Trim(input string, maxChars int) (string, bool) {
	if maxChars <= 0 || len(input) <= maxChars {
		return input, false
	}
	const marker = "\n\n[...content elided to fit local model context...]\n\n"
	keep := maxChars - len(marker)
	if keep < 200 {
		// Degenerate cap; just hard-cut the head (rune-safe).
		return headCut(input, maxChars), true
	}
	head := keep * 2 / 3
	tail := keep - head
	return headCut(input, head) + marker + tailCut(input, tail), true
}

// headCut returns at most n leading bytes of s, backing off so a multibyte
// rune is never split (the same loop preview() uses in the pipeline).
func headCut(s string, n int) string {
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// tailCut returns at most n trailing bytes of s, advancing the start past any
// leading continuation bytes so the suffix begins on a rune boundary.
func tailCut(s string, n int) string {
	cut := s[len(s)-n:]
	for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
		cut = cut[1:]
	}
	return cut
}

// IsTrivial reports whether input is too small/empty to bother offloading.
func IsTrivial(input string) bool {
	return len(strings.TrimSpace(input)) < 8
}
