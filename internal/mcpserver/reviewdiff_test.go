// offload_review_diff's MCP surface. The parsing, grounding and ranking rules live in
// internal/reviewlane and are tested there; what is pinned HERE is the front door's own
// jobs — advertise the tool unconditionally, ship the seat a contract carrying the task and
// the diff and NOTHING else, publish grounded/ranked findings with the ungrounded ones
// counted, and turn every failure into a deferred-shape result instead of an MCP error.

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// reviewDiff is a small unified diff over one file, used by every case below.
const reviewDiff = "diff --git a/run.go b/run.go\n--- a/run.go\n+++ b/run.go\n@@ -1,3 +1,4 @@\n+for i := 0; i <= len(xs); i++ {\n"

// seatFindings builds the wire result a healthy seat produces: the structured re-pack's
// {findings:[...]} string array, alongside the raw answer it was extracted from.
//
// With no lines the raw answer is the NONE token, because that is what a GENUINE clean review
// looks like. Leaving it empty would make this fixture the BROKEN-run shape instead — and
// those two were indistinguishable until the clean-verdict gate landed, which is precisely
// why the fixture has to commit to being one of them. seatRaw builds the other.
func seatFindings(lines ...string) core.AgentWireResult {
	if lines == nil {
		lines = []string{}
	}
	arr, _ := json.Marshal(map[string]any{"findings": lines})
	raw := strings.Join(lines, "\n")
	if raw == "" {
		raw = "NONE"
	}
	return core.AgentWireResult{
		SchemaVersion: core.AgentWireSchemaVersion,
		Seat:          "fake-seat",
		Output:        raw,
		Structured:    arr,
		Steps:         2,
		StopReason:    "done",
	}
}

// seatRaw sets the seat's raw answer independently of its structured findings. The two CAN
// disagree, and they disagree exactly when a run is broken — an empty final message re-packs
// into a schema-valid empty array.
func seatRaw(output string, lines ...string) core.AgentWireResult {
	w := seatFindings(lines...)
	w.Output = output
	return w
}

func reviewArgs(t *testing.T, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The lane's whole value is that it is reachable at the moment of deciding, so — like
// offload_ask and unlike agent_delegate — it must be on tools/list with no config flag set.
func TestReviewDiffAdvertisedUnconditionally(t *testing.T) {
	for _, tool := range listTools(t, config.Default()) {
		if tool.Name == "offload_review_diff" {
			if tool.InputSchema == nil {
				t.Fatal("offload_review_diff advertised without an input schema")
			}
			// The two properties the council required of the description: advisory
			// standing, and findings as triage input rather than verdicts.
			for _, want := range []string{"ADVISORY ONLY", "TRIAGE INPUT", "does-it-actually-work"} {
				if !strings.Contains(tool.Description, want) {
					t.Errorf("description must say %q — a lead reading it must not mistake findings for a verdict", want)
				}
			}
			return
		}
	}
	t.Fatal("offload_review_diff not advertised on tools/list")
}

// The mechanism IS the product: the seat must receive the task and the diff, and no history.
func TestReviewDiffShipsTaskAndDiffAndNothingElse(t *testing.T) {
	var got core.AgentContract
	s := askTestServer(t, func(_ context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		got = c
		return seatFindings(), nil
	})
	if _, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	}))); err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	if !strings.Contains(got.Goal, "iterate over every element exactly once") {
		t.Error("the task statement must reach the seat — without intent a reviewer grades style")
	}
	if !strings.Contains(got.Goal, "for i := 0; i <= len(xs); i++") {
		t.Error("the diff body must reach the seat")
	}
	if len(got.Context) != 0 {
		t.Errorf("no context docs may ride along: %d", len(got.Context))
	}
	if len(got.Acceptance) != 0 {
		t.Errorf("no acceptance check: an empty findings list is a correct outcome here, so any check would punish a clean diff or pass anything; got %v", got.Acceptance)
	}
}

