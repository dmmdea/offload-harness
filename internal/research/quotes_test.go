package research

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/delegate"
)

// A page as a browser renders it to text: typographic quotes and dashes, a
// no-break space, a line wrapped mid-sentence.
const uiPage = "Press the \u201CSave changes\u201D button \u2014 the dialog closes.\n" +
	"Use File \u2013 Export to write a copy; the\u00A0export dialog\nremembers its last folder.\n" +
	"alpha beta gamma delta epsilon"

func digestWith(quotes ...string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"key_facts": []string{"one fact"}, "numbers": []string{}, "quotes": quotes, "verdict": "ok",
	})
	return b
}

func quotesOf(t *testing.T, structured json.RawMessage) []string {
	t.Helper()
	var d struct {
		Quotes []string `json:"quotes"`
	}
	if err := json.Unmarshal(structured, &d); err != nil {
		t.Fatalf("structured is not a digest: %v: %s", err, structured)
	}
	return d.Quotes
}

// The seat's quote is already the page's bytes: nothing is rewritten and the
// report says so, so an absent flag is never confused with an unchecked digest.
func TestVerifyQuotesLeavesAnExactQuoteAlone(t *testing.T) {
	const exact = "The driven path uses shared memory transfers between processes."
	out, qc := VerifyQuotes(prosePage, digestWith(exact))
	if qc == nil {
		t.Fatal("no report for a digest that carries a quote")
	}
	if want := (&delegate.QuoteCheck{Checked: 1, Verbatim: 1}); !reflect.DeepEqual(qc, want) {
		t.Fatalf("report = %+v, want %+v", qc, want)
	}
	if got := quotesOf(t, out); !reflect.DeepEqual(got, []string{exact}) {
		t.Fatalf("quotes = %q, want the exact quote untouched", got)
	}
}

// A paraphrase is what F4 reported: it reads like the page and cannot be found
// in it. It is flagged and kept where the seat put it, never dropped, and the
// digest does not fail.
func TestVerifyQuotesFlagsAParaphraseAndKeepsIt(t *testing.T) {
	const paraphrase = "Pinned staging is slower than shared handles."
	const exact = "The driven path uses shared memory transfers between processes."
	out, qc := VerifyQuotes(prosePage, digestWith(paraphrase, exact))
	if qc == nil || qc.Checked != 2 || qc.Verbatim != 1 || qc.Corrected != 0 {
		t.Fatalf("report = %+v, want 2 checked and 1 verbatim", qc)
	}
	if !reflect.DeepEqual(qc.Unverified, []string{paraphrase}) {
		t.Fatalf("unverified = %q, want the paraphrase named as the seat wrote it", qc.Unverified)
	}
	if !strings.Contains(qc.Note, "not on the fetched page") {
		t.Fatalf("note = %q, want it to say the unverified quotes are not on the page", qc.Note)
	}
	if got := quotesOf(t, out); !reflect.DeepEqual(got, []string{paraphrase, exact}) {
		t.Fatalf("quotes = %q, want order and content kept (an unverified quote is flagged, not dropped)", got)
	}
}

// Locate tolerantly, publish exactly: a quote the seat typed with straight
// quote marks, an ASCII dash, a single space and another case is found, and the
// published item is the page's own span, byte for byte.
func TestVerifyQuotesReplacesATolerantMatchWithThePagesOwnSpan(t *testing.T) {
	const want = "Press the \u201CSave changes\u201D button \u2014 the dialog closes."
	const wrapped = "Use File \u2013 Export to write a copy; the\u00A0export dialog\nremembers its last folder."
	if !strings.Contains(uiPage, want) || !strings.Contains(uiPage, wrapped) {
		t.Fatal("fixture drifted: the wanted spans must be on the page")
	}
	seat := []string{
		`press the "save changes" button - the dialog closes.`,
		`Use File - Export to write a copy; the export dialog remembers its last folder.`,
	}
	out, qc := VerifyQuotes(uiPage, digestWith(seat...))
	if qc == nil || qc.Checked != 2 || qc.Verbatim != 0 || qc.Corrected != 2 || len(qc.Unverified) != 0 {
		t.Fatalf("report = %+v, want both quotes corrected", qc)
	}
	got := quotesOf(t, out)
	if !reflect.DeepEqual(got, []string{want, wrapped}) {
		t.Fatalf("quotes = %q, want the page's own spans %q", got, []string{want, wrapped})
	}
	for _, q := range got {
		if !strings.Contains(uiPage, q) {
			t.Fatalf("published quote %q is not a byte-for-byte span of the page", q)
		}
	}
}

