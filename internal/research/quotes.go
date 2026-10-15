package research

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/quotecheck"
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
	folds := quotecheck.NewFolds(page)
	changed := false
	for i, it := range items {
		var q string
		if json.Unmarshal(it, &q) != nil || strings.TrimSpace(q) == "" {
			continue
		}
		qc.Checked++
		span, how := quotecheck.Locate(page, folds, q)
		switch how {
		case quotecheck.Verbatim:
			qc.Verbatim++
		case quotecheck.Corrected:
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