// TestReviewDiffWallComesFromTheBoxTimeout (0.115.21, register D-03/D-09):
// the review contract's wall is the box's agent_timeout_sec when that is
// larger than the wire default — a 51 KB diff on the 30 tok/s 27B timed out
// at the 300 s default after 9 steps (2026-09-10). The wire ceiling still
// caps it, and a box that seeds nothing keeps the default.
func TestReviewDiffWallComesFromTheBoxTimeout(t *testing.T) {
	for _, tc := range []struct{ box, want int }{{600, 600}, {0, core.AgentTimeoutSecDefault}, {5000, core.AgentTimeoutSecCap}, {120, core.AgentTimeoutSecDefault}} {
		var got core.AgentContract
		home := t.TempDir()
		cfg := config.Default()
		cfg.Home = home
		cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
		cfg.AgentTimeoutSec = tc.box
		s := New(pipeline.New(cfg, nil, nil, nil))
		s.localAgent = func(_ context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
			got = c
			return seatFindings(), nil
		}
		if _, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
			"diff": reviewDiff, "task": "iterate over every element exactly once",
		}))); err != nil {
			t.Fatalf("handleReviewDiff: %v", err)
		}
		if got.TimeoutSec != tc.want {
			t.Errorf("agent_timeout_sec %d: review wall = %d s, want %d", tc.box, got.TimeoutSec, tc.want)
		}
	}
}

func TestReviewDiffPublishesRankedGroundedFindings(t *testing.T) {
	s := askTestServer(t, func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return seatFindings(
			"minor | run.go:9 | naming | cosmetic",
			"severe | nowhere.go:1 | invented file | this file is not in the diff",
			"severe | run.go:5 | off-by-one in the loop bound | indexes one past the end",
		), nil
	})
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != nil {
		t.Fatalf("a healthy run must not defer: %v", m)
	}
	findings, _ := m["findings"].([]any)
	if len(findings) != 2 {
		t.Fatalf("want 2 grounded findings, got %v", m["findings"])
	}
	first, _ := findings[0].(map[string]any)
	if first["severity"] != "severe" || first["file"] != "run.go" || first["line"].(float64) != 5 {
		t.Errorf("severe grounded finding must lead, with file and line: %v", first)
	}
	if first["claim"] != "off-by-one in the loop bound" || first["why"] != "indexes one past the end" {
		t.Errorf("claim/why must survive the round trip: %v", first)
	}
	if m["dropped_ungrounded"].(float64) != 1 {
		t.Errorf("the invented file must be dropped AND counted: %v", m["dropped_ungrounded"])
	}
	if m["reviewed_bytes"].(float64) != float64(len(reviewDiff)) {
		t.Errorf("reviewed_bytes must report what was actually judged: %v", m["reviewed_bytes"])
	}
	if m["note"] != nil {
		t.Errorf("the clean-review note belongs only on an empty findings list: %v", m["note"])
	}
}

// "No findings" is the one result a lead might read as reassurance, so it must say in words
// what it is not.
func TestReviewDiffEmptyFindingsSaysWhatItIsNot(t *testing.T) {
	s := askTestServer(t, func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return seatFindings(), nil
	})
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	m := decodeResult(t, res)
	findings, ok := m["findings"].([]any)
	if !ok || len(findings) != 0 {
		t.Fatalf("an empty review must publish [], never null or a missing field: %v", m["findings"])
	}
	if note, _ := m["note"].(string); !strings.Contains(note, "not a verification") {
		t.Errorf("an empty findings list must not read as a pass: %q", note)
	}
}

// THE CRITICAL CASE. A structurally valid but UNEARNED empty findings array: agent/loop.go
// returns stop_reason "done" the moment the model stops requesting tools, with no check that
// the final message carries content, so an empty Output reaches the re-pack, which extracts
// findings from an empty string and returns a schema-valid {"findings":[]}. steps, stop_reason
// and the array are all identical to a real clean review — the raw answer is the only field
// that differs, so it is what decides.
func TestReviewDiffDefersOnAnEmptyReviewItDidNotEarn(t *testing.T) {
	for name, raw := range map[string]string{"empty": "", "whitespace": "  \n ", "too short to be a verdict": "ok"} {
		s := askTestServer(t, func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
			return seatRaw(raw), nil
		})
		res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
			"diff": reviewDiff, "task": "iterate over every element exactly once",
		})))
		if err != nil {
			t.Fatalf("%s: handleReviewDiff: %v", name, err)
		}
		m := decodeResult(t, res)
		if m["deferred"] != true {
			t.Fatalf("%s: an unearned empty review must defer, not publish a clean bill of health: %v", name, m)
		}
		if reason, _ := m["reason"].(string); !strings.Contains(reason, "clean NONE verdict") {
			t.Errorf("%s: the defer must name WHY it is not trusted: %q", name, reason)
		}
		if m["findings"] != nil || m["note"] != nil {
			t.Errorf("%s: a defer must carry neither a findings list nor the clean-review note: %v", name, m)
		}
	}
}

