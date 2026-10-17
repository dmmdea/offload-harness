package quotecheck

import "testing"

const page = "Build output\n" +
	"    parse_test.go:42: got 3 items, want 4\n" +
	"FAIL\texample.invalid/pkg/parse\t0.034s\n" +
	"Terms & Conditions: “accept” — continue\n" +
	"another run, no problems\n"

func locate(q string) (string, How) { return Locate(page, NewFolds(page), q) }

// A quote that is the text's own bytes is returned as it is, with its edge spaces trimmed.
func TestLocateVerbatimIsTheTextsOwnBytes(t *testing.T) {
	got, how := locate("  parse_test.go:42: got 3 items, want 4 \n")
	if how != Verbatim || got != "parse_test.go:42: got 3 items, want 4" {
		t.Fatalf("got %q, %v; want the trimmed quote, Verbatim", got, how)
	}
}

// Case, spacing, typographic quote marks and dashes are set aside, and the text's own span is what is returned.
func TestLocateFoldsAndPublishesTheTextsSpan(t *testing.T) {
	for in, want := range map[string]string{
		"PARSE_TEST.GO:42: GOT 3 ITEMS, WANT 4":     "parse_test.go:42: got 3 items, want 4",
		"parse_test.go:42:   got 3 items,\twant 4":  "parse_test.go:42: got 3 items, want 4",
		"\"accept\" - continue":                     "“accept” — continue",
		"FAIL example.invalid/pkg/parse 0.034s":     "FAIL\texample.invalid/pkg/parse\t0.034s",
		"terms & conditions: \"accept\" - continue": "Terms & Conditions: “accept” — continue",
	} {
		got, how := locate(in)
		if how != Corrected || got != want {
			t.Errorf("locate(%q) = %q, %v; want %q, Corrected", in, got, how, want)
		}
	}
}

// Failing that, only the words are compared: a colon a seat added, a table's cell borders.
func TestLocateLooseSetsPunctuationAside(t *testing.T) {
	got, how := locate("Terms & Conditions accept continue")
	if how != Corrected || got == "" {
		t.Fatalf("got %q, %v; want a Corrected span", got, how)
	}
}

// "first words ... last words": both parts, in order.
func TestLocateElisionNeedsBothPartsInOrder(t *testing.T) {
	if got, how := locate("Build output ... 0.034s"); how != Verbatim || got != "Build output ... 0.034s" {
		t.Errorf("in-order elision: %q, %v", got, how)
	}
	if _, how := locate("0.034s ... Build output"); how != Unverified {
		t.Errorf("out-of-order elision found: %v", how)
	}
}

// What is not in the text is Unverified, and a word is not found inside a longer word.
func TestLocateRefusesWhatIsNotThere(t *testing.T) {
	for _, q := range []string{"panic: runtime error: index out of range", "THER", "Output Build"} {
		if got, how := locate(q); how != Unverified || got != q {
			t.Errorf("locate(%q) = %q, %v; want the quote back, Unverified", q, got, how)
		}
	}
}
