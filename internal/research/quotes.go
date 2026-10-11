package research

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/delegate"
)

// The `quotes` of a research digest promise the page's own words, and a seat that
// paraphrases them hands a fact-checker text no search of the page can find
// (register F4: a goal that asked for exact UI labels got reworded digests). The
// goal now asks for copies (verbatimQuotesAsk), and this file is the delegator's
// half: after the engine returns, every quote is looked up in the page that
// digest was written from.
//
//   - Locate tolerantly, publish exactly. A quote is found with case, spacing,
//     typographic quote marks and dashes set aside and, failing that, with
//     punctuation set aside too (the seat's added colon, a table's cell borders),
//     and when it is found the published item is the PAGE's span, so it is the
//     page's bytes by construction. This is the extraction step: the seat points, the delegator
//     copies.
//   - A quote found nowhere is FLAGGED, never dropped and never failed. An
//     acceptance failure discards a whole run (register C15, 36 pages lost to
//     nonempty:verdict), and a new acceptance verb would be refused at the ACK by
//     every node one release behind, so the check lives here, outside the contract.
//   - The delegation-log corpus and the ledger rows are written inside the engine,
//     before this runs, so they keep the seat's raw answer; the published body
//     carries the corrected one.

// verbatimQuotesAsk is the sentence Build adds to a goal whose schema has a
// `quotes` list. It routes a request for exact wording to that list and says what
// copying means, because the length rule just before it ("at most 18 words each")
// invites a seat to shorten what it quotes.
const verbatimQuotesAsk = " Anything the goal asks for in the page's exact words (a quotation, a label, button or menu text) goes in `quotes`, copied character for character from the file: never shortened, reworded, translated or re-cased; where a passage is too long, copy a shorter exact stretch of it rather than a paraphrase."

// schemaHasQuotes reports whether a digest schema declares a `quotes` property.
func schemaHasQuotes(schema json.RawMessage) bool {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return false
	}
	_, ok := s.Properties["quotes"]
	return ok
}

// ResultSources maps a research call's results to the pages they came from: one
// result per usable source, in source order, so a skipped (failed or refused)
// page has none.
func ResultSources(sources []Source) []int {
	out := make([]int, 0, len(sources))
	for _, s := range sources {
		if s.Skipped == "" {
			out = append(out, s.Index)
		}
	}
	return out
}

// AnnotateWire checks the quotes of every finished digest in wire against the
// page it was written from (resultSources[i] is result i's source index), replaces
// quotes found only after folding with the page's own span, and attaches the
// report as the result's QuoteCheck. A deferred or failed result, one with no
// digest, and one with no mapped page are left alone.
func AnnotateWire(wire *delegate.ResponseWire, resultSources []int, fetched []Fetched) {
	for i := range wire.Results {
		if i >= len(resultSources) {
			return
		}
		r := &wire.Results[i]
		src := resultSources[i]
		if r.Deferred || r.Failed || len(r.Structured) == 0 || src < 0 || src >= len(fetched) {
			continue
		}
		r.Structured, r.QuoteCheck = VerifyQuotes(fetched[src].Text, r.Structured)
	}
}

// VerifyQuotes looks every item of the digest's `quotes` list up in page. It
// returns the digest with each tolerantly-found quote replaced by the page's own
// span, and the report; a digest with nothing to look up (no `quotes` list, an
// empty one, a field of another shape, not JSON) comes back byte for byte with a
// nil report, and so does one whose quotes were all already the page's bytes.
func VerifyQuotes(page string, structured json.RawMessage) (json.RawMessage, *delegate.QuoteCheck) {
	if len(structured) == 0 {
		return structured, nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(structured, &obj) != nil {
		return structured, nil
	}
	raw, ok := obj["quotes"]
	if !ok {
		return structured, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return structured, nil
	}
	qc := &delegate.QuoteCheck{}
	folds := &pageFolds{page: page}
	changed := false
	for i, it := range items {
		var q string
		if json.Unmarshal(it, &q) != nil || strings.TrimSpace(q) == "" {
			continue
		}
		qc.Checked++
		span, how := locateQuote(page, folds, q)
		switch how {
		case quoteVerbatim:
			qc.Verbatim++
		case quoteCorrected:
			qc.Corrected++
		default:
			qc.Unverified = append(qc.Unverified, q)
			continue
		}
		if span != q {
			b, err := marshalNoEscape(span)
			if err != nil {
				continue
			}
			items[i] = b
			changed = true
		}
	}
	if qc.Checked == 0 {
		return structured, nil
	}
	if len(qc.Unverified) > 0 {
		qc.Note = fmt.Sprintf("%d of %d quotes are not on the fetched page (case, spacing and punctuation set aside): they are the seat's wording, not the page's, and cannot be checked against it", len(qc.Unverified), qc.Checked)
	}
	if !changed {
		return structured, qc
	}
	list, err := marshalNoEscape(items)
	if err != nil {
		return structured, qc
	}
	obj["quotes"] = list
	out, err := marshalNoEscape(obj)
	if err != nil {
		return structured, qc
	}
	return out, qc
}

// marshalNoEscape is json.Marshal without HTML escaping: a quote such as
// "Terms & Conditions" must stay readable, and the page's text is not HTML.
func marshalNoEscape(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

type quoteHow int

const (
	quoteUnverified quoteHow = iota
	quoteVerbatim            // the page's bytes already (after trimming the item's own edge spaces)
	quoteCorrected           // found only after folding; the span is the page's own
)

// elision splits a quote at an ellipsis ("first words ... last words"), the
// bracketed form included.
var elision = regexp.MustCompile(`\s*(?:\[\s*(?:\.\.\.|\x{2026})\s*\]|\.\.\.|\x{2026})\s*`)

// pageFolds holds the folded forms of one page, each built on first use.
type pageFolds struct {
	page          string
	strict, loose *foldedText
}

func (p *pageFolds) get(loose bool) foldedText {
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

// locateQuote returns the text to publish for q and how it was found. The page's
// own bytes come first (what a reader's find-in-page does); then the tolerant
// forms, strict before loose: strict sets case, spacing, quote marks and dashes
// aside and keeps the rest of the punctuation, loose compares the words alone
// (a colon a seat added after a label, a table's cell borders).
func locateQuote(page string, folds *pageFolds, q string) (string, quoteHow) {
	raw := strings.TrimSpace(q)
	if strings.Contains(page, raw) {
		return raw, quoteVerbatim
	}
	parts := splitElision(raw)
	if len(parts) > 1 && inOrder(page, parts) {
		return raw, quoteVerbatim
	}
	for _, loose := range []bool{false, true} {
		f := folds.get(loose)
		if s, ok := findFolded(page, f, []string{raw}, loose); ok {
			return s, quoteCorrected
		}
		if len(parts) > 1 {
			if s, ok := findFolded(page, f, parts, loose); ok {
				return s, quoteCorrected
			}
		}
	}
	return q, quoteUnverified
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
