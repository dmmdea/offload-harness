package reviewlane

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The mechanism IS the product: a reviewer that never saw the work catches what the
// author's own judgement has stopped seeing. So the prompt must carry the task and the
// diff — and must not invite anything else.
func TestBuildPromptCarriesTaskAndDiffButNoSessionContext(t *testing.T) {
	p := buildPrompt("make the parser reject empty input", "--- a/x.go\n+++ b/x.go\n+if s == \"\" { return nil }\n")
	if !strings.Contains(p, "make the parser reject empty input") {
		t.Fatal("prompt must carry the task statement so the reviewer knows intent")
	}
	if !strings.Contains(p, "+if s ==") {
		t.Fatal("prompt must carry the diff body")
	}
	if strings.Contains(p, "session") || strings.Contains(p, "conversation") {
		t.Fatal("prompt must NOT invite prior session context - clean context is the mechanism")
	}
}

// Regression guard for the one defect only a live run could find (see promptFormatTail): an
// abstract placeholder template made the seat write back the placeholder NAME instead of the
// path. The prompt must carry a filled-in example, and must not present a bare metavariable
// line the seat can copy verbatim.
func TestPromptCarriesAFilledInExampleRatherThanABareTemplate(t *testing.T) {
	p := buildPrompt("make it work", "--- a/x.go\n+++ b/x.go\n+ok\n")
	if !strings.Contains(p, "internal/store/load.go:57") {
		t.Fatal("the format spec must show a filled-in example line — an abstract template gets copied verbatim")
	}
	if strings.Contains(p, "severity | file:line") {
		t.Fatal("the bare metavariable template is exactly what the seat echoed back as content")
	}
	if !strings.Contains(p, "<severity> | <path>:<line> | <claim> | <why>") {
		t.Fatal("the field spec must be unmistakably placeholders, not words a seat can read as literals")
	}
}

// At MaxDiffBytes the first statement of the format sits a quarter of a megabyte from the
// point where it has to be applied — and attention decay over a long window is this lane's
// own founding thesis, so burying the one instruction that has already failed live at the far
// end of the context would be incoherent.
func TestPromptRepeatsTheFormatSpecAfterTheDiff(t *testing.T) {
	p := buildPrompt("make it work", "--- a/x.go\n+++ b/x.go\n+MARKER_IN_THE_DIFF\n")
	if n := strings.Count(p, fieldSpec); n < 2 {
		t.Fatalf("the field spec must be stated on BOTH sides of the diff; found %d occurrence(s)", n)
	}
	after := p[strings.Index(p, "MARKER_IN_THE_DIFF"):]
	if !strings.Contains(after, fieldSpec) {
		t.Fatal("the repeat must come AFTER the diff body, where it has to be applied")
	}
	if !strings.Contains(after, "NONE") {
		t.Fatal("the NONE instruction must be restated after the diff too")
	}
}

func TestRankFindingsPutsSevereFirstAndCaps(t *testing.T) {
	in := []Finding{
		{Severity: "minor", Claim: "naming"},
		{Severity: "severe", Claim: "off-by-one"},
		{Severity: "moderate", Claim: "missing nil check"},
	}
	got := rankFindings(in, 2)
	if len(got) != 2 {
		t.Fatalf("cap not applied: got %d", len(got))
	}
	if got[0].Severity != "severe" || got[1].Severity != "moderate" {
		t.Fatalf("bad order: %+v", got)
	}
}

// An unrecognised severity must sort LAST rather than accidentally outranking `severe`
// (a map miss returns 0, which is severe's own rank). A small local seat inventing
// "critical" or "info" is the ordinary case, not the exotic one.
func TestRankFindingsSortsUnknownSeverityLastAndKeepsInputOrderWithin(t *testing.T) {
	in := []Finding{
		{Severity: "critical", Claim: "invented severity A"},
		{Severity: "minor", Claim: "naming"},
		{Severity: "", Claim: "unparsed line"},
		{Severity: "severe", Claim: "off-by-one"},
	}
	got := rankFindings(in, 0) // 0 = no cap
	if len(got) != 4 {
		t.Fatalf("a zero cap must not truncate: got %d", len(got))
	}
	if got[0].Severity != "severe" || got[1].Severity != "minor" {
		t.Fatalf("known severities must lead: %+v", got)
	}
	if got[2].Claim != "invented severity A" || got[3].Claim != "unparsed line" {
		t.Fatalf("unknown severities must trail in input order: %+v", got)
	}
}

// The contract must be dispatchable as-is and carry NOTHING but the task and the diff:
// no context docs, because there is no session history to ship.
func TestBuildContractIsValidAndShipsNoContextDocs(t *testing.T) {
	c, err := BuildContract("stop the retry loop from spinning forever", "--- a/run.go\n+++ b/run.go\n+for attempts < maxAttempts {\n")
	if err != nil {
		t.Fatalf("BuildContract: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the built contract must already be valid: %v", err)
	}
	if c.SchemaVersion != core.AgentWireSchemaVersion {
		t.Fatalf("schema_version = %d, want %d (PrepareContract mints it)", c.SchemaVersion, core.AgentWireSchemaVersion)
	}
	if len(c.Context) != 0 {
		t.Fatalf("the review lane ships no context docs; got %d", len(c.Context))
	}
	if len(c.OutputSchema) == 0 {
		t.Fatal("an output schema is required — the findings arrive through the structured re-pack")
	}
	if !strings.Contains(c.Goal, "for attempts < maxAttempts") {
		t.Fatal("the diff must ride in the goal, where the seat cannot fail to read it")
	}
	if c.Profile != "" {
		t.Fatalf("profile must stay empty so the executing seat's own agent_profile wins; got %q", c.Profile)
	}
}

func TestBuildContractRefusesEmptyTaskOrDiff(t *testing.T) {
	if _, err := BuildContract("  ", "--- a/x\n+++ b/x\n+ok\n"); !errors.Is(err, ErrNoTask) {
		t.Fatalf("empty task must refuse with ErrNoTask, got %v", err)
	}
	if _, err := BuildContract("do the thing", "   \n"); !errors.Is(err, ErrNoDiff) {
		t.Fatalf("empty diff must refuse with ErrNoDiff, got %v", err)
	}
}

// The diff rides in the GOAL, so core.AgentContract.Validate's context cap never sees it.
// This lane therefore owns the bound itself, and must refuse with the real numbers rather
// than shipping an unbounded prompt at a seat whose window cannot hold it.
func TestBuildContractRefusesAnOversizeDiffWithTheNumbers(t *testing.T) {
	big := strings.Repeat("+x\n", (MaxDiffBytes/3)+16)
	_, err := BuildContract("review it", big)
	if !errors.Is(err, ErrDiffTooLarge) {
		t.Fatalf("an oversize diff must refuse with ErrDiffTooLarge, got %v", err)
	}
	if !strings.Contains(err.Error(), "262144") {
		t.Fatalf("the refusal must name the ceiling so the caller can act on it: %v", err)
	}
}

