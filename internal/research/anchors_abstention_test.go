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
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
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

// builtSpec builds the research contract for one page the way offload_research does, so a test judges the check and the
// goal the seat is really handed together.
func builtSpec(t *testing.T, page, goal string) delegate.SubtaskSpec {
	t.Helper()
	specs, _ := Build(Request{Goal: goal}, []Fetched{{URL: "https://docs.example/guide", FinalURL: "https://docs.example/guide", Title: "Flash chip guide", Text: page}})
	if len(specs) != 1 {
		t.Fatalf("Build produced %d contracts, want one", len(specs))
	}
	return specs[0]
}

// fingerprintFailed reports whether the contract's docanchor check fails for the result, judged by the delegator's own
// acceptance evaluation (the only place a research result is graded).
func fingerprintFailed(spec delegate.SubtaskSpec, wire core.AgentWireResult) bool {
	for _, f := range delegate.EvalAcceptance(spec.AgentContract, wire) {
		if strings.Contains(f, "(?P<docanchor>") {
			return true
		}
	}
	return false
}

// digestWire is a finished default-schema digest: the four fields the default schema declares.
func digestWire(keyFacts, numbers, quotes []string, verdict string) core.AgentWireResult {
	list := func(in []string) []string {
		if in == nil {
			return []string{}
		}
		return in
	}
	structured, _ := json.Marshal(map[string]any{"key_facts": list(keyFacts), "numbers": list(numbers), "quotes": list(quotes), "verdict": verdict})
	return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Structured: structured, StopReason: "done"}
}

// A goal that dictates its own absence phrase (the nine corpus failures were such goals) and one that does not: the
// honest-absence rule must hold for both, because it reads what the digest SAYS, not what the goal said.
var absenceGoals = map[string]string{
	"a goal that dictates the phrase": "List the supported board revisions and the pin voltage for each. Say 'not on page' for anything absent.",
	"a plain goal":                    "List the supported board revisions and the pin voltage for each.",
}

func TestAnHonestAbsenceDigestPassesTheFingerprint(t *testing.T) {
	notOnPage := []string{"not on page", "not on page", "not on page", "not on page", "not on page"}
	withProse := digestWire(nil, nil, nil, "The page does not address it.")
	withProse.Output = "not on page"
	for gname, goal := range absenceGoals {
		spec := builtSpec(t, guidePage, goal)
		for name, wire := range map[string]core.AgentWireResult{
			"every list empty, the verdict says so":    digestWire(nil, nil, nil, "The page does not address it."),
			"the verdict is the whole digest":          digestWire(nil, nil, nil, "Not on the page."),
			"five not-on-page entries and a verdict":   digestWire(notOnPage, nil, nil, "The page does not contain the requested numbers."),
			"a verdict that names the document":        digestWire(nil, nil, nil, "The document does not mention any board revisions."),
			"a passive form":                           digestWire(nil, nil, nil, "The requested revisions are not documented in this text."),
			"a contraction":                            digestWire(nil, nil, nil, "The page doesn't address the pin voltage."),
			"the same with the loop's prose beside it": withProse,
		} {
			if fingerprintFailed(spec, wire) {
				t.Errorf("%s / %s: an honest absence was failed as off-document", gname, name)
			}
		}
	}
}

// A stock absence phrase is not a pass for a digest that CLAIMS something: a seat that read the wrong document and
// wrote confident facts about it can still write "not specified" in one of its fields. The tripwire exists for that digest.
func TestAStockAbsencePhraseDoesNotRescueADigestThatClaimsSomething(t *testing.T) {
	spec := builtSpec(t, guidePage, "Say what the guide is about.")
	for name, wire := range map[string]core.AgentWireResult{
		"facts about another subject, one of them hedged": digestWire(
			[]string{"The latest stable Go release ships a rewritten scheduler.", "Garbage collector pauses are lower.", "The release date is not specified."}, nil, nil,
			"A release note about a programming language."),
		"a number and an absence verdict": digestWire(nil, []string{"1.24"}, nil, "The page does not address it."),
		"a quote and an absence verdict":  digestWire(nil, nil, []string{"ships a rewritten scheduler"}, "The page does not address it."),
		"an absence and then a claim, in the verdict itself": digestWire(nil, nil, nil,
			"The page does not mention the date. It describes the new scheduler and the lower garbage collector pauses of the latest Go release."),
		"an absence with a claim brought in beside it": digestWire(nil, nil, nil,
			"The page does not mention the date, but it describes the new scheduler of the latest Go release."),
		"an absence in one field and a claim in a list": digestWire([]string{"The page does not mention the date.", "It describes the new scheduler of the latest Go release."}, nil, nil,
			"The page does not address it."),
	} {
		if !fingerprintFailed(spec, wire) {
			t.Errorf("%s: passed the fingerprint", name)
		}
	}
}

// A seat that could not read the file wrote an error, not a finding about the page, however it words the rest.
func TestAReadFailureIsNeverAnHonestAbsence(t *testing.T) {
	spec := builtSpec(t, guidePage, "Say what the guide is about.")
	for name, verdict := range map[string]string{
		"could not read":     "I could not read the file, so the page does not address the goal.",
		"cannot open":        "The page does not contain anything I can use: I cannot open the context file.",
		"missing file":       "The file is missing; the page does not mention the requested numbers.",
		"an error":           "An error occurred while opening the context file, and the page does not address it.",
		"does not exist":     "The document does not exist in the read root, so it does not contain the numbers.",
		"not found, as ever": "The requested file was not found; the page does not address it.",
	} {
		if !fingerprintFailed(spec, digestWire(nil, nil, nil, verdict)) {
			t.Errorf("%s: %q passed the fingerprint", name, verdict)
		}
	}
}

// The check is built from the page's words alone, so no sentence the goal contains can satisfy it: the goal echoed
// back, as the loop's prose or as the verdict of a digest, fails, and the contract is not flagged parrot-passable.
func TestABuiltResearchContractIsNotParrotPassable(t *testing.T) {
	for gname, goal := range absenceGoals {
		spec := builtSpec(t, guidePage, goal)
		for _, w := range delegate.LintAcceptance(spec.AgentContract) {
			if strings.Contains(w, "PARROT-PASSABLE") {
				t.Errorf("%s: the contract is flagged parrot-passable: %s", gname, w)
			}
		}
		for _, line := range spec.Acceptance {
			if !strings.Contains(line, "(?P<docanchor>") {
				continue
			}
			chk, err := core.ParseAcceptanceCheck(line)
			if err != nil {
				t.Fatalf("the fingerprint does not parse: %v", err)
			}
			if re := chk.Pattern(); re == nil || re.MatchString(spec.Goal) {
				t.Errorf("%s: the fingerprint matches the goal the seat is handed (pattern %v)", gname, re)
			}
		}
		if !fingerprintFailed(spec, core.AgentWireResult{Output: spec.Goal}) {
			t.Errorf("%s: the goal echoed back as the answer passed the fingerprint", gname)
		}
		if !fingerprintFailed(spec, digestWire(nil, nil, nil, spec.Goal)) {
			t.Errorf("%s: the goal echoed back as the verdict of an empty digest passed the fingerprint", gname)
		}
		if !fingerprintFailed(spec, digestWire([]string{spec.Goal}, nil, nil, "The page does not address it.")) {
			t.Errorf("%s: the goal echoed back as a key fact passed the fingerprint", gname)
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