// A list the FILTERS emptied is not a review (0.178.0). Until then a run whose findings were
// all dropped came back as a success carrying a note — a hollow or invented review published
// as if it were a clean one. It now defers like a list the seat left empty, and the deferral
// carries the count and a reason naming it, so the caller learns why and reviews the diff.
func TestReviewDiffAllUngroundedDefersAndNamesTheDrops(t *testing.T) {
	s := askTestServer(t, func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return seatFindings(
			"severe | ghost.go:1 | invented file | not in the diff",
			"minor | phantom.go:2 | also invented | still not in the diff",
		), nil
	})
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true || m["defer_class"] != string(core.DeferClassAbstention) {
		t.Fatalf("a list emptied by filtering must defer as an abstention, not publish a success: %v", m)
	}
	if m["dropped_ungrounded"] != float64(2) {
		t.Fatalf("both invented findings must be counted ON THE DEFER: %v", m)
	}
	reason, _ := m["reason"].(string)
	if !strings.Contains(reason, "2 named files the diff never touched") || !strings.Contains(reason, "clean NONE verdict") {
		t.Errorf("the reason must name the drops and the missing verdict: %q", reason)
	}
	if m["findings"] != nil || m["note"] != nil {
		t.Errorf("a defer carries neither a findings list nor the empty-review note: %v", m)
	}
}

// The exact live shape (2026-10-09, a 228-line diff on a small seat): four lines, each only a
// claim — no severity, no file, no why — that merely restated the diff. They arrived as four
// findings with every other field empty, published as a success with no note and no counts.
func TestReviewDiffDefersWhenEveryFindingIsHollow(t *testing.T) {
	s := askTestServer(t, func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return seatFindings(
			"The loop now iterates over every element of the slice",
			"A bounds check was added before the index is used",
			"The result of the iteration is returned to the caller",
			"The function signature was left unchanged",
		), nil
	})
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true || m["defer_class"] != string(core.DeferClassAbstention) {
		t.Fatalf("a hollow review must defer as an abstention: %v", m)
	}
	if m["dropped_hollow"] != float64(4) {
		t.Fatalf("all four hollow lines must be counted on the defer: %v", m)
	}
	if reason, _ := m["reason"].(string); !strings.Contains(reason, "4 had no severity, file or why") {
		t.Errorf("the reason must say the findings were hollow: %q", reason)
	}
	if m["findings"] != nil {
		t.Errorf("a defer must not carry a findings list: %v", m["findings"])
	}
}

// Every filter that can empty a list is named in the one reason, each with its own count.
func TestReviewDiffDeferReasonNamesEachFilterThatEmptiedTheList(t *testing.T) {
	s := askTestServer(t, func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return seatFindings(
			"The loop now iterates over every element of the slice",
			"severe | ghost.go:1 | invented file | not in the diff",
			exampleReviewLine,
		), nil
	})
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("hollow + ungrounded + echo, nothing surviving, must defer: %v", m)
	}
	for _, key := range []string{"dropped_hollow", "dropped_ungrounded", "dropped_echo"} {
		if m[key] != float64(1) {
			t.Errorf("%s must ride the defer: %v", key, m)
		}
	}
	reason, _ := m["reason"].(string)
	for _, want := range []string{"none of the seat's 3 findings", "1 had no severity, file or why", "1 named files the diff never touched", "1 handed the prompt's own template back"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the reason must contain %q: %q", want, reason)
		}
	}
}

// exampleReviewLine is the prompt's worked example, handed back verbatim (the echo the lane
// counts); reviewlane keeps the text unexported, so the front-door test spells it out.
const exampleReviewLine = "moderate | internal/store/load.go:57 | the returned error is discarded with _ | a failed load reads as an empty store"

// The deferral did not remove the honest clean result: a seat whose own raw answer is the
// explicit NONE verdict still earns an empty list and its note. The one corner that keeps the
// "never contradict the drop count" property alive is a clean verdict beside lines that were
// all dropped (a self-contradicting answer, or a re-pack that invented a line the answer never
// held): empty, counted, and worded as what it is.
func TestReviewDiffCleanVerdictBesideDroppedLinesStaysAnEmptyResult(t *testing.T) {
	s := askTestServer(t, func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return seatRaw("NONE", "severe | ghost.go:1 | invented file | not in the diff", "The loop now iterates over every element"), nil
	})
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != nil {
		t.Fatalf("an explicit NONE verdict is the one thing that earns an empty list: %v", m)
	}
	if findings, ok := m["findings"].([]any); !ok || len(findings) != 0 {
		t.Fatalf("the list is empty, and published as [] rather than null: %v", m["findings"])
	}
	if m["dropped_ungrounded"] != float64(1) || m["dropped_hollow"] != float64(1) {
		t.Fatalf("both drops must be counted: %v", m)
	}
	note, _ := m["note"].(string)
	if strings.Contains(note, "found nothing in the diff") {
		t.Errorf("the note contradicts the drop counts it ships beside: %q", note)
	}
	if !strings.Contains(note, "survived filtering") || !strings.Contains(note, "no severity, file or why") {
		t.Errorf("the note must say what actually happened: %q", note)
	}
}