func TestParseFindingsToleratesHowASeatActuallyWrites(t *testing.T) {
	got := ParseFindings([]string{
		"severe | internal/run.go:42 | off-by-one in the loop bound | reads one past the end",
		"- moderate | cfg.go:7 | missing nil check",
		"  MINOR | notes.md | trailing whitespace | cosmetic",
		"NONE",
		"   ",
		"the diff looks fine to me",
	})
	if len(got) != 4 {
		t.Fatalf("want 4 findings (NONE and the blank line dropped): %+v", got)
	}
	if got[0].Severity != "severe" || got[0].File != "internal/run.go" || got[0].Line != 42 {
		t.Fatalf("full line parsed wrong: %+v", got[0])
	}
	if got[0].Why != "reads one past the end" {
		t.Fatalf("why parsed wrong: %+v", got[0])
	}
	if got[1].Severity != "moderate" || got[1].File != "cfg.go" || got[1].Line != 7 || got[1].Why != "" {
		t.Fatalf("list marker + missing why parsed wrong: %+v", got[1])
	}
	if got[2].Severity != "minor" || got[2].File != "notes.md" || got[2].Line != 0 {
		t.Fatalf("case + file without a line parsed wrong: %+v", got[2])
	}
	// A line the seat wrote in its own shape is KEPT by the parser as an unranked claim, never
	// silently discarded: the parser must not turn a reviewer that did work into an empty
	// list. Whether a bare claim SURVIVES is Report's call (DropHollow), where the drop is
	// counted and an emptied list defers instead of reading as a clean review.
	if got[3].Claim != "the diff looks fine to me" || got[3].Severity != "" {
		t.Fatalf("an unformatted line must survive as an unranked claim: %+v", got[3])
	}
}

// A HOLLOW finding is a bare claim: no known severity, no file and no why. Any one of the three
// is structure, and a line carrying it survives however badly the rest was formatted — that is
// what is left of ParseFindings' "nothing is dropped for being badly formatted" rule.
func TestDropHollowKeepsAnyStructureAndDropsTheBareClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		f      Finding
		hollow bool
	}{
		{"claim only", Finding{Claim: "adds a null check before the call"}, true},
		{"an unrecognised label alone is not structure", Finding{Severity: "critical", Claim: "adds a null check before the call"}, true},
		{"known severity only", Finding{Severity: "minor", Claim: "adds a null check before the call"}, false},
		{"file only", Finding{File: "run.go", Claim: "adds a null check before the call"}, false},
		{"file and line only", Finding{File: "run.go", Line: 7, Claim: "adds a null check before the call"}, false},
		{"why only", Finding{Claim: "adds a null check before the call", Why: "a nil input no longer panics"}, false},
		{"all four fields", Finding{Severity: "severe", File: "run.go", Line: 7, Claim: "off-by-one", Why: "reads past the end"}, false},
		{"known severity with an empty claim is still a severity", Finding{Severity: "severe"}, false},
	} {
		kept, dropped := DropHollow([]Finding{tc.f})
		if tc.hollow && (dropped != 1 || len(kept) != 0) {
			t.Errorf("%s: want it dropped and counted, got kept=%+v dropped=%d", tc.name, kept, dropped)
		}
		if !tc.hollow && (dropped != 0 || len(kept) != 1) {
			t.Errorf("%s: carries structure and must survive, got kept=%+v dropped=%d", tc.name, kept, dropped)
		}
	}
}

// The same rule through the parser, on the shapes a seat actually writes: bare prose is hollow,
// and each single-field line below parses to exactly one structured field.
func TestDropHollowOnWhatParseFindingsMakesOfRealLines(t *testing.T) {
	kept, dropped := DropHollow(ParseFindings([]string{
		"the new guard restates what the diff already shows", // bare prose
		"Added a null check before the dereference",          // bare prose
		"critical | run.go",                // unknown label, the path lands in Claim
		"severe | the loop bound is wrong", // severity only
		"run.go:5 | off by one",            // file only
		"the loop | reads past the end",    // why only
	}))
	if dropped != 3 {
		t.Fatalf("the three bare claims must be dropped and counted, got %d: kept=%+v", dropped, kept)
	}
	if len(kept) != 3 || kept[0].Severity != "severe" || kept[1].File != "run.go" || kept[2].Why != "reads past the end" {
		t.Fatalf("the three structured lines must survive in the seat's order: %+v", kept)
	}
}

// The live shape (2026-10-09, a 228-line diff on a small seat): every line a bare claim that
// merely restated the diff. Nothing survives, all four are counted, and nothing else is.
func TestReportDropsAnAllHollowReviewAndCountsEveryLine(t *testing.T) {
	diff := "--- a/alarm.go\n+++ b/alarm.go\n@@ -1 +1 @@\n+x\n"
	rep := Report([]string{
		"The alarm handler now checks for a nil alarm before dispatching",
		"A new retry counter is incremented on every failed dispatch",
		"The dispatch loop was moved into its own function",
		"Logging was added around the alarm acknowledgement call",
	}, diff, 0)
	if len(rep.Findings) != 0 {
		t.Fatalf("a hollow review must publish no findings: %+v", rep.Findings)
	}
	if rep.DroppedHollow != 4 {
		t.Fatalf("all four lines must be counted as hollow, got %d", rep.DroppedHollow)
	}
	if rep.DroppedUngrounded+rep.DroppedEcho+rep.DroppedDuplicate+rep.TruncatedByCap != 0 {
		t.Fatalf("no other count may move: %+v", rep)
	}
}

// Hollow lines beside real ones are dropped one by one, and the three filters that can empty a
// list each keep their own count — a hollow line is never booked as an ungrounded or echoed one.
func TestReportCountsHollowBesideSurvivorsEchoesAndInventedFiles(t *testing.T) {
	diff := "--- a/run.go\n+++ b/run.go\n@@ -1 +1 @@\n+x\n"
	rep := Report([]string{
		"severe | run.go:5 | off-by-one | reads past the end",
		"adds a bounds check",
		"severe | ghost.go:1 | invented | not in the diff",
		fieldSpec,
	}, diff, 0)
	if len(rep.Findings) != 1 || rep.Findings[0].Claim != "off-by-one" {
		t.Fatalf("the one structured, grounded finding must survive: %+v", rep.Findings)
	}
	if rep.DroppedHollow != 1 || rep.DroppedUngrounded != 1 || rep.DroppedEcho != 1 {
		t.Fatalf("each filter must count its own line: %+v", rep)
	}
}

func TestFilesInDiffReadsBothHeaderStyles(t *testing.T) {
	files := FilesInDiff("diff --git a/internal/run.go b/internal/run.go\n" +
		"--- a/internal/run.go\n+++ b/internal/run.go\n@@ -1 +1 @@\n" +
		"--- /dev/null\n+++ b/pkg/new_file.go\n")
	if !files["run.go"] || !files["new_file.go"] {
		t.Fatalf("both changed files must be recognised: %v", files)
	}
	if files["null"] {
		t.Fatalf("/dev/null is not a file under review: %v", files)
	}
}

// A finding about a file the diff does not touch cannot be triaged from the diff, and a
// small seat inventing one is the ordinary failure mode. It is dropped and COUNTED —
// never dropped silently.
func TestGroundDropsFindingsNamingAFileTheDiffNeverTouched(t *testing.T) {
	files := map[string]bool{"run.go": true}
	kept, dropped := Ground([]Finding{
		{Severity: "severe", File: "internal/run.go", Claim: "real"},
		{Severity: "severe", File: "somewhere/else.go", Claim: "invented"},
		{Severity: "minor", File: "", Claim: "no file named"},
	}, files)
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if len(kept) != 2 || kept[0].Claim != "real" || kept[1].Claim != "no file named" {
		t.Fatalf("a finding naming no file must survive: %+v", kept)
	}
}

