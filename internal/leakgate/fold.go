package leakgate

import (
	"bytes"
	"unicode/utf8"
)

// The fold makes encoded spellings of a name visible to the tokenizer.
//
// Pass A (Fold) maps a text to pure ASCII:
//
//   - \uXXXX JSON escapes are decoded (a surrogate pair becomes one separator);
//     a decoded code point below U+0020, or U+007F, becomes a space, never
//     itself, so the fold cannot add a newline;
//   - zero-width characters (U+200B, U+200C, U+200D, U+2060, U+FEFF, U+00AD)
//     disappear, so a name split by one rejoins;
//   - combining marks U+0300 to U+036F disappear;
//   - Latin letters of U+00C0 to U+024F whose canonical decomposition is one
//     ASCII letter plus marks fold to that letter (the 244-entry foldLatin);
//   - full-width ASCII (U+FF01 to U+FF5E) and the ligatures U+FB00 to U+FB06
//     fold to ASCII;
//   - every other non-ASCII rune, and every invalid byte, becomes a separator.
//
// The fold never adds or removes a newline, so line numbers stay exact; columns
// after a fold refer to the folded stream.
//
// Pass B (BlankEscapes) blanks literal escape introducers in place, with spaces
// of the same length: the escape letter would otherwise glue onto the next word
// (\nname is the run nname) and %20 would glue the two halves of a phrase.

// NeedsFold reports whether b holds a byte at or above 0x80 or the two bytes
// \u, the only inputs the fold changes.
func NeedsFold(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return true
		}
	}
	return bytes.Contains(b, []byte(`\u`))
}

var ligatures = [7]string{"ff", "fi", "fl", "ffi", "ffl", "st", "st"}

func isZeroWidth(r rune) bool {
	switch r {
	case 0x200B, 0x200C, 0x200D, 0x2060, 0xFEFF, 0x00AD:
		return true
	}
	return false
}

// foldRune appends the fold of one code point (never a control character below
// U+0020, which the caller maps to a space).
func foldRune(out []byte, r rune) []byte {
	switch {
	case r < 0x80:
		return append(out, byte(r))
	case isZeroWidth(r):
		return out
	case r >= 0x300 && r <= 0x36F:
		return out
	case r >= 0xFF01 && r <= 0xFF5E:
		return append(out, byte(r-0xFEE0))
	case r >= 0xFB00 && r <= 0xFB06:
		return append(out, ligatures[r-0xFB00]...)
	case r >= 0xC0 && r < rune(len(foldLatin)):
		if b := foldLatin[r]; b != 0 {
			return append(out, b)
		}
	}
	return append(out, ' ')
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// hex4 reads four hex digits.
func hex4(b []byte) (rune, bool) {
	if len(b) < 4 {
		return 0, false
	}
	var v rune
	for _, c := range b[:4] {
		h := hexVal(c)
		if h < 0 {
			return 0, false
		}
		v = v<<4 | rune(h)
	}
	return v, true
}

// Fold is pass A. Callers check NeedsFold first; on text with nothing to fold
// it returns an equal copy.
func Fold(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		if c == '\\' && i+5 < len(b) && b[i+1] == 'u' {
			if v, ok := hex4(b[i+2 : i+6]); ok {
				n := 6
				switch {
				case v >= 0xD800 && v <= 0xDBFF:
					out = append(out, ' ')
					if i+11 < len(b) && b[i+6] == '\\' && b[i+7] == 'u' {
						if lo, ok := hex4(b[i+8 : i+12]); ok && lo >= 0xDC00 && lo <= 0xDFFF {
							n = 12
						}
					}
				case v >= 0xDC00 && v <= 0xDFFF:
					out = append(out, ' ')
				case v < 0x20 || v == 0x7F:
					out = append(out, ' ')
				default:
					out = foldRune(out, v)
				}
				i += n
				continue
			}
		}
		if c < 0x80 {
			out = append(out, c)
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError {
			out = append(out, ' ')
		} else {
			out = foldRune(out, r)
		}
		i += size
	}
	return out
}

func isEscLetter(c byte) bool {
	switch c {
	case 'n', 't', 'r', 'b', 'f', 'v', 'a', 'e', '0':
		return true
	}
	return false
}

// escapeLen is the length of the escape introducer at the start of b, or 0:
// a backslash or backtick before one of n t r b f v a e 0 (two bytes), a
// backslash x with two hex digits (four), a percent with two hex digits
// (three). The alternatives are tried in that order, left to right.
func escapeLen(b []byte) int {
	switch b[0] {
	case '\\', '`':
		if len(b) >= 2 && isEscLetter(b[1]) {
			return 2
		}
		if b[0] == '\\' && len(b) >= 4 && b[1] == 'x' && hexVal(b[2]) >= 0 && hexVal(b[3]) >= 0 {
			return 4
		}
	case '%':
		if len(b) >= 3 && hexVal(b[1]) >= 0 && hexVal(b[2]) >= 0 {
			return 3
		}
	}
	return 0
}

// BlankEscapes is pass B. It returns b itself and false when there is no
// escape to blank, and otherwise a copy with every escape replaced in place by
// spaces of the same length (so every column equals pass A's).
func BlankEscapes(b []byte) ([]byte, bool) {
	var out []byte
	for i := 0; i < len(b); {
		c := b[i]
		if c != '\\' && c != '`' && c != '%' {
			i++
			continue
		}
		n := escapeLen(b[i:])
		if n == 0 {
			i++
			continue
		}
		if out == nil {
			out = append([]byte(nil), b...)
		}
		for j := 0; j < n; j++ {
			out[i+j] = ' '
		}
		i += n
	}
	if out == nil {
		return b, false
	}
	return out, true
}