// dropped_hollow rides a delivered review on the same terms as the other counts: present when
// non-zero, beside the findings that did survive.
func TestReviewDiffPublishesDroppedHollowBesideSurvivors(t *testing.T) {
	s := askTestServer(t, func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return seatFindings(
			"severe | run.go:5 | off-by-one in the loop bound | indexes one past the end",
			"The loop now iterates over every element of the slice",
			"A bounds check was added before the index is used",
		), nil
	})
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != nil {
		t.Fatalf("one finding survived, so this is a review: %v", m)
	}
	if findings, _ := m["findings"].([]any); len(findings) != 1 {
		t.Fatalf("want the one structured finding: %v", m["findings"])
	}
	if m["dropped_hollow"] != float64(2) {
		t.Fatalf("the two bare claims must be counted: %v", m)
	}
	if m["note"] != nil {
		t.Errorf("the empty-review note belongs only on an empty list: %v", m["note"])
	}
}

// max_findings had no front-door test at all, and truncation was silent while drops were
// counted — two structurally identical "we found more than we are showing you" situations
// treated differently.
func TestReviewDiffHonoursMaxFindingsAndReportsWhatItHid(t *testing.T) {
	s := askTestServer(t, func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return seatFindings(
			"severe | run.go:1 | one | why one",
			"severe | run.go:2 | two | why two",
			"moderate | run.go:3 | three | why three",
			"minor | run.go:4 | four | why four",
			"minor | run.go:5 | five | why five",
		), nil
	})
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once", "max_findings": 3,
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	m := decodeResult(t, res)
	if findings, _ := m["findings"].([]any); len(findings) != 3 {
		t.Fatalf("max_findings must narrow the published list: %v", m["findings"])
	}
	if m["truncated_by_cap"].(float64) != 2 {
		t.Fatalf("what the cap hid must be reported the way drops are: %v", m["truncated_by_cap"])
	}
}

// Register D-90: the same defect reported more than once (here, a plain restatement one line
// off plus a punctuation variant) must publish dropped_duplicate on the wire beside the other
// two drop counts, and dedupe must run before the cap so the genuinely distinct finding still
// gets published rather than being crowded out by copies of the other one.
func TestReviewDiffPublishesDroppedDuplicateAndDedupesBeforeTheCap(t *testing.T) {
	s := askTestServer(t, func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return seatFindings(
			"minor | run.go:9 | naming issue | cosmetic",
			"minor | run.go:9 | naming issue | cosmetic",
			"minor | run.go:8 | naming issue! | cosmetic",
			"severe | run.go:5 | off-by-one in the loop bound | indexes one past the end",
		), nil
	})
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once", "max_findings": 1,
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != nil {
		t.Fatalf("a run that produced text is not the broken-run shape: %v", m)
	}
	if m["dropped_duplicate"].(float64) != 2 {
		t.Fatalf("the 2 restatements of the naming finding must be counted: %v", m["dropped_duplicate"])
	}
	findings, _ := m["findings"].([]any)
	if len(findings) != 1 {
		t.Fatalf("cap of 1 must still publish exactly 1: %v", findings)
	}
	first, _ := findings[0].(map[string]any)
	if first["severity"] != "severe" {
		t.Fatalf("dedupe must run BEFORE the cap, or the 3 duplicate copies of the minor finding would have filled the one slot instead of the distinct severe one: %v", first)
	}
}

