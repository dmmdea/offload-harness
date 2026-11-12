package delegate

// A digest that is verifiably about its own page.
//
// The document fingerprint (the `docanchor` regex research.AnchorCheck writes) asks the loop's final prose to restate one of the
// page's top prose words, and the words are chosen so the goal cannot restate them. A digest that answers a narrow goal restates
// the goal's words and the page's figures, not twenty-four of the page's other words, so the check failed answers that were
// right: in the 30 days to 2026-11-12, 282 research digests failed it, and 125 of them quote the page they were handed, word for
// word, at a length another page does not hold by chance. The fingerprint exists to catch an answer from memory or about
// another document; a digest that quotes its page is neither, and that is a fact the delegator can check without any model: it
// holds the page.
//
// DigestGrounded is that check, and like HonestAbsence it only EXCUSES a failed fingerprint (excusesFingerprint). It reads the
// decoded digest (every string at any depth, never a key and never the loop's prose), looks each string up in the contract's
// context documents with the lookup the research digest's quote check uses (quotecheck.Locate: case, spacing, quote marks and
// dashes set aside), and passes when enough distinct strings are found. Three rules keep it from becoming a second way for a
// wrong answer to pass:
//
//   - A string the GOAL also contains counts for nothing, as no goal word is ever an anchor: the goal echoed back, the page's
//     title and URL (both are in the goal), and candidate sentences a caller pasted into the goal cannot ground a digest.
//   - A string must be long enough that another page could not hold it by chance (groundedMinRunes), and a figure or a label
//     alone is too short: a rule that also counted three digit-bearing strings of four or more characters grounded 0.2 % of the
//     digests paired with an unrelated page (1.2 % at three characters), against 0.05 % for the rule below.
//   - One string is enough only when it is a whole sentence or more (groundedLongRunes); otherwise two different ones are needed.
//
// The numbers come from the 30 days of research digests in the ledger and delegation log read on 2026-11-12: 4,968 digests,
// each paired with three pages of other sites and up to three pages of the same site (a different URL), and a grid of
// (strings, length) rules scored on the fingerprint failures and on those pairs. This one clears 125 of the 282 failures, and a
// digest paired with an unrelated page of another site was grounded in 7 of 14,904 pairs (0.05 %: shared footers, an llms.txt
// banner and one article's abstract under two addresses). Pages of the same site share navigation, price tables and licence text,
// and some are the same page under two addresses, so there 1,281 of 11,186 pairs (11 %) are grounded: the rule is no proof of
// the page against its own site, only of the page against the answer of a seat that never read it.

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/quotecheck"
)

const (
	// groundedMinRunes is the shortest string that counts: shorter ones (a figure, a label, a year) are found on other pages.
	groundedMinRunes = 15
	// groundedLongRunes is the length at which one found string is enough.
	groundedLongRunes = 60
	// groundedMinStrings is how many different strings of groundedMinRunes it takes when none is groundedLongRunes long.
	groundedMinStrings = 2
)

// DigestGrounded reports whether the structured digest holds enough strings that are on the contract's context documents,
// word for word apart from case, spacing, quote marks and dashes, and not in the goal: one of at least groundedLongRunes
// characters, or groundedMinStrings different ones of at least groundedMinRunes. A contract with no document, a digest that
// is not a JSON object, and a digest with no such string are not grounded.
func DigestGrounded(contract core.AgentContract, structured json.RawMessage) bool {
	if len(bytes.TrimSpace(structured)) == 0 || len(contract.Context) == 0 {
		return false
	}
	var root any
	if err := json.Unmarshal(structured, &root); err != nil {
		return false
	}
	if _, ok := root.(map[string]any); !ok {
		return false
	}
	var docs []string
	for _, d := range contract.Context {
		if strings.TrimSpace(d.Text) != "" {
			docs = append(docs, d.Text)
		}
	}
	if len(docs) == 0 {
		return false
	}
	docFolds := make([]*quotecheck.Folds, len(docs))
	for i, d := range docs {
		docFolds[i] = quotecheck.NewFolds(d)
	}
	goalFolds := quotecheck.NewFolds(contract.Goal)

	seen := map[string]bool{}
	found := 0
	var walk func(v any) bool
	walk = func(v any) bool {
		switch t := v.(type) {
		case string:
			s := strings.TrimSpace(t)
			n := utf8.RuneCountInString(s)
			if n < groundedMinRunes {
				return false
			}
			key := strings.ToLower(strings.Join(strings.Fields(s), " "))
			if seen[key] {
				return false
			}
			seen[key] = true
			if _, how := quotecheck.Locate(contract.Goal, goalFolds, s); how != quotecheck.Unverified {
				return false
			}
			for i, d := range docs {
				if _, how := quotecheck.Locate(d, docFolds[i], s); how != quotecheck.Unverified {
					found++
					return n >= groundedLongRunes || found >= groundedMinStrings
				}
			}
		case []any:
			for _, e := range t {
				if walk(e) {
					return true
				}
			}
		case map[string]any:
			for _, e := range t {
				if walk(e) {
					return true
				}
			}
		}
		return false
	}
	return walk(root)
}
