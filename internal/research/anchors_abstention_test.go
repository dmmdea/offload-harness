package research

// The research acceptance check is a WRONG-DOCUMENT tripwire: one any-of alternation of the page's own prose words, kept
// off the goal's vocabulary so an echoed question cannot pass. Two shapes of FAITHFUL answer failed it, and a failed
// tripwire is not a harmless extra row: strikeOnFingerprint counts it against the node, and the second strike in 30
// minutes quarantines the node for the session (one node was out for 30 minutes after two such rows while the rest of the
// fleet stood at "queue full").
//
//   - an honest absence ("not on page", "the page does not address it"): the research goal tells the seat to say exactly
//     that, written in the goal's own vocabulary, which the anchor excludes by construction;
//   - an inflection: the anchor was the plural ("specifications", "programmers") and the digest wrote the singular.
//
// 9 of 254 research answers in the corpus failed it, every one a faithful or honest-absence digest and none off-document.

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

const guidePage = `Reading the flash chip with an inexpensive clip tool is the usual first step for a repair.
The guide explains how to attach the clip to the eight pin package without removing the chip from the board.
Early programmers wrote their specifications by hand and every specifications sheet listed the supply voltage.
Always confirm the supply voltage before attaching the tool, because the cheapest adapters drive five volts.
A backup of the original firmware image should be saved and compared against a second read of the same chip.`

func passesAnchor(t *testing.T, page, goal, output string) bool {
	t.Helper()
	line := AnchorCheck(page, goal)
	if line == "" {
		t.Fatalf("no anchor check for the page")
	}
	chk, err := core.ParseAcceptanceCheck(line)
	if err != nil {
		t.Fatalf("the anchor check does not parse: %v\n%s", err, line)
	}
	pass, _ := chk.Eval(core.AgentWireResult{Output: output})
	return pass
}

func TestAnHonestAbsenceDigestPassesTheFingerprint(t *testing.T) {
	const goal = "List the supported board revisions and the pin voltage for each. Say 'not on page' for anything absent."
	for _, out := range []string{
		"not on page",
		"Not on the page.",
		"The page does not address it.",
		"The document does not mention any board revisions.",
		"Nothing here: the page does not contain the requested numbers, so every field is empty.",
		"The requested revisions are not documented in this text.",
	} {
		if !passesAnchor(t, guidePage, goal, out) {
			t.Errorf("an honest absence was failed as off-document: %q", out)
		}
	}
}

func TestAnInflectedRestatementPassesTheFingerprint(t *testing.T) {
	// The page's anchors include the plurals; the digest writes the singulars and nothing else the page says.
	const goal = "Say what the guide is about."
	for _, out := range []string{
		"It is a how-to for a clip programmer.",
		"A single specification sheet is the basis of it.",
	} {
		if !passesAnchor(t, guidePage, goal, out) {
			t.Errorf("a faithful restatement was failed for its inflection: %q", out)
		}
	}
}

func TestAnOffDocumentAnswerStillFailsTheFingerprint(t *testing.T) {
	const goal = "Say what the guide is about."
	for name, out := range map[string]string{
		"a confident digest of another page": "The latest stable Go release ships a rewritten scheduler and a new garbage collector with lower pauses.",
		"the seat could not read the file":   "I could not read the file: file not found. The context document is missing.",
		"an empty answer":                    "",
		"an echo of the goal":                "Say what the guide is about.",
	} {
		if passesAnchor(t, guidePage, goal, out) {
			t.Errorf("%s passed the fingerprint: %q", name, out)
		}
	}
}

// An echoed goal cannot pass through a stem either: the anchor is a word of the page, and so is its singular, but a goal
// that already uses the singular must not make the plural a way to restate the goal.
func TestAGoalWordIsNeverAnAnchorEvenThroughItsSingular(t *testing.T) {
	goal := "Explain the programmer and the specification in plain words."
	line := AnchorCheck(guidePage, goal)
	for _, bad := range []string{"programmer", "specification"} {
		if strings.Contains(line, bad) {
			t.Errorf("the goal's own word %q is in the check: %s", bad, line)
		}
	}
	if passesAnchor(t, guidePage, goal, goal) {
		t.Error("the goal echoed back passed the fingerprint")
	}
}

func TestTheFingerprintStillCarriesTheDocanchorTagAndOneAlternation(t *testing.T) {
	line := AnchorCheck(guidePage, "Say what the guide is about.")
	if !strings.HasPrefix(line, "regex:(?i)(?P<docanchor>") || strings.Count(line, "(?P<docanchor>") != 1 {
		t.Fatalf("the check must stay one regex tagged docanchor (the delegator keys quarantine and the retry on it): %s", line)
	}
}