// A quote wrapped in marks of its own, or closed with a full stop the page does
// not have, is the page's words all the same: the published item is the page's
// span, with the page's own marks if it has them around the words.
func TestVerifyQuotesSetsAsideWrappingMarksAndTrailingPunctuation(t *testing.T) {
	// uiPage has curly marks around Save changes, so the straight-quoted copy is that span, marks included.
	out, qc := VerifyQuotes(uiPage, digestWith(`"Save changes"`, `Save changes.`, `Save changes`))
	if got, want := quotesOf(t, out), []string{"\u201CSave changes\u201D", "Save changes", "Save changes"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("quotes = %q, want %q", got, want)
	}
	if qc == nil || qc.Checked != 3 || qc.Verbatim != 1 || qc.Corrected != 2 || len(qc.Unverified) != 0 {
		t.Fatalf("report = %+v, want 1 verbatim and 2 corrected", qc)
	}
	// A page with no marks around the words: the quote's own wrapping is not part of the page's text.
	out, qc = VerifyQuotes("The Save button is on the toolbar.", digestWith(`"Save button"`, `'Save button'`))
	if got, want := quotesOf(t, out), []string{"Save button", "Save button"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("quotes = %q, want %q", got, want)
	}
	if qc == nil || qc.Corrected != 2 || len(qc.Unverified) != 0 {
		t.Fatalf("report = %+v, want both corrected", qc)
	}
}

// An elided quote ("first words ... last words") is found when each part is on
// the page in order; the parts out of order are not.
func TestVerifyQuotesAcceptsAnElisionOnlyInPageOrder(t *testing.T) {
	out, qc := VerifyQuotes(uiPage, digestWith("alpha ... delta", "Alpha \u2026 DELTA", "delta ... alpha"))
	if qc == nil || qc.Checked != 3 || qc.Verbatim != 1 || qc.Corrected != 1 || !reflect.DeepEqual(qc.Unverified, []string{"delta ... alpha"}) {
		t.Fatalf("report = %+v, want 1 verbatim, 1 corrected, the reversed one unverified", qc)
	}
	got := quotesOf(t, out)
	if got[0] != "alpha ... delta" || got[1] != "alpha ... delta" || got[2] != "delta ... alpha" {
		t.Fatalf("quotes = %q", got)
	}
}

// The check is the quotes' and nothing else's: the other fields, a caller's own
// extras, and the order of the list come through unchanged.
func TestVerifyQuotesTouchesOnlyTheQuotesField(t *testing.T) {
	in := json.RawMessage(`{"verdict":"v","extra":{"n":1},"quotes":["PRESS THE \"SAVE CHANGES\" BUTTON"],"key_facts":["a","b"]}`)
	out, qc := VerifyQuotes("Press the \"Save changes\" button now.", in)
	if qc == nil || qc.Corrected != 1 {
		t.Fatalf("report = %+v", qc)
	}
	var got, want map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("out is not JSON: %v: %s", err, out)
	}
	_ = json.Unmarshal(in, &want)
	for _, k := range []string{"verdict", "extra", "key_facts"} {
		if string(got[k]) != string(want[k]) {
			t.Fatalf("field %q = %s, want %s", k, got[k], want[k])
		}
	}
	if string(got["quotes"]) != `["Press the \"Save changes\" button"]` {
		t.Fatalf("quotes = %s", got["quotes"])
	}
}

// Nothing to look up is not a finding: a digest with no quotes field, an empty
// list, a field of another shape, an empty item or no JSON at all is returned
// as it came, with no report.
func TestVerifyQuotesHasNothingToSayWithoutQuotes(t *testing.T) {
	for name, in := range map[string]string{
		"no quotes field":      `{"key_facts":["a"],"verdict":"v"}`,
		"an empty list":        `{"quotes":[],"verdict":"v"}`,
		"a null list":          `{"quotes":null}`,
		"a string, not a list": `{"quotes":"the whole page"}`,
		"only empty items":     `{"quotes":["", "  "]}`,
		"only non-strings":     `{"quotes":[1,{"a":2}]}`,
		"not JSON":             `not json`,
		"empty":                ``,
	} {
		out, qc := VerifyQuotes(prosePage, json.RawMessage(in))
		if qc != nil {
			t.Errorf("%s: report %+v, want none", name, qc)
		}
		if string(out) != in {
			t.Errorf("%s: structured = %s, want it returned unchanged", name, out)
		}
	}
}