// Fail OPEN: if nothing about the diff parsed into a file set, grounding has no basis and
// must not delete the whole review.
func TestGroundKeepsEverythingWhenTheDiffNamedNoFiles(t *testing.T) {
	kept, dropped := Ground([]Finding{{File: "x.go", Claim: "a"}}, nil)
	if dropped != 0 || len(kept) != 1 {
		t.Fatalf("no file set means no grounding basis: kept=%+v dropped=%d", kept, dropped)
	}
}

func TestReportGroundsRanksAndCaps(t *testing.T) {
	diff := "--- a/run.go\n+++ b/run.go\n@@ -1 +1 @@\n+x\n"
	rep := Report([]string{
		"minor | run.go:3 | naming | cosmetic",
		"severe | ghost.go:1 | invented file | not in the diff",
		"severe | run.go:42 | off-by-one | reads past the end",
		"moderate | run.go:9 | missing nil check | panics on empty input",
	}, diff, 2)
	if rep.DroppedUngrounded != 1 {
		t.Fatalf("the ungrounded finding must be counted: %d", rep.DroppedUngrounded)
	}
	if len(rep.Findings) != 2 {
		t.Fatalf("cap not applied: %+v", rep.Findings)
	}
	if rep.TruncatedByCap != 1 {
		t.Fatalf("what the cap hid must be counted too, not silently dropped: %d", rep.TruncatedByCap)
	}
	if rep.Findings[0].Severity != "severe" || rep.Findings[0].Claim != "off-by-one" {
		t.Fatalf("severe must lead: %+v", rep.Findings)
	}
	if rep.Findings[1].Severity != "moderate" {
		t.Fatalf("moderate must follow severe: %+v", rep.Findings)
	}
}

// The ceiling has to bind THROUGH Report, not just inside capFindings. Asserting on
// capFindings alone left the whole suite green with the clamp deleted from Report entirely —
// a test that cannot fail is decorative. Mutation-verified: removing capFindings from Report
// makes this test RED.
func TestReportAppliesTheCeilingEvenWhenTheCallerAsksForMore(t *testing.T) {
	diff := "--- a/run.go\n+++ b/run.go\n@@ -1 +1 @@\n+x\n"
	lines := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		lines = append(lines, fmt.Sprintf("minor | run.go:%d | finding %d | why %d", i+1, i, i))
	}
	rep := Report(lines, diff, 999)
	if len(rep.Findings) != DefaultMaxFindings {
		t.Fatalf("a cap above the ceiling must clamp to DefaultMaxFindings (%d); got %d", DefaultMaxFindings, len(rep.Findings))
	}
	if rep.TruncatedByCap != 12-DefaultMaxFindings {
		t.Fatalf("truncation must be counted: got %d, want %d", rep.TruncatedByCap, 12-DefaultMaxFindings)
	}
	if rep = Report(lines, diff, 3); len(rep.Findings) != 3 || rep.TruncatedByCap != 9 {
		t.Fatalf("a caller narrowing the list must be honoured and counted: %d findings, %d truncated", len(rep.Findings), rep.TruncatedByCap)
	}
}

// An unrecognised severity label used to SHRED the line: the label became the claim and the
// real claim, path and why were rejoined into Why. File came out empty, so Ground skipped it
// (it only judges findings that name a file) and the wreckage reached the caller UNCOUNTED,
// looking like an ordinary finding. Small seats drift to critical/high/blocker/P0 routinely.
func TestParseFindingsKeepsAnUnrecognisedSeverityInsteadOfShreddingTheLine(t *testing.T) {
	got := ParseFindings([]string{"critical | run.go:5 | off-by-one | indexes past the end"})
	if len(got) != 1 {
		t.Fatalf("want one finding: %+v", got)
	}
	f := got[0]
	if f.File != "run.go" || f.Line != 5 {
		t.Fatalf("the path must still be parsed, or Ground can never judge it: %+v", f)
	}
	if f.Claim != "off-by-one" || f.Why != "indexes past the end" {
		t.Fatalf("claim and why must survive an unknown label: %+v", f)
	}
	if f.Severity != "critical" {
		t.Fatalf("the label the seat actually used must be kept, not discarded: %+v", f)
	}
	if ranked := rankFindings([]Finding{f, {Severity: "minor", Claim: "m"}}, 0); ranked[0].Severity != "minor" {
		t.Fatalf("an unrecognised label must not outrank a real severity: %+v", ranked)
	}
}

// The consequence of the fix above, and the reason it matters: with the path parsed, an
// invented one is now visible to Ground and COUNTED instead of escaping.
func TestAnUnrecognisedSeverityFindingStillFacesGrounding(t *testing.T) {
	rep := Report([]string{"critical | ghost.go:1 | invented | not in the diff"},
		"--- a/run.go\n+++ b/run.go\n@@ -1 +1 @@\n+x\n", 0)
	if rep.DroppedUngrounded != 1 {
		t.Fatalf("an invented path must be counted whatever severity label rode with it: %+v", rep)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("and it must not reach the caller: %+v", rep.Findings)
	}
}

