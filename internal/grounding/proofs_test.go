package grounding

import (
	"errors"
	"testing"
)

func TestPathsExistFlagsOnlyMissingPaths(t *testing.T) {
	data := []byte(`{"config":"C:\\real\\config.json","script":"C:\\gone\\missing.mjs","name":"not a path"}`)
	res := ProvePathsExist(data, func(p string) error {
		if p == `C:\real\config.json` {
			return nil
		}
		return errors.New("not found")
	})
	if !res.Applicable {
		t.Fatal("two path-shaped values were present; validator reported inapplicable")
	}
	if res.Checked != 2 {
		t.Fatalf("Checked = %d, want 2 — 'not a path' must not be stat'ed", res.Checked)
	}
	if len(res.Failures) != 1 || res.Failures[0] != `C:\gone\missing.mjs` {
		t.Fatalf("failures = %v, want just the missing path", res.Failures)
	}
	if res.OK() {
		t.Fatal("OK() true despite a failure")
	}
}

// The distinction that keeps "we ran the checks" meaningful: nothing to check is a NON-ANSWER,
// not a pass. Collapsing the two is the silent-failure shape this estate keeps hitting.
func TestNoCandidatesIsInapplicableNotAPass(t *testing.T) {
	res := ProvePathsExist([]byte(`{"summary":"no paths here at all"}`), func(string) error { return nil })
	if res.Applicable {
		t.Fatal("no path-shaped values, but validator claimed to be applicable")
	}
	if res.Checked != 0 {
		t.Fatalf("Checked = %d, want 0", res.Checked)
	}
	if res.OK() {
		t.Fatal("OK() must be false when nothing was checked — an inapplicable validator has not passed")
	}
}

// THE GAP Check() CANNOT CLOSE. Every word of this quote appears in the source, so the
// existing per-value grounding test is satisfied, yet the quote itself was never said.
func TestCitedSpanCatchesAQuoteAssembledFromRealWords(t *testing.T) {
	input := "The report notes revenue rose. Separately, costs fell in the same quarter."
	data := []byte(`{"finding":"The report notes \"revenue rose and costs fell\" this quarter."}`)
	res := ProveCitedSpans(input, data)
	if !res.Applicable || res.Checked != 1 {
		t.Fatalf("expected one citation checked, got applicable=%v checked=%d", res.Applicable, res.Checked)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("an invented contiguous quote was not flagged: %+v", res)
	}
}

func TestCitedSpanAcceptsReflowedAndRecasedQuotes(t *testing.T) {
	input := "The system\n  returns   an empty result\nwhen the schema has no usable properties."
	data := []byte(`{"finding":"It says \"returns an empty RESULT when the schema\" is bad."}`)
	res := ProveCitedSpans(input, data)
	if len(res.Failures) != 0 {
		t.Fatalf("whitespace/case differences were flagged as fabrication: %v", res.Failures)
	}
	if !res.OK() {
		t.Fatal("a genuine quote differing only in whitespace/case should pass")
	}
}

// Short quoted fragments are punctuation or emphasis, not citations. Checking them produces
// constant false alarms that bury the real signal.
func TestShortQuotesAreNotTreatedAsCitations(t *testing.T) {
	res := ProveCitedSpans("anything at all", []byte(`{"note":"the \"ok\" flag and the \"two words\" case"}`))
	if res.Applicable {
		t.Fatalf("short fragments were treated as citations: %+v", res)
	}
}

// The precondition that reassigned slot 1: extract is GBNF-constrained, so structurally
// invalid JSON cannot reach these validators. They must therefore degrade quietly rather than
// pretend to have checked something.
func TestMalformedJSONIsInapplicableNotAFailure(t *testing.T) {
	for _, res := range []ProofResult{
		ProvePathsExist([]byte(`{not json`), func(string) error { return nil }),
		ProveCitedSpans("src", []byte(`{not json`)),
	} {
		if res.Applicable || len(res.Failures) != 0 {
			t.Fatalf("%s: malformed JSON should be inapplicable with no failures, got %+v", res.Name, res)
		}
	}
}

// Regression guard for the mispairing bug above: with a length minimum inside the regex, the
// closing quote of a SHORT span pairs with the opening quote of the next one and the text
// BETWEEN quotes is captured as if it were a citation.
func TestQuotePairingIsAdjacentNotGreedy(t *testing.T) {
	got := quotedSpans(`the "ok" flag and the "two words here" case`)
	want := []string{"ok", "two words here"}
	if len(got) != len(want) {
		t.Fatalf("spans = %q, want %q — inter-quote text was captured as a span", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("span %d = %q, want %q", i, got[i], want[i])
		}
	}
}
