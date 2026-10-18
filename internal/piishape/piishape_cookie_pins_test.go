package piishape

import (
	"strings"
	"testing"
)

// The guards here were found by deleting them one at a time (a table of single-line mutants of the cookie class): the
// suite stayed green without them. Each test is green against the class as it stands and red against the mutant that
// removes the guard it names.

// TestCookieQuotedValueThatStartsWithAUnicodeEscapeIsAPair: Python's json.dumps writes a backslash-u escape for every
// non-ASCII letter (Cyrillic, Latin Extended, Devanagari: U+0100 to U+0FFF, whose hex digits start with 0 and a second
// digit that is not 0), so a short quoted cookie value such as a language name starts with one. The class has to read
// that quote as the opener of a value and not as the quote that closes the string the header sits in, or the span ends
// there and the long cookie behind it is written out.
func TestCookieQuotedValueThatStartsWithAUnicodeEscapeIsAPair(t *testing.T) {
	v := valueFixture(861, 36, alnum)
	bs := "\x5c"
	for _, hex := range []string{"0416", "0107", "0905", "03a9", "0fff"} {
		esc := bs + "u" + hex
		text := `{"cmd": "` + ckName + `: lang=` + bs + `"` + esc + esc + bs + `"; sessionid=` + v + `"}`
		if out := cookieClass().Re.ReplaceAllString(text, "[R]"); strings.Contains(out, v[:12]) {
			t.Errorf("%s: the long cookie value survives: %q", hex, out)
		}
	}
}
