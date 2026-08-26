package reviewlane

import (
	"errors"
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
	c, err := BuildContract("stop the retry loop from spinning forever", "--- a/run.go\n+++ b/run.go\n+for attempts < maxAttempts {\n", 0)
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
	if _, err := BuildContract("  ", "--- a/x\n+++ b/x\n+ok\n", 0); !errors.Is(err, ErrNoTask) {
		t.Fatalf("empty task must refuse with ErrNoTask, got %v", err)
	}
	if _, err := BuildContract("do the thing", "   \n", 0); !errors.Is(err, ErrNoDiff) {
		t.Fatalf("empty diff must refuse with ErrNoDiff, got %v", err)
	}
}

// The diff rides in the GOAL, so core.AgentContract.Validate's context cap never sees it.
// This lane therefore owns the bound itself, and must refuse with the real numbers rather
// than shipping an unbounded prompt at a seat whose window cannot hold it.
func TestBuildContractRefusesAnOversizeDiffWithTheNumbers(t *testing.T) {
	big := strings.Repeat("+x\n", (MaxDiffBytes/3)+16)
	_, err := BuildContract("review it", big, 0)
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
	got, dropped := Report([]string{
		"minor | run.go:3 | naming | cosmetic",
		"severe | ghost.go:1 | invented file | not in the diff",
		"severe | run.go:42 | off-by-one | reads past the end",
		"moderate | run.go:9 | missing nil check | panics on empty input",
	}, diff, 2)
	if dropped != 1 {
		t.Fatalf("the ungrounded finding must be counted: %d", dropped)
	}
	if len(got) != 2 {
		t.Fatalf("cap not applied: %+v", got)
	}
	if got[0].Severity != "severe" || got[0].Claim != "off-by-one" {
		t.Fatalf("severe must lead: %+v", got)
	}
	if got[1].Severity != "moderate" {
		t.Fatalf("moderate must follow severe: %+v", got)
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