// The worked example is parseable and grounds against any diff touching a file with that base
// name, so an echo of it would arrive as an ordinary finding. Echoing it is MEASURED
// behaviour of this seat, so the guard is a machine check rather than a human's vigilance.
func TestReportDropsAndCountsTemplateEchoes(t *testing.T) {
	diff := "--- a/load.go\n+++ b/load.go\n@@ -1 +1 @@\n+x\n" // grounds the example's own path
	rep := Report([]string{
		exampleFinding,               // the worked example, verbatim
		"- " + exampleFinding + "  ", // ...wearing the decoration a seat adds
		fieldSpec,                    // the placeholder line itself
		"severe | load.go:3 | real finding | genuine",
	}, diff, 0)
	if rep.DroppedEcho != 3 {
		t.Fatalf("every echo of the prompt's own template must be dropped and counted: %+v", rep)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Claim != "real finding" {
		t.Fatalf("the genuine finding must survive: %+v", rep.Findings)
	}
	// Byte-equality, never resemblance: a finding that merely looks like the example is a
	// finding, and dropping it would be the quality judgement this lane does not make.
	if near := Report([]string{exampleFinding + " and also this"}, diff, 0); near.DroppedEcho != 0 {
		t.Fatalf("only a byte-identical echo is dropped: %+v", near)
	}
}

// The gate that separates a clean review from a broken run. Both arrive as a schema-valid
// empty findings array with stop_reason "done"; the seat's raw answer is the only field that
// differs, which is why it is read.
func TestVerdictReadsCleanSeparatesASilentRunFromACleanOne(t *testing.T) {
	for _, notClean := range []string{
		"", "   \n  ", "ok", ".",
		// Both of these were flagged by the LANE ITSELF reviewing this diff, and both
		// passed the first version of the gate. The first defeated a bare \bnone\b
		// token match; the second defeated a "long enough to be an answer" fallback —
		// it is 25 characters and is a seat reporting FAILURE, the exact case the gate
		// exists to catch.
		"I tried but none of the tools worked",
		"I could not read the diff",
		"The seat was unable to complete the review of this rather large diff.",
	} {
		if VerdictReadsClean(notClean) {
			t.Errorf("%q must not read as a clean verdict — it is not an affirmative all-clear", notClean)
		}
	}
	for _, clean := range []string{"NONE", "none", "None.", "I found no defects in this diff.", "no issues found", "reviewed all hunks:\nNONE\n"} {
		if !VerdictReadsClean(clean) {
			t.Errorf("%q must read as a clean verdict", clean)
		}
	}
}

// The core fixture for register D-90: 3 EXACT duplicates (same file, same line, same claim
// text) plus 2 NEAR-duplicates (line ±1 of the group, punctuation/casing differs) all name the
// SAME defect and must collapse to one survivor; a 6th, textually distinct finding must be
// untouched. 5 in, 1 out per group: 2 kept overall, 4 dropped_duplicate.
func TestDedupeCollapsesExactAndNearDuplicates(t *testing.T) {
	in := []Finding{
		{Severity: "moderate", File: "run.go", Line: 57, Claim: "the returned error is discarded"},
		{Severity: "moderate", File: "run.go", Line: 57, Claim: "the returned error is discarded"},
		{Severity: "moderate", File: "run.go", Line: 57, Claim: "the returned error is discarded"},
		{Severity: "moderate", File: "run.go", Line: 56, Claim: "The returned error is discarded."},
		{Severity: "moderate", File: "run.go", Line: 58, Claim: "the returned error is discarded!!"},
		{Severity: "severe", File: "run.go", Line: 12, Claim: "off-by-one in the loop bound"},
	}
	out, dropped := Dedupe(in)
	if dropped != 4 {
		t.Fatalf("dropped = %d, want 4", dropped)
	}
	if len(out) != 2 {
		t.Fatalf("kept = %d, want 2: %+v", len(out), out)
	}
	claims := map[string]bool{}
	for _, f := range out {
		claims[f.Claim] = true
	}
	if !claims["off-by-one in the loop bound"] {
		t.Fatalf("the distinct finding must survive untouched: %+v", out)
	}
}

// Duplicates must merge across ANY tolerated pair in the chain even when the two ends are more
// than 2 lines apart — 57, then 56 (gap 1), then 58 (gap 2 from 56) all chain into one cluster,
// even though 58-57=1 and 58-56=2 are both within tolerance too; this asserts the CHAINING
// itself, not just single-pair tolerance.
func TestDedupeChainsLineTolerance(t *testing.T) {
	in := []Finding{
		{Severity: "minor", File: "x.go", Line: 10, Claim: "missing nil check"},
		{Severity: "minor", File: "x.go", Line: 12, Claim: "missing nil check"},
	}
	out, dropped := Dedupe(in)
	if dropped != 1 || len(out) != 1 {
		t.Fatalf("a ±2 line gap must still merge: kept=%+v dropped=%d", out, dropped)
	}
	// A gap of 3 must NOT merge — this is the boundary the tolerance is supposed to enforce.
	in2 := []Finding{
		{Severity: "minor", File: "x.go", Line: 10, Claim: "missing nil check"},
		{Severity: "minor", File: "x.go", Line: 13, Claim: "missing nil check"},
	}
	out2, dropped2 := Dedupe(in2)
	if dropped2 != 0 || len(out2) != 2 {
		t.Fatalf("a >2 line gap must NOT merge: kept=%+v dropped=%d", out2, dropped2)
	}
}

// Within a duplicate cluster the most severe report wins, regardless of input order or line
// order — a seat that first under-called a defect "minor" and later, re-describing it,
// correctly called it "severe" must not have its stronger read thrown away.
func TestDedupeKeepsTheMostSevereOfADuplicateCluster(t *testing.T) {
	in := []Finding{
		{Severity: "minor", File: "run.go", Line: 5, Claim: "off-by-one in the loop bound"},
		{Severity: "severe", File: "run.go", Line: 6, Claim: "off-by-one in the loop bound"},
	}
	out, dropped := Dedupe(in)
	if dropped != 1 || len(out) != 1 {
		t.Fatalf("kept=%+v dropped=%d", out, dropped)
	}
	if out[0].Severity != "severe" {
		t.Fatalf("the more severe report must win: %+v", out[0])
	}
}

// When severities tie, the FIRST occurrence in the seat's own answer wins — never whichever
// the sort happened to visit last, and never the one with the lower line number (the fixture
// deliberately puts the later-in-input finding on the earlier line, so a line-order tie-break
// would pick the wrong one).
func TestDedupeSeverityTieBreakKeepsFirstOccurrence(t *testing.T) {
	in := []Finding{
		{Severity: "moderate", File: "run.go", Line: 20, Claim: "missing nil check", Why: "first, reported second-in-line"},
		{Severity: "moderate", File: "run.go", Line: 19, Claim: "missing nil check", Why: "reported first, later line-sort position wins nothing"},
	}
	out, dropped := Dedupe(in)
	if dropped != 1 || len(out) != 1 {
		t.Fatalf("kept=%+v dropped=%d", out, dropped)
	}
	if out[0].Why != "first, reported second-in-line" {
		t.Fatalf("the first occurrence in input order must survive a severity tie: %+v", out[0])
	}
}

// A leading "path:line" the seat folded into Claim itself (ParseFindings's own two-field
// shape, "severe | run.go:5", leaves the whole "run.go:5" in Claim with File empty) must
// normalise away so it can still match its properly-parsed sibling on claim text.
func TestDedupeStripsARepeatedFileLinePrefixFromTheClaim(t *testing.T) {
	in := []Finding{
		{Severity: "moderate", File: "run.go", Line: 5, Claim: "off-by-one in the loop bound"},
		{Severity: "moderate", File: "run.go", Line: 5, Claim: "run.go:5: off-by-one in the loop bound"},
	}
	out, dropped := Dedupe(in)
	if dropped != 1 || len(out) != 1 {
		t.Fatalf("the repeated file:line prefix must not defeat the match: kept=%+v dropped=%d", out, dropped)
	}
}

// Different files, or a genuinely different claim, must never merge — Dedupe only ever
// narrows what the CAP later sees, never invents a match across unrelated findings.
func TestDedupeNeverMergesAcrossFilesOrDistinctClaims(t *testing.T) {
	in := []Finding{
		{Severity: "moderate", File: "a.go", Line: 5, Claim: "missing nil check"},
		{Severity: "moderate", File: "b.go", Line: 5, Claim: "missing nil check"},
		{Severity: "moderate", File: "a.go", Line: 5, Claim: "unrelated defect"},
	}
	out, dropped := Dedupe(in)
	if dropped != 0 || len(out) != 3 {
		t.Fatalf("nothing here is a duplicate: kept=%+v dropped=%d", out, dropped)
	}
}

// The ordering guard for register D-90: dedupe must run BEFORE the cap, or duplicate copies of
// ONE finding crowd a genuinely different finding out of the published list. Five restatements
// of the same defect plus one distinct finding, capped at 1: with dedupe-before-cap the
// distinct finding can still win the cap on its own severity; with dedupe AFTER the cap the
// five duplicates alone would already have filled the one slot the cap allows, and the
// distinct finding would never even reach ranking.
//
// Mutation-verified: swapping Report's `deduped, duplicate := Dedupe(kept); ranked :=
// rankFindings(deduped, capFindings(max))` for `ranked := rankFindings(kept, capFindings(max));
// deduped, duplicate := Dedupe(ranked)` (dedupe after the cap) makes this test FAIL — the cap
// runs first and keeps only 1 raw (pre-dedupe) finding, so Dedupe then sees a single-item
// slice and reports DroppedDuplicate=0 instead of 4 (FAIL: "DroppedDuplicate = 0, want 4"),
// and which single finding survived the cap depends on rankFindings' stable sort over 6
// still-undeduplicated entries rather than on the real severe/duplicate distinction.
func TestReportDedupesBeforeApplyingTheCap(t *testing.T) {
	diff := "--- a/run.go\n+++ b/run.go\n@@ -1 +1 @@\n+x\n"
	rep := Report([]string{
		"minor | run.go:10 | missing nil check | panics on empty input",
		"minor | run.go:10 | missing nil check | panics on empty input",
		"minor | run.go:11 | missing nil check | panics on empty input",
		"minor | run.go:9 | missing nil check | panics on empty input",
		"minor | run.go:12 | missing nil check | panics on empty input",
		"severe | run.go:40 | off-by-one in the loop bound | reads one past the end",
	}, diff, 1)
	if rep.DroppedDuplicate != 4 {
		t.Fatalf("DroppedDuplicate = %d, want 4", rep.DroppedDuplicate)
	}
	if len(rep.Findings) != 1 {
		t.Fatalf("cap of 1 must still publish exactly 1: %+v", rep.Findings)
	}
	if rep.Findings[0].Severity != "severe" {
		t.Fatalf("the cap must be applied AFTER dedupe, so the distinct severe finding wins it: %+v", rep.Findings[0])
	}
	// 6 in, 4 duplicates dropped, 2 survive dedupe, cap keeps 1 of those 2.
	if rep.TruncatedByCap != 1 {
		t.Fatalf("TruncatedByCap must count against the POST-dedupe total, not the raw input: %d", rep.TruncatedByCap)
	}
}

func TestReportClampsTheCapToWhatTheSeatWasAskedFor(t *testing.T) {
	if got := capFindings(0); got != DefaultMaxFindings {
		t.Fatalf("an unset cap must fall back to the default: %d", got)
	}
	if got := capFindings(999); got != DefaultMaxFindings {
		t.Fatalf("a cap above what the seat is asked for must clamp: %d", got)
	}
	if got := capFindings(3); got != 3 {
		t.Fatalf("a caller narrowing the list must be honoured: %d", got)
	}
}

// The live shape (2026-10-09, a 48 KB diff): three findings stacked on ONE line of one file,
// each a rewording of the same issue. Dedupe keys on the claim text and could not see them. They
// merge into the most severe, every other claim stays readable in Also in the seat's order, and
// the two absorbed findings are counted as duplicates.
func TestMergeSameLineFoldsRestatementsAndKeepsEveryClaim(t *testing.T) {
	in := []Finding{
		{Severity: "moderate", File: "alarm.go", Line: 101, Claim: "the alarm id is never checked for nil", Why: "a nil id panics in dispatch"},
		{Severity: "severe", File: "alarm.go", Line: 101, Claim: "dispatch dereferences an unvalidated alarm", Why: "a malformed alarm crashes the service"},
		{Severity: "minor", File: "alarm.go", Line: 101, Claim: "no guard before the alarm lookup", Why: "the alarm is silently skipped"},
	}
	out, dropped := MergeSameLine(in)
	if dropped != 2 || len(out) != 1 {
		t.Fatalf("three findings on one line must become one with two counted: kept=%+v dropped=%d", out, dropped)
	}
	if out[0].Severity != "severe" || out[0].Claim != "dispatch dereferences an unvalidated alarm" {
		t.Fatalf("the most severe report must be the one kept: %+v", out[0])
	}
	if len(out[0].Also) != 2 || out[0].Also[0] != "the alarm id is never checked for nil" || out[0].Also[1] != "no guard before the alarm lookup" {
		t.Fatalf("the other two claims must ride in Also, in the seat's order: %+v", out[0].Also)
	}
	if len(in[0].Also) != 0 || len(in[1].Also) != 0 {
		t.Fatalf("the input must not be mutated: %+v", in)
	}
}

// The same shape end to end through Report, which is what the lane publishes.
func TestReportFoldsAStackedLineIntoOneFindingAndCountsIt(t *testing.T) {
	diff := "--- a/alarm.go\n+++ b/alarm.go\n@@ -1 +1 @@\n+x\n"
	rep := Report([]string{
		"moderate | alarm.go:101 | the alarm id is never checked for nil | a nil id panics in dispatch",
		"severe | alarm.go:101 | dispatch dereferences an unvalidated alarm | a malformed alarm crashes the service",
		"minor | alarm.go:101 | no guard before the alarm lookup | the alarm is silently skipped",
	}, diff, 0)
	if len(rep.Findings) != 1 || len(rep.Findings[0].Also) != 2 {
		t.Fatalf("want one finding carrying two also-claims: %+v", rep.Findings)
	}
	if rep.DroppedDuplicate != 2 {
		t.Fatalf("the two folded findings must be counted as duplicates: %d", rep.DroppedDuplicate)
	}
	if rep.TruncatedByCap != 0 || rep.DroppedHollow != 0 || rep.DroppedUngrounded != 0 {
		t.Fatalf("no other count may move: %+v", rep)
	}
}

// Equal severity keeps the FIRST finding in the seat's order, whatever the other fields say.
func TestMergeSameLineSeverityTieKeepsTheFirstInTheSeatsOrder(t *testing.T) {
	out, dropped := MergeSameLine([]Finding{
		{Severity: "moderate", File: "run.go", Line: 9, Claim: "first", Why: "w1"},
		{Severity: "moderate", File: "run.go", Line: 9, Claim: "second", Why: "w2"},
		{Severity: "moderate", File: "run.go", Line: 9, Claim: "third", Why: "w3"},
	})
	if dropped != 2 || len(out) != 1 || out[0].Claim != "first" || len(out[0].Also) != 2 || out[0].Also[0] != "second" || out[0].Also[1] != "third" {
		t.Fatalf("a tie must keep the first and fold the rest in order: kept=%+v dropped=%d", out, dropped)
	}
	// An unrecognised label ranks below a real severity, as it does everywhere else.
	out, _ = MergeSameLine([]Finding{
		{Severity: "critical", File: "run.go", Line: 9, Claim: "invented label"},
		{Severity: "minor", File: "run.go", Line: 9, Claim: "real severity"},
	})
	if len(out) != 1 || out[0].Claim != "real severity" {
		t.Fatalf("an invented severity label must not outrank a real one: %+v", out)
	}
}

// Line 0 is "the seat did not say where". Two findings that both failed to name a place are not
// the same place, and neither is a finding that named a line but no file.
func TestMergeSameLineNeverMergesOnAMissingOrUnknownLocation(t *testing.T) {
	for name, in := range map[string][]Finding{
		"line 0 in one file":          {{File: "alarm.go", Claim: "a"}, {File: "alarm.go", Claim: "b"}},
		"line 0 beside a real line":   {{File: "alarm.go", Claim: "a"}, {File: "alarm.go", Line: 5, Claim: "b"}},
		"no file, same line":          {{Line: 5, Claim: "a"}, {Line: 5, Claim: "b"}},
		"different files, same line":  {{File: "alarm.go", Line: 5, Claim: "a"}, {File: "run.go", Line: 5, Claim: "b"}},
		"one file, different lines":   {{File: "alarm.go", Line: 5, Claim: "a"}, {File: "alarm.go", Line: 6, Claim: "b"}},
		"negative line is not a line": {{File: "alarm.go", Line: -1, Claim: "a"}, {File: "alarm.go", Line: -1, Claim: "b"}},
	} {
		out, dropped := MergeSameLine(in)
		if dropped != 0 || len(out) != len(in) {
			t.Errorf("%s: nothing here is the same place: kept=%+v dropped=%d", name, out, dropped)
		}
		for _, f := range out {
			if len(f.Also) != 0 {
				t.Errorf("%s: nothing may be folded: %+v", name, f)
			}
		}
	}
}

// The file is compared the way Ground and Dedupe compare it: a seat may root a path differently
// in the same answer, and all of these name one file.
func TestMergeSameLineComparesTheFileByBaseName(t *testing.T) {
	out, dropped := MergeSameLine([]Finding{
		{Severity: "minor", File: "internal/alarm.go", Line: 12, Claim: "one"},
		{Severity: "minor", File: "b/internal/Alarm.go", Line: 12, Claim: "two"},
		{Severity: "minor", File: `internal\alarm.go`, Line: 12, Claim: "three"},
		{Severity: "minor", File: "alarm.go", Line: 12, Claim: "four"},
	})
	if dropped != 3 || len(out) != 1 || len(out[0].Also) != 3 {
		t.Fatalf("four spellings of one file at one line are one place: kept=%+v dropped=%d", out, dropped)
	}
}

// Survivors keep the seat's order, and findings elsewhere are untouched.
func TestMergeSameLineKeepsInputOrderAndLeavesOtherFindingsAlone(t *testing.T) {
	out, dropped := MergeSameLine([]Finding{
		{Severity: "minor", File: "a.go", Line: 1, Claim: "stack member, folded away"},
		{Severity: "moderate", File: "b.go", Line: 7, Claim: "lone finding"},
		{Severity: "severe", File: "a.go", Line: 1, Claim: "stack keeper"},
		{Severity: "minor", File: "c.go", Line: 0, Claim: "no line"},
	})
	if dropped != 1 || len(out) != 3 {
		t.Fatalf("want 3 survivors, 1 folded: kept=%+v dropped=%d", out, dropped)
	}
	if out[0].Claim != "lone finding" || out[1].Claim != "stack keeper" || out[2].Claim != "no line" {
		t.Fatalf("survivors must keep the seat's relative order: %+v", out)
	}
	if len(out[1].Also) != 1 || out[1].Also[0] != "stack member, folded away" {
		t.Fatalf("the folded claim must ride on the keeper: %+v", out[1])
	}
	if len(out[0].Also) != 0 || len(out[2].Also) != 0 {
		t.Fatalf("findings that absorbed nothing carry no Also: %+v", out)
	}
}

// A folded finding with no claim text adds nothing to Also (a blank entry is noise), but it is
// still one fewer finding and is still counted.
func TestMergeSameLineSkipsBlankClaimsInAlsoButStillCountsThem(t *testing.T) {
	out, dropped := MergeSameLine([]Finding{
		{Severity: "severe", File: "run.go", Line: 3, Claim: "real claim"},
		{Severity: "minor", File: "run.go", Line: 3, Claim: "  "},
	})
	if dropped != 1 || len(out) != 1 || len(out[0].Also) != 0 {
		t.Fatalf("kept=%+v dropped=%d", out, dropped)
	}
}

// The ordering guard, same reasoning as register D-90: folding must happen BEFORE the cap, or a
// stack crowds a genuinely different finding out. Four severe restatements at one line plus one
// distinct moderate finding, capped at 2: folded first, the stack is ONE slot and the distinct
// finding keeps the other. Capped first, the two slots would both go to the stack's severe
// members and the distinct finding would never be published.
//
// Mutation-verified: swapping Report's `merged, folded := MergeSameLine(deduped)` /
// `rankFindings(merged, ...)` for a cap before the merge makes this test FAIL.
func TestReportFoldsBeforeApplyingTheCapSoAStackCannotCrowdOutADistinctFinding(t *testing.T) {
	diff := "--- a/run.go\n+++ b/run.go\n@@ -1 +1 @@\n+x\n"
	rep := Report([]string{
		"severe | run.go:7 | the index is not bounds-checked | reads past the end",
		"severe | run.go:7 | slice access without a length guard | out-of-range panic",
		"severe | run.go:7 | missing check on len(xs) | crash on empty input",
		"severe | run.go:7 | unchecked subscript | crash",
		"moderate | run.go:40 | the error from Close is dropped | a failed close is silent",
	}, diff, 2)
	if len(rep.Findings) != 2 || rep.TruncatedByCap != 0 {
		t.Fatalf("the stack is one slot, so a cap of 2 hides nothing: %+v (truncated %d)", rep.Findings, rep.TruncatedByCap)
	}
	if rep.DroppedDuplicate != 3 {
		t.Fatalf("three restatements folded: %d", rep.DroppedDuplicate)
	}
	if rep.Findings[0].Line != 7 || len(rep.Findings[0].Also) != 3 || rep.Findings[1].Line != 40 {
		t.Fatalf("the folded stack and the distinct finding must both be published: %+v", rep.Findings)
	}
}

// ...and the cap still binds AFTER the merge: 12 distinct places plus two restatements stacked
// on the first one is 12 findings, of which the ceiling shows 10 and counts the 2 it hid.
func TestReportCapStillAppliesAfterMerging(t *testing.T) {
	diff := "--- a/run.go\n+++ b/run.go\n@@ -1 +1 @@\n+x\n"
	lines := make([]string, 0, 14)
	for i := 1; i <= 12; i++ {
		lines = append(lines, fmt.Sprintf("minor | run.go:%d | distinct finding %d | consequence %d", i*10, i, i))
	}
	lines = append(lines,
		"minor | run.go:10 | the first place worded another way | same consequence",
		"minor | run.go:10 | the first place worded a third way | same consequence")
	rep := Report(lines, diff, 0)
	if len(rep.Findings) != DefaultMaxFindings || rep.TruncatedByCap != 2 || rep.DroppedDuplicate != 2 {
		t.Fatalf("12 survivors capped to %d: got %d findings, truncated %d, duplicates %d", DefaultMaxFindings, len(rep.Findings), rep.TruncatedByCap, rep.DroppedDuplicate)
	}
}

// Also is additive on the wire: absent unless something was folded, so a result with no stacked
// line is byte-identical to what it was before the field existed.
func TestFindingAlsoIsOmittedFromTheWireUnlessSomethingWasFolded(t *testing.T) {
	plain, err := json.Marshal(Finding{Severity: "minor", File: "run.go", Line: 1, Claim: "c", Why: "w"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "also") {
		t.Fatalf("a finding that absorbed nothing must not publish the field: %s", plain)
	}
	folded, err := json.Marshal(Finding{Severity: "minor", File: "run.go", Line: 1, Claim: "c", Why: "w", Also: []string{"x", "y"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(folded), `"also":["x","y"]`) {
		t.Fatalf("a folded finding must publish its other claims: %s", folded)
	}
}

// A review the seat WROTE, stopped only by the clock on the structuring step, is not lost work
// (F20: three of five review defers in 26 hours). Salvage keys on what the node says about the
// deferral - OutputTruncated, or SchemaMiss with the budget class - and on nothing else, so the
// table below is the whole contract: which deferrals become a review, and which stay a defer.
func TestSalvageReadsOnlyTheDefersWhereTheClockEndedTheStructuring(t *testing.T) {
	const l1, l2 = "severe | run.go:5 | off-by-one | reads past the end", "moderate | run.go:9 | missing nil check | panics"
	const cutReason = "output failed schema: re-pack skipped: the final answer was cut at the completion budget (output_truncated) - a partial cannot be re-packed into the requested object; the partial rides in output"
	const skipReason = "structured re-pack skipped: 0 s left to the wall + 30 s grace at 5.6 tok/s buys 0 tokens < the answer's 91"
	deferred := func(class, reason, output string, mut func(*core.AgentWireResult)) core.AgentWireResult {
		w := core.AgentWireResult{Deferred: true, DeferClass: class, Reason: reason, Output: output, StopReason: "done"}
		if mut != nil {
			mut(&w)
		}
		return w
	}
	cut := func(w *core.AgentWireResult) { w.OutputTruncated = true }
	miss := func(w *core.AgentWireResult) { w.SchemaMiss = true }
	for _, tc := range []struct {
		name  string
		w     core.AgentWireResult
		kind  string
		lines []string
	}{
		{"cut final: the fragment after the last newline is dropped",
			deferred(core.DeferClassAbstention, cutReason, l1+"\n"+l2+"\nminor | run.go:11 | the cut li", cut), SalvagedOutputTruncated, []string{l1, l2}},
		{"cut final that ended on a newline loses no real line",
			deferred(core.DeferClassAbstention, cutReason, l1+"\n"+l2+"\n", cut), SalvagedOutputTruncated, []string{l1, l2}},
		{"cut final with one fragment and no complete line",
			deferred(core.DeferClassAbstention, cutReason, "severe | run.go:5 | off", cut), SalvagedOutputTruncated, []string{}},
		{"re-pack skipped for lack of wall: the answer is whole, every line kept",
			deferred(core.DeferClassBudget, skipReason, l1+"\n"+l2, miss), SalvagedWall, []string{l1, l2}},
		{"re-pack clamped and cut by the time left is the same clock",
			deferred(core.DeferClassBudget, "structured re-pack re-pack truncated at 300 tokens (the time left set this budget)", l1+"\n"+l2, miss), SalvagedWall, []string{l1, l2}},

		{"gpu busy is not the structuring step",
			deferred(core.DeferClassCapacity, "gpu busy: a text job holds the GPU", l1, nil), "", nil},
		{"a seat or stack failure during the re-pack is not the clock",
			deferred(core.DeferClassInfrastructure, "structured re-pack unreachable: dial tcp: connection refused", l1, miss), "", nil},
		{"a re-pack that answered the wrong shape is an abstention, not the clock",
			deferred(core.DeferClassAbstention, "output failed schema: the model answered the wrong shape", l1, miss), "", nil},
		{"a re-pack the caller canceled has nobody waiting",
			deferred(core.DeferClassBudget, core.RepackCanceledReason+" (the caller's context ended)", l1, miss), "", nil},
		{"a budget defer that is not a finished answer (no schema_miss flag)",
			deferred(core.DeferClassBudget, "step budget exhausted (12 steps)", l1, nil), "", nil},
		{"a deferral with no answer in it",
			deferred(core.DeferClassBudget, skipReason, "  \n ", miss), "", nil},
		{"a cut final with no answer in it",
			deferred(core.DeferClassAbstention, cutReason, "", cut), "", nil},
		{"a delivered result is never a salvage",
			core.AgentWireResult{Output: l1, SchemaMiss: true, DeferClass: core.DeferClassBudget}, "", nil},
		{"a deferral that already carries a structured object",
			deferred(core.DeferClassBudget, skipReason, l1, func(w *core.AgentWireResult) { w.SchemaMiss = true; w.Structured = []byte(`{"findings":[]}`) }), "", nil},
	} {
		kind, lines := Salvage(tc.w)
		if kind != tc.kind {
			t.Errorf("%s: kind = %q, want %q", tc.name, kind, tc.kind)
			continue
		}
		if tc.kind == "" {
			if lines != nil {
				t.Errorf("%s: a non-salvage must return no lines, got %q", tc.name, lines)
			}
			continue
		}
		if strings.Join(lines, "\n") != strings.Join(tc.lines, "\n") || len(lines) != len(tc.lines) {
			t.Errorf("%s: lines = %q, want %q", tc.name, lines, tc.lines)
		}
	}
}

// Salvaged lines are ordinary lines: they meet every filter Report applies, so a cut answer
// full of hollow, invented and echoed lines salvages to nothing and a mixed one keeps only
// what a structured answer's survivors would be.
func TestSalvagedLinesMeetEveryFilterReportApplies(t *testing.T) {
	diff := "--- a/run.go\n+++ b/run.go\n@@ -1 +1 @@\n+x\n"
	_, lines := Salvage(core.AgentWireResult{
		Deferred: true, OutputTruncated: true, DeferClass: core.DeferClassAbstention,
		Output: strings.Join([]string{
			"severe | run.go:5 | off-by-one | reads past the end",
			"The loop now iterates over every element", // hollow
			"severe | ghost.go:1 | invented | not in the diff",
			exampleFinding,
			"moderate | run.go:5 | the bound is wrong | one past the end", // same line as the first
			"minor | run.go:8 | the cut li",
		}, "\n"),
	})
	rep := Report(lines, diff, 0)
	if len(rep.Findings) != 1 || rep.Findings[0].Claim != "off-by-one" || len(rep.Findings[0].Also) != 1 {
		t.Fatalf("want the one grounded finding with the same-line restatement folded into it: %+v", rep.Findings)
	}
	if rep.DroppedHollow != 1 || rep.DroppedUngrounded != 1 || rep.DroppedEcho != 1 || rep.DroppedDuplicate != 1 {
		t.Fatalf("every filter must have counted its line, and the fragment must not be among them: %+v", rep)
	}
}

// The live fixtures below are the seats' own words, captured 2026-10-09 by running this lane's
// exact prompt and schema on the fleet (agent_delegate, one node pinned per call), trimmed to a
// few findings. They are what the structured re-pack was handed, and RawLines is what reads them.
const (
	// A qwen3.6-35b-a3b seat: one perfectly formed line. The grammar-lane re-pack returned it as
	// ["note text is inverted relative to intent", "the condition and branches swap ..."] - the
	// claim and the why as two list items, the severity and the location gone.
	liveQwenAnswer = `severe | internal/mcpserver/mcpserver.go:3764 | note text is inverted relative to intent | the condition and branches swap the "found nothing" vs "filtered" messages`

	// A mimo-9b seat after the cut-final re-issue said "return the same JSON object that was asked
	// for": a fenced JSON object of finding objects, not the lines the prompt asked for. The
	// re-pack returned the three claims as bare strings.
	liveMimoAnswer = "```json\n" + `{
  "findings": [
    {
      "severity": "moderate",
      "path": "internal/mcpserver/mcpserver.go",
      "line": 3717,
      "claim": "The clean-verdict gate now fires whenever any filter dropped anything, not only when nothing was filtered",
      "why": "A run that produced text but was entirely filtered out now defers, conflating a hollow review with a broken run"
    },
    {
      "severity": "minor",
      "path": "internal/mcpserver/mcpserver.go",
      "line": 3748,
      "claim": "The empty-findings note is emitted after the defer gate, so it can never be reached for a deferred run",
      "why": "The note branch is dead for the case it was written to explain"
    },
    {
      "severity": "minor",
      "path": "internal/mcpserver/mcpserver.go",
      "line": 3762,
      "claim": "unearnedReason counts only hollow, ungrounded and echo drops, silently ignoring DroppedDuplicate",
      "why": "The stated total can undercount the dropped findings"
    }
  ]
}
` + "```"
)

func TestRawLinesReadsThePipeLinesAndTheJSONObjectsASeatFallsBackTo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   []string
	}{
		{"pipe lines are the answer split on newlines",
			"severe | run.go:5 | off-by-one | reads past the end\nmoderate | run.go:9 | nil check | panics",
			[]string{"severe | run.go:5 | off-by-one | reads past the end", "moderate | run.go:9 | nil check | panics"}},
		{"the live qwen answer is one line",
			liveQwenAnswer, []string{liveQwenAnswer}},
		{"the live mimo answer: a fenced document of finding objects, rendered back to pipe lines",
			liveMimoAnswer, []string{
				"moderate | internal/mcpserver/mcpserver.go:3717 | The clean-verdict gate now fires whenever any filter dropped anything, not only when nothing was filtered | A run that produced text but was entirely filtered out now defers, conflating a hollow review with a broken run",
				"minor | internal/mcpserver/mcpserver.go:3748 | The empty-findings note is emitted after the defer gate, so it can never be reached for a deferred run | The note branch is dead for the case it was written to explain",
				"minor | internal/mcpserver/mcpserver.go:3762 | unearnedReason counts only hollow, ungrounded and echo drops, silently ignoring DroppedDuplicate | The stated total can undercount the dropped findings",
			}},
		{"the object the prompt's schema names, with string items",
			`{"findings":["severe | run.go:5 | off-by-one | reads past the end","minor | run.go:9 | naming | cosmetic"]}`,
			[]string{"severe | run.go:5 | off-by-one | reads past the end", "minor | run.go:9 | naming | cosmetic"}},
		{"a bare array, the key file instead of path, the line as a string",
			`[{"severity":"severe","file":"run.go","line":"5","claim":"off-by-one","why":"reads past the end"}]`,
			[]string{"severe | run.go:5 | off-by-one | reads past the end"}},
		{"no line: the file stands alone",
			"{\"findings\":[{\"severity\":\"minor\",\"path\":\"run.go\",\"claim\":\"naming\",\"why\":\"cosmetic\"}]}",
			[]string{"minor | run.go | naming | cosmetic"}},
		{"a path that already carries its line is not given a second one",
			`[{"severity":"minor","path":"run.go:7","line":7,"claim":"naming","why":"cosmetic"}]`,
			[]string{"minor | run.go:7 | naming | cosmetic"}},
		{"a pipe inside a value would shift the fields, so it is written as a slash",
			`[{"severity":"minor","path":"run.go","line":3,"claim":"a | b is wrong","why":"x"}]`,
			[]string{"minor | run.go:3 | a / b is wrong | x"}},
		{"a sentence of preamble before the document",
			"Here are the defects I found:\n[{\"severity\":\"severe\",\"path\":\"run.go\",\"line\":5,\"claim\":\"off-by-one\",\"why\":\"reads past the end\"}]",
			[]string{"severe | run.go:5 | off-by-one | reads past the end"}},
		{"a bracket inside a pipe-line claim cannot hijack a plain answer",
			"severe | run.go:5 | indexes xs[len(xs)] | panics at runtime",
			[]string{"severe | run.go:5 | indexes xs[len(xs)] | panics at runtime"}},
		{"the loop's repetition marker is just another plain line",
			"minor | run.go:5 | naming | cosmetic\n[repetition trimmed x4]",
			[]string{"minor | run.go:5 | naming | cosmetic", "[repetition trimmed x4]"}},
		{"a document cut mid-way does not parse, so the answer reads as plain lines",
			"```json\n{\n  \"findings\": [\n    {\"severity\": \"minor\"",
			[]string{"```json", "{", "  \"findings\": [", "    {\"severity\": \"minor\""}},
		{"a document that yields no finding reads as the prose around it",
			"I checked [1, 2, 3] and found nothing\nNONE",
			[]string{"I checked [1, 2, 3] and found nothing", "NONE"}},
	} {
		got := RawLines(tc.output)
		if strings.Join(got, "\n") != strings.Join(tc.want, "\n") || len(got) != len(tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// What the raw reading is for, end to end through the filters: both live shapes reach the caller
// with every field the seat wrote, where the re-pack's copy of the same answer is all hollow.
func TestTheRawReadingKeepsWhatTheRepackFlattened(t *testing.T) {
	diff := "--- a/internal/mcpserver/mcpserver.go\n+++ b/internal/mcpserver/mcpserver.go\n@@ -1 +1 @@\n+x\n"
	for _, tc := range []struct {
		name      string
		output    string
		flattened []string
		want      int
	}{
		{"qwen3.6: one line split into its fields", liveQwenAnswer,
			[]string{"note text is inverted relative to intent", `the condition and branches swap the "found nothing" vs "filtered" messages`}, 1},
		{"mimo-9b: finding objects reduced to their claims", liveMimoAnswer,
			[]string{
				"The clean-verdict gate now fires whenever any filter dropped anything, not only when nothing was filtered",
				"The empty-findings note is emitted after the defer gate, so it can never be reached for a deferred run",
				"unearnedReason counts only hollow, ungrounded and echo drops, silently ignoring DroppedDuplicate",
			}, 3},
	} {
		flat := Report(tc.flattened, diff, 0)
		if flat.Survivors() != 0 || flat.DroppedHollow != len(tc.flattened) {
			t.Fatalf("%s: the re-pack's copy must be all hollow (this is the reported shape): %+v", tc.name, flat)
		}
		raw := Report(RawLines(tc.output), diff, 0)
		if raw.Survivors() != tc.want || raw.DroppedHollow != 0 {
			t.Fatalf("%s: the seat's own answer holds %d findings: %+v", tc.name, tc.want, raw)
		}
		for _, f := range raw.Findings {
			if f.Severity == "" || f.File != "internal/mcpserver/mcpserver.go" || f.Line == 0 || f.Claim == "" || f.Why == "" {
				t.Errorf("%s: every field the seat wrote must survive: %+v", tc.name, f)
			}
		}
	}
}
