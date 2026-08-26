package reviewlane

import (
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
	// A line the seat wrote in its own shape is KEPT as a claim, never dropped: silently
	// discarding it would turn a reviewer that did work into a clean bill of health.
	if got[3].Claim != "the diff looks fine to me" || got[3].Severity != "" {
		t.Fatalf("an unformatted line must survive as an unranked claim: %+v", got[3])
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
	for _, notClean := range []string{"", "   \n  ", "ok", "."} {
		if VerdictReadsClean(notClean) {
			t.Errorf("%q must not read as a clean verdict — it is the broken-run shape", notClean)
		}
	}
	for _, clean := range []string{"NONE", "none", "None.", "I found no defects in this diff."} {
		if !VerdictReadsClean(clean) {
			t.Errorf("%q must read as a clean verdict", clean)
		}
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
