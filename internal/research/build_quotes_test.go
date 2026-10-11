package research

import (
	"encoding/json"
	"strings"
	"testing"
)

// The length rule the goal already carries ("at most 18 words each") invites a
// seat to shorten what it quotes, and a goal that asks for a page's exact words
// (a label, a button's text) got a paraphrase in key_facts (register F4). The
// default digest's goal now says where exact wording goes and what copying it
// means, and still ends with the verdict ask (build_verdict_test.go).
func TestTheDefaultGoalAsksForExactWordsInQuotesCopiedFromTheFile(t *testing.T) {
	const verdictAsk = "Always end with a one-sentence verdict that answers the goal from this page, or says plainly that the page does not address it."
	for name, page := range map[string]string{"an anchored page": prosePage, "a page too thin for an anchor": thinPage} {
		specs, _ := Build(Request{Goal: "List the button labels."}, pageWith(page))
		g := specs[0].Goal
		for _, want := range []string{
			"exact words",
			"`quotes`",
			"character for character from the file",
			"never shortened, reworded, translated or re-cased",
			"a shorter exact stretch of it rather than a paraphrase",
		} {
			if strings.Count(g, want) != 1 {
				t.Fatalf("%s: goal does not carry %q exactly once: %q", name, want, g)
			}
		}
		if !strings.HasSuffix(g, verdictAsk) {
			t.Fatalf("%s: the verdict ask must stay the goal's last sentence: %q", name, g)
		}
		if strings.Index(g, "character for character") > strings.Index(g, verdictAsk) {
			t.Fatalf("%s: the quote sentence must come before the verdict ask: %q", name, g)
		}
	}
}

// The sentence is about the `quotes` field: a caller's schema that has one gets
// it (the field name is the promise), one that has none does not, and the anchor
// words still come from the caller's goal alone.
func TestTheQuoteSentenceFollowsTheQuotesField(t *testing.T) {
	withQuotes := json.RawMessage(`{"type":"object","properties":{"quotes":{"type":"array","items":{"type":"string"}},"rows":{"type":"array","items":{"type":"string"}}}}`)
	without := json.RawMessage(`{"type":"object","properties":{"rows":{"type":"array","items":{"type":"string"}}}}`)
	a, _ := Build(Request{Goal: "List the button labels.", OutputSchema: withQuotes}, pageWith(prosePage))
	if !strings.Contains(a[0].Goal, "character for character from the file") {
		t.Fatalf("a caller's schema with a quotes field got no quote sentence: %q", a[0].Goal)
	}
	if strings.Contains(a[0].Goal, "verdict") {
		t.Fatalf("a caller's schema goal gained a verdict ask: %q", a[0].Goal)
	}
	b, _ := Build(Request{Goal: "List the button labels.", OutputSchema: without}, pageWith(prosePage))
	if strings.Contains(b[0].Goal, "character for character") || strings.Contains(b[0].Goal, "`quotes`") {
		t.Fatalf("a caller's schema without a quotes field got the quote sentence: %q", b[0].Goal)
	}
}