// Three findings stacked on one line in different words (the 2026-10-09 report, a 48 KB diff)
// are one place to look. They publish as ONE finding - the most severe - with the other two
// claims in `also`, and the two absorbed findings are counted in dropped_duplicate. Nothing the
// reviewer said is lost, and a finding that absorbed nothing carries no `also` key at all.
func TestReviewDiffFoldsASameLineStackIntoOneFindingWithAlso(t *testing.T) {
	s := askTestServer(t, func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return seatFindings(
			"moderate | run.go:5 | the loop bound is off by one | reads one past the end",
			"severe | run.go:5 | indexes past the end of xs | out-of-range panic at runtime",
			"minor | run.go:5 | the comparison should be strict | fence-post error",
			"minor | run.go:9 | naming is inconsistent | cosmetic",
		), nil
	})
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != nil {
		t.Fatalf("a delivered review must not defer: %v", m)
	}
	findings, _ := m["findings"].([]any)
	if len(findings) != 2 {
		t.Fatalf("want the folded stack plus the lone finding: %v", m["findings"])
	}
	first, _ := findings[0].(map[string]any)
	if first["severity"] != "severe" || first["claim"] != "indexes past the end of xs" {
		t.Fatalf("the most severe member of the stack must be the one published: %v", first)
	}
	also, _ := first["also"].([]any)
	if len(also) != 2 || also[0] != "the loop bound is off by one" || also[1] != "the comparison should be strict" {
		t.Fatalf("the other two claims must ride in also, in the seat's order: %v", first["also"])
	}
	if m["dropped_duplicate"] != float64(2) {
		t.Fatalf("the two absorbed findings must be counted: %v", m)
	}
	second, _ := findings[1].(map[string]any)
	if _, has := second["also"]; has {
		t.Fatalf("a finding that absorbed nothing must not carry an also key: %v", second)
	}
}

func TestReviewDiffRequiresExactlyOneDiffSource(t *testing.T) {
	s := askTestServer(t, func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		t.Error("the seat must never be reached on a caller-input refusal")
		return core.AgentWireResult{}, nil
	})
	for name, args := range map[string]map[string]any{
		"neither": {"task": "do the thing"},
		"both":    {"task": "do the thing", "diff": reviewDiff, "diff_path": "x.diff"},
		"no task": {"diff": reviewDiff},
	} {
		res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, args)))
		if err != nil {
			t.Fatalf("%s: handleReviewDiff returned an MCP error instead of a defer: %v", name, err)
		}
		m := decodeResult(t, res)
		if m["deferred"] != true {
			t.Errorf("%s: want a defer, got %v", name, m)
		}
		if reason, _ := m["reason"].(string); strings.TrimSpace(reason) == "" {
			t.Errorf("%s: a defer with no reason is unactionable", name)
		}
	}
}

// Every failure is a defer, never an MCP error: a caller told "the call failed" discards
// the work, while a caller told why reviews the diff itself.
func TestReviewDiffDefersOnASeatFailure(t *testing.T) {
	s := askTestServer(t, func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{}, errors.New("planner unreachable")
	})
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("want a deferred result, got an MCP error: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true || !strings.Contains(m["reason"].(string), "planner unreachable") {
		t.Fatalf("the seat's failure must arrive as a named defer: %v", m)
	}
}

// A seat that comes back with no structured findings must DEFER, never degrade into an
// empty findings list — the one shape a caller could misread as "the diff is clean".
func TestReviewDiffDefersWhenTheSeatReturnedNoStructuredFindings(t *testing.T) {
	s := askTestServer(t, func(_ context.Context, _ core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Seat: "fake-seat", Output: "I could not read the diff", StopReason: "done"}, nil
	})
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("no structured findings must defer, not publish a clean review: %v", m)
	}
	if m["findings"] != nil {
		t.Errorf("a defer must not carry a findings list: %v", m["findings"])
	}
}

func TestReviewDiffReadsDiffPathUnderReadRootAndRefusesOutsideIt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "change.diff")
	if err := os.WriteFile(p, []byte(reviewDiff), 0o644); err != nil {
		t.Fatal(err)
	}
	var got core.AgentContract
	s := askTestServer(t, func(_ context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		got = c
		return seatFindings("severe | run.go:5 | off-by-one | reads past the end"), nil
	})

	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff_path": p, "read_root": dir, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	if m := decodeResult(t, res); m["deferred"] != nil {
		t.Fatalf("a readable diff_path must not defer: %v", m)
	}
	if !strings.Contains(got.Goal, "for i := 0; i <= len(xs); i++") {
		t.Error("diff_path's content must reach the seat")
	}

	// Same file, read_root pointing elsewhere: the confined reader must refuse, and the
	// refusal must arrive as a defer naming the path.
	res, err = s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff_path": p, "read_root": t.TempDir(), "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("a diff_path outside read_root must be refused: %v", m)
	}
}
