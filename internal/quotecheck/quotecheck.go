// Package quotecheck finds a quote in the text it claims to come from. It is the
// lookup the research digest's quote check (internal/research/quotes.go) has always
// made, in a package that imports nothing of the harness, so internal/agent can ask
// the same question of a tool's output without importing internal/research (which
// imports internal/delegate, whose tests import the agent).
//
//   - Locate tolerantly, publish exactly. A quote is found with case, spacing,
//     typographic quote marks and dashes set aside and, failing that, with
//     punctuation set aside too (the seat's added colon, a table's cell borders),
//     and when it is found the span returned is the TEXT's, so it is the text's
//     bytes by construction. The seat points, the caller copies.
//   - A quote found nowhere is reported as Unverified. What to do about that (flag
//     it, drop a receipt) is the caller's decision.
package quotecheck

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// How says how a quote was found.
type How int

const (
	// Unverified: the quote is nowhere in the text.
	Unverified How = iota
	// Verbatim: the text's bytes already (after trimming the quote's edge spaces).
	Verbatim
	// Corrected: found only after folding; the span returned is the text's own.
	Corrected
)

// elision splits a quote at an ellipsis ("first words ... last words"), the
// bracketed form included.
var elision = regexp.MustCompile(`\s*(?:\[\s*(?:\.\.\.|\x{2026})\s*\]|\.\.\.|\x{2026})\s*`)

// Folds holds the folded forms of one page, each built on first use.
type Folds struct {
	page          string
	strict, loose *foldedText
}

// NewFolds returns the fold cache for page. Make one per page and pass it to every
// Locate on that page, so the page is folded once.
func NewFolds(page string) *Folds { return &Folds{page: page} }

func (p *Folds) get(loose bool) foldedText {
	slot := &p.strict
	if loose {
		slot = &p.loose
	}
	if *slot == nil {
		f := foldWith(p.page, loose)
		*slot = &f
	}
	return **slot
}

// Locate returns the text to publish for q and how it was found. The page's
// own bytes come first (what a reader's find-in-page does); then the tolerant
// forms, strict before loose: strict sets case, spacing, quote marks and dashes
// aside and keeps the rest of the punctuation, loose compares the words alone
// (a colon a seat added after a label, a table's cell borders).
func Locate(page string, folds *Folds, q string) (string, How) {
	raw := strings.TrimSpace(q)
	if strings.Contains(page, raw) {
		return raw, Verbatim
	}
	parts := splitElision(raw)
	if len(parts) > 1 && inOrder(page, parts) {
		return raw, Verbatim
	}
	for _, loose := range []bool{false, true} {
		f := folds.get(loose)
		if s, ok := findFolded(page, f, []string{raw}, loose); ok {
			return s, Corrected
		}
		if len(parts) > 1 {
			if s, ok := findFolded(page, f, parts, loose); ok {
				return s, Corrected
			}
		}
	}
	return q, Unverified
}

// findFolded looks the parts of a quote up in a folded page, each after the one
// before, and returns the page's own span (the parts' spans joined by " ... " when
// there are several).
func findFolded(page string, f foldedText, parts []string, loose bool) (string, bool) {
	var spans []string
	pos := 0
	for _, part := range parts {
		lo, hi, ok := f.find(strings.TrimSpace(foldWith(part, loose).s), pos)
		if !ok {
			return "", false
		}
		spans = append(spans, f.span(page, lo, hi))
		pos = hi
	}
	return strings.Join(spans, " ... "), true
}

// splitElision returns the non-empty parts of s around its ellipses.
func splitElision(s string) []string {
	var parts []string
	for _, p := range elision.Split(s, -1) {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

// inOrder reports whether every part occurs in page, each after the one before.
func inOrder(page string, parts []string) bool {
	pos := 0
	for _, p := range parts {
		i := strings.Index(page[pos:], p)
		if i < 0 {
			return false
		}
		pos += i + len(p)
	}
	return true
}

// foldedText is a text folded for tolerant LOCATING with the map back to the
// original's bytes: case is folded, typographic quote marks and dashes become
// their ASCII forms, runs of white space (a no-break space and a line break
// included) become one space, and zero-width characters vanish. The loose form
// also turns every other character that is not a letter or a digit into
// space, so only the words are compared. start[i] and end[i] are the original byte
// offsets of the character that produced folded byte i.
type foldedText struct {
	s     string
	start []int
	end   []int
}

func foldWith(text string, loose bool) foldedText {
	var b strings.Builder
	var start, end []int
	lastSpace := false
	for i := 0; i < len(text); {
		r, w := utf8.DecodeRuneInString(text[i:])
		j := i + w
		switch {
		case isInvisible(r):
			// No unit: it neither starts nor ends a span, and does not break a run of spaces.
		case unicode.IsSpace(r) || (loose && !isWordRune(foldRune(r))):
			if lastSpace {
				end[len(end)-1] = j
			} else {
				b.WriteByte(' ')
				start, end = append(start, i), append(end, j)
				lastSpace = true
			}
		default:
			var buf [utf8.UTFMax]byte
			n := utf8.EncodeRune(buf[:], foldRune(r))
			b.Write(buf[:n])
			for k := 0; k < n; k++ {
				start, end = append(start, i), append(end, j)
			}
			lastSpace = false
		}
		i = j
	}
	return foldedText{s: b.String(), start: start, end: end}
}

func isInvisible(r rune) bool {
	switch r {
	case '\u00AD', '\u200B', '\u200C', '\u200D', '\u2060', '\uFEFF':
		return true
	}
	return false
}

func foldRune(r rune) rune {
	switch r {
	case '\u2018', '\u2019', '\u201A', '\u201B', '\u2032', '\u02BC', '`', '\u00B4':
		return '\''
	case '\u201C', '\u201D', '\u201E', '\u201F', '\u00AB', '\u00BB', '\u2033':
		return '"'
	case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
		return '-'
	}
	return unicode.ToLower(r)
}

// find returns the folded byte range of the first occurrence of needle at or
// after from that starts and ends on a word boundary: a quote of "no" is not
// found inside "another".
func (f foldedText) find(needle string, from int) (lo, hi int, ok bool) {
	if needle == "" {
		return 0, 0, false
	}
	for from <= len(f.s) {
		i := strings.Index(f.s[from:], needle)
		if i < 0 {
			return 0, 0, false
		}
		lo, hi = from+i, from+i+len(needle)
		if f.bounded(lo, hi) {
			return lo, hi, true
		}
		_, w := utf8.DecodeRuneInString(f.s[lo:])
		from = lo + w
	}
	return 0, 0, false
}

// bounded reports whether the folded range [lo, hi) does not cut a word at either end.
func (f foldedText) bounded(lo, hi int) bool {
	first, _ := utf8.DecodeRuneInString(f.s[lo:])
	last, _ := utf8.DecodeLastRuneInString(f.s[:hi])
	if isWordRune(first) && lo > 0 {
		if prev, _ := utf8.DecodeLastRuneInString(f.s[:lo]); isWordRune(prev) {
			return false
		}
	}
	if isWordRune(last) && hi < len(f.s) {
		if next, _ := utf8.DecodeRuneInString(f.s[hi:]); isWordRune(next) {
			return false
		}
	}
	return true
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// span is the original text under the folded range [lo, hi).
func (f foldedText) span(page string, lo, hi int) string {
	return page[f.start[lo]:f.end[hi-1]]
}
