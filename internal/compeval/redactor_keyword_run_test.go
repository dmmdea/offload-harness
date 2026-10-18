package compeval

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/piishape"
)

// The residual gate fails closed: after redaction, a credential header keyword with a long value run a few characters
// behind it is not safe to keep, whatever shape stands between them. The class table reads shapes, and every shape it
// does not know (a YAML list item, a backtick string, a HAR name/value pair, an XML attribute) used to leave the value
// in the text with the gate clean.

// gateForms are texts in which the cookie class has no opener; each holds the value a.
func gateForms(a string) map[string]string {
	return map[string]string{
		"yaml block list, three levels": "request:\n  headers:\n    " + spanSetCk + ":\n      - 'sid=" + a + "'\n",
		"yaml block list, one level":    spanCk + ":\n  - \"sid=" + a + "\"\n",
		"backtick, Header.Set":          `req.Header.Set("` + spanCk + `", ` + "`sid=" + a + "`)",
		"backtick, Header.Add":          `w.Header().Add("` + spanSetCk + `", ` + "`sid=" + a + "; Path=/; HttpOnly`)",
		"backtick, javascript":          "headers.set('" + spanCk + "', `sid=" + a + "`)",
		"har name and value":            `{"name":"` + spanCk + `","value":"sid=` + a + `"}`,
		"har set-cookie":                `{"log":{"entries":[{"response":{"headers":[{"name":"` + spanSetCk + `","value":"sid=` + a + `; Path=/"}]}}]}}`,
		"xml attributes":                `<header name="` + spanCk + `" value="sid=` + a + `"/>`,
		"entity-escaped quotes":         "<" + spanCk + ">sid=&quot;" + a + "&quot;</" + spanCk + ">",
		"digest scheme":                 "Authorization: Digest " + a,
	}
}

// TestResidualFiresWhereTheClassesHaveNoShape: for each such text, after the redactor has run, either the value is gone or
// the residual gate names the keyword check; the value never stays in the text with the gate quiet.
func TestResidualFiresWhereTheClassesHaveNoShape(t *testing.T) {
	a := tailFake(7201, 22, tailAlnum)
	for name, text := range gateForms(a) {
		r := NewRedactor()
		out := r.Redact(text)
		res := r.Residual(out)
		fired := false
		for _, c := range res {
			fired = fired || c == piishape.KeywordRunClass
		}
		if _, leaked := anyRun(a, out, 8); leaked && !fired {
			t.Errorf("%s: the value is still in %q and the residual gate is quiet (%v)", name, out, res)
		}
		if !fired && strings.Contains(out, a) {
			t.Errorf("%s: the gate does not name %s on %q", name, piishape.KeywordRunClass, out)
		}
	}
}

// TestResidualIsQuietOnARedactedHeader: a header the classes took leaves a placeholder, and the placeholder carries the
// keyword in its name and is a long run itself; neither fires the gate.
func TestResidualIsQuietOnARedactedHeader(t *testing.T) {
	a, b := tailFake(7211, 22, tailAlnum), tailFake(7212, 36, tailAlnum)
	for name, text := range map[string]string{
		"cookie line":  spanCk + ": sid=" + a + "; theme=dark\nHost: api.example.invalid/v1/users",
		"set cookie":   spanSetCk + ": sid=" + a + "; Path=/\nContent-Type: application/json",
		"bearer":       "Authorization: Bearer " + a + "\nAccept: application/json",
		"basic":        "Authorization: Basic " + b + "\nAccept: application/json",
		"json key":     `{"` + spanCk + `":"sid=` + a + `","host":"api.example.invalid"}`,
		"in Args":      reviewJSON(t, map[string]string{"cmd": spanCk + ": sid=" + a + "\nHost: api.example.invalid/v1/users"}),
		"api key line": "X-Api-" + "Key: " + a + "\nAccept: application/json",
	} {
		r := NewRedactor()
		out := r.Redact(text)
		if res := r.Residual(out); len(res) != 0 {
			t.Errorf("%s: residual %v on %q", name, res, out)
		}
	}
}