// Skipped pages produce no result, so the map from results to pages leaves them out.
func TestResultSourcesMapsResultsToTheirPages(t *testing.T) {
	sources := []Source{{Index: 0}, {Index: 1, Skipped: "http 404"}, {Index: 2}}
	if got, want := ResultSources(sources), []int{0, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ResultSources = %v, want %v (skipped pages produce no result)", got, want)
	}
}

// Each result is looked up in ITS page, found through the result->source map,
// never in the page of the same position or of another source.
func TestAnnotateWireChecksEachResultAgainstItsOwnPage(t *testing.T) {
	const pageA = "Alpha page says the first thing exactly."
	const pageB = "Beta page says the second thing exactly."
	fetched := []Fetched{{Text: pageA}, {Err: "http 404"}, {Text: pageB}}
	wire := delegate.ResponseWire{Results: []delegate.ResultWire{
		{Structured: digestWith("the first thing exactly", "the second thing exactly")}, // page A: second is page B's
		{Structured: digestWith("the second thing exactly")},                            // page B
	}}
	AnnotateWire(&wire, []int{0, 2}, fetched)
	a, b := wire.Results[0].QuoteCheck, wire.Results[1].QuoteCheck
	if a == nil || a.Verbatim != 1 || !reflect.DeepEqual(a.Unverified, []string{"the second thing exactly"}) {
		t.Fatalf("result 0 report = %+v, want page B's quote unverified against page A", a)
	}
	if b == nil || b.Verbatim != 1 || len(b.Unverified) != 0 {
		t.Fatalf("result 1 report = %+v, want it verified against page B (source 2)", b)
	}
}

// A deferred or failed result carries no digest, and a result with no matching
// source (a longer result list than the map) is left alone.
func TestAnnotateWireSkipsResultsWithNoDigest(t *testing.T) {
	fetched := []Fetched{{Text: prosePage}}
	wire := delegate.ResponseWire{Results: []delegate.ResultWire{
		{Deferred: true, Structured: digestWith("not on the page at all")},
		{Failed: true, Structured: digestWith("not on the page at all")},
		{},
		{Structured: digestWith("not on the page at all")}, // no source mapped for this one
	}}
	AnnotateWire(&wire, []int{0, 0, 0}, fetched)
	for i, r := range wire.Results {
		if r.QuoteCheck != nil {
			t.Errorf("result %d: report %+v, want none", i, r.QuoteCheck)
		}
	}
}

// The report rides the result row as `quote_check`, and only when there is one.
func TestQuoteCheckIsPublishedOnTheResultRowOnlyWhenPresent(t *testing.T) {
	wire := delegate.ResponseWire{Results: []delegate.ResultWire{
		{Structured: digestWith("Pinned staging is slower than shared handles.")},
		{},
	}}
	AnnotateWire(&wire, []int{0, 0}, []Fetched{{Text: prosePage}})
	b, err := json.Marshal(wire.Results)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	if _, ok := rows[0]["quote_check"]; !ok {
		t.Fatalf("row 0 has no quote_check: %s", b)
	}
	if _, ok := rows[1]["quote_check"]; ok {
		t.Fatalf("row 1 carries a quote_check it has no digest for: %s", b)
	}
}

// A page that wraps a soft hyphen or a zero-width space into its words is found
// by the words alone, and the published span is the page's, characters included.
func TestVerifyQuotesSeesThroughInvisibleCharacters(t *testing.T) {
	const page = "Choose Save\u200B changes or Dis\u00ADcard to leave."
	out, qc := VerifyQuotes(page, digestWith("Save changes", "Discard"))
	if qc == nil || qc.Corrected != 2 || len(qc.Unverified) != 0 {
		t.Fatalf("report = %+v, want both found through the invisible characters", qc)
	}
	got := quotesOf(t, out)
	if want := []string{"Save\u200B changes", "Dis\u00ADcard"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("quotes = %q, want the page's own spans %q", got, want)
	}
}

// The page's text is not HTML: an ampersand or an angle bracket in a corrected
// quote stays as the page has it instead of becoming a \u0026 escape.
func TestVerifyQuotesDoesNotHTMLEscapeThePagesText(t *testing.T) {
	out, qc := VerifyQuotes("Read the Terms & Conditions <here> first.", digestWith("terms & conditions <here>"))
	if qc == nil || qc.Corrected != 1 {
		t.Fatalf("report = %+v", qc)
	}
	if !strings.Contains(string(out), `"Terms & Conditions <here>"`) {
		t.Fatalf("structured = %s, want the span unescaped", out)
	}
}