// TestHarvestSkipsATraceWithAKeywordBeforeALongValue: the trace is left out with a note that names the check and does not
// echo the text, and the others are kept.
func TestHarvestSkipsATraceWithAKeywordBeforeALongValue(t *testing.T) {
	a := tailFake(7221, 22, tailAlnum)
	dir := t.TempDir()
	rec := sampleTrace("gate", "HTTP/1.1 200 OK\nreq.Header.Set(\""+spanCk+"\", `sid="+a+"`)\n")
	writeTraceFile(t, dir, "a-gate.json", rec)
	writeTraceFile(t, dir, "b-clean.json", sampleTrace("clean", "ok  \texample.invalid/pkg\t0.012s\n"))
	entries, stats, err := HarvestTraces(dir, HarvestOpts{})
	if err != nil {
		t.Fatalf("harvest refused: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "hv-b-clean" {
		t.Fatalf("want only the clean trace kept, got %d entries", len(entries))
	}
	if len(stats.Skipped) != 1 || stats.Skipped[0].File != "a-gate.json" {
		t.Fatalf("want a-gate.json skipped, got %+v", stats.Skipped)
	}
	reason := stats.Skipped[0].Reason
	if !strings.Contains(reason, "credential keyword") || strings.Contains(reason, a) {
		t.Errorf("the skip note should name the keyword check and not echo the text: %q", reason)
	}
	b, _ := json.Marshal(entries)
	if strings.Contains(string(b), a) {
		t.Errorf("the value is in the harvested entries")
	}
}

// TestHarvestKeepsATraceThatNamesAHeaderAndNoValue: a trace that talks about the headers, with no long run near the
// keyword, is kept.
func TestHarvestKeepsATraceThatNamesAHeaderAndNoValue(t *testing.T) {
	dir := t.TempDir()
	writeTraceFile(t, dir, "a.json", sampleTrace("names", "the "+spanCk+" header and the Authorization header are set by the proxy; see RFC 6265\n"))
	entries, stats, err := HarvestTraces(dir, HarvestOpts{})
	if err != nil || len(entries) != 1 || len(stats.Skipped) != 0 {
		t.Fatalf("want the trace kept: err %v, %d entries, skips %+v", err, len(entries), stats.Skipped)
	}
}

// TestResidualFiresWhenAYamlListContinuesBehindARedactedHeader: the cookie class redacts a YAML block list up to the quote that
// closes its first item, and the header name goes with it, so the items behind it have no keyword in front of them any more.
// The gate sees the list continue behind the placeholder.
func TestResidualFiresWhenAYamlListContinuesBehindARedactedHeader(t *testing.T) {
	a, b := tailFake(7231, 22, tailAlnum), tailFake(7232, 22, tailAlnum)
	for name, text := range map[string]string{
		"single quotes": spanSetCk + ":\n  - 'a=" + a + "; Path=/'\n  - 'b=" + b + "'\n",
		"double quotes": "headers:\n  " + spanCk + ":\n    - \"sid=" + a + "\"\n    - \"b=" + b + "\"\n",
		"in Args":       reviewJSON(t, map[string]string{"cmd": spanSetCk + ":\n  - 'a=" + a + "; Path=/'\n  - 'b=" + b + "'\n"}),
	} {
		r := NewRedactor()
		out := r.Redact(text)
		if _, ok := anyRun(a, out, 8); ok {
			t.Errorf("%s: the first item is still in %q", name, out)
		}
		res := r.Residual(out)
		fired := false
		for _, c := range res {
			fired = fired || c == piishape.KeywordRunClass
		}
		if _, leaked := anyRun(b, out, 8); leaked && !fired {
			t.Errorf("%s: the second item is still in %q and the residual gate is quiet (%v)", name, out, res)
		}
	}
}
