// reviewfence_test.go pins the review lane's behaviour under a GPU lease it does
// not own (register D-110).
//
// The defect: offload_review_diff built a LOCAL agent loop unconditionally. Under
// another process's exclusive or draining lease that loop waits at the affinity
// cordon for the whole agent_lease_wait_sec and is then filed as a capacity defer
// — so the one lane a lead reaches for at the moment of deciding was unusable for
// the lease's entire length, while an idle fleet seat sat there. The fix offers the
// review to the fleet FIRST when the fence is FOREIGN, and leaves the local path
// exactly as it was when the lease is the caller's own.

package mcpserver

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/reviewlane"
)

// fenceServer builds a review-lane server whose state dir (and therefore whose
// machine-wide lease) is private to the test, and arms an exclusive text lease on
// it. It returns the server and the held lease's epoch, which is what an INHERITED
// lease is proved by.
func fenceServer(t *testing.T, local delegate.LocalRunner) (*Server, uint64) {
	t.Helper()
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.StateDir = t.TempDir()
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	m, err := gpulease.OpenAt("", cfg.StateDir)
	if err != nil {
		t.Fatalf("open lease manager: %v", err)
	}
	l, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "5070 Ti bench", Exclusive: true, TTL: time.Hour})
	if err != nil {
		t.Fatalf("acquire lease: %v", err)
	}
	t.Cleanup(func() { _ = l.Release() })
	info := delegate.LocalLease(cfg.GPULockPath, cfg.StateDir)
	if !info.Held || !info.Exclusive {
		t.Fatalf("the test lease did not take: %+v", info)
	}
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.localAgent = local
	return s, info.Epoch
}

// fleetWire is the wire result a remote seat returns for a review: one grounded
// finding and one that names a file the diff never touches, so the post-filters
// have something to do on the REMOTE path too.
func fleetWire() core.AgentWireResult {
	w := seatFindings(
		"severe | run.go:1 | the loop reads one past the end | index out of range at runtime",
		"minor | nowhere.go:9 | invented file | must be dropped as ungrounded",
	)
	w.Seat = "qwen3.8-27b-vllm"
	return w
}

// TestReviewUnderAForeignFenceRidesTheFleet: an exclusive lease this process does
// not hold sends the review to the fleet at route=remote, carrying the SAME
// contract the local seat would have run, and the remote findings go through the
// same filters and are published naming the node that produced them.
func TestReviewUnderAForeignFenceRidesTheFleet(t *testing.T) {
	localCalls := 0
	s, _ := fenceServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		localCalls++
		return seatFindings(), nil
	})
	var gotRoute string
	var gotContracts []core.AgentContract
	s.reviewFleet = func(_ context.Context, _ config.Config, _ delegate.LocalRunner, subtasks []core.AgentContract, route string, _ []string, _ *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		gotRoute, gotContracts = route, subtasks
		return []delegate.PlacedResult{{
			Node: "node-b", Seat: "qwen3.8-27b-vllm",
			PlacementReason: "route=remote forced → node-b",
			Result:          fleetWire(),
		}}, delegate.Summary{Succeeded: 1}, nil
	}
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	if localCalls != 0 {
		t.Errorf("the local seat is fenced — it must not be dialled (%d calls)", localCalls)
	}
	if gotRoute != "remote" {
		t.Errorf("route = %q, want remote: a fenced local seat must never be a silent fallthrough", gotRoute)
	}
	if len(gotContracts) != 1 {
		t.Fatalf("want exactly one contract dispatched, got %d", len(gotContracts))
	}
	c := gotContracts[0]
	if !strings.Contains(c.Goal, "iterate over every element exactly once") || !strings.Contains(c.Goal, "for i := 0; i <= len(xs); i++") {
		t.Error("the fleet must receive the SAME review prompt — task and diff — the local seat would have run")
	}
	if len(c.OutputSchema) == 0 {
		t.Error("the findings schema must ride along: it is what makes the remote result mechanically checkable (remoteEligible refuses a contract without one)")
	}
	out := decodeResult(t, res)
	if out["deferred"] != nil {
		t.Fatalf("a delivered fleet review must not be a defer: %v", out)
	}
	findings, _ := out["findings"].([]any)
	if len(findings) != 1 {
		t.Fatalf("want the one grounded finding published, got %v", out["findings"])
	}
	if out["dropped_ungrounded"] != float64(1) {
		t.Errorf("the remote path must apply the SAME filters as the local one: %v", out)
	}
	if out["node"] != "node-b" {
		t.Errorf("the executing node must be published: %v", out)
	}
	if p, _ := out["placement"].(string); !strings.Contains(p, "route=remote") {
		t.Errorf("the placement must be published: %v", out)
	}
	if out["seat"] != "qwen3.8-27b-vllm" {
		t.Errorf("the executing seat must be published: %v", out)
	}
}

// TestReviewUnderItsOwnLeaseStaysLocal: `gpu reserve … -- <claude session>` runs
// this server as the holder's child with GPU_LEASE_EPOCH set. The holder's own
// review must not be routed off the box it reserved.
func TestReviewUnderItsOwnLeaseStaysLocal(t *testing.T) {
	localCalls := 0
	s, epoch := fenceServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		localCalls++
		return seatFindings("severe | run.go:1 | the loop reads one past the end | index out of range at runtime"), nil
	})
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(epoch, 10))
	fleetCalls := 0
	s.reviewFleet = func(context.Context, config.Config, delegate.LocalRunner, []core.AgentContract, string, []string, *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		fleetCalls++
		return nil, delegate.Summary{}, nil
	}
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	if fleetCalls != 0 {
		t.Errorf("an INHERITED lease is not a fence for its own holder — the fleet must not be asked (%d calls)", fleetCalls)
	}
	if localCalls != 1 {
		t.Errorf("the local path must run unchanged under the caller's own lease (%d calls)", localCalls)
	}
	out := decodeResult(t, res)
	if findings, _ := out["findings"].([]any); len(findings) != 1 {
		t.Fatalf("the local review must publish unchanged: %v", out)
	}
	if out["node"] != nil {
		t.Errorf("a local review publishes no fleet node: %v", out)
	}
}

// TestReviewUnderAForeignFenceWithNoEligibleRemoteKeepsTheCapacityDefer: when the
// fleet takes nothing, today's path stands — the local wait, then the capacity
// defer — and the caller is told BOTH why the fleet could not help and, through
// the lease message, when the card is declared free.
func TestReviewUnderAForeignFenceWithNoEligibleRemoteKeepsTheCapacityDefer(t *testing.T) {
	localCalls := 0
	s, _ := fenceServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		localCalls++
		return core.AgentWireResult{
			SchemaVersion: core.AgentWireSchemaVersion,
			Deferred:      true,
			DeferClass:    core.DeferClassCapacity,
			Reason:        `gpu busy: gpu-lease timeout after 5m0s (bound 5m0s): a text job holds the GPU (pid 4242, held 2m0s, reason "5070 Ti bench"), declared until 11:40PM (~37m0s left)`,
			Seat:          "agent-pool",
		}, nil
	})
	s.reviewFleet = func(_ context.Context, _ config.Config, _ delegate.LocalRunner, _ []core.AgentContract, _ string, _ []string, _ *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		return []delegate.PlacedResult{{
			Unplaced:        true,
			PlacementReason: "route=remote: no eligible remote",
			Result: core.AgentWireResult{
				SchemaVersion: core.AgentWireSchemaVersion,
				Deferred:      true,
				DeferClass:    core.DeferClassCapacity,
				Reason:        "route=remote: no remotes are configured (delegate_remotes is empty)",
			},
		}}, delegate.Summary{Deferred: 1}, nil
	}
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	if localCalls != 1 {
		t.Errorf("with no eligible remote the lane must keep today's path (%d local calls)", localCalls)
	}
	out := decodeResult(t, res)
	if out["deferred"] != true || out["defer_class"] != string(core.DeferClassCapacity) {
		t.Fatalf("want the capacity defer: %v", out)
	}
	if r, _ := out["reason"].(string); !strings.Contains(r, "declared until") {
		t.Errorf("the capacity defer must carry the holder's declared window: %v", out)
	}
	fn, _ := out["fleet"].(string)
	if !strings.Contains(fn, "no eligible remote") && !strings.Contains(fn, "no remotes are configured") {
		t.Errorf("the caller must be told the fleet was asked and why it took nothing: %v", out)
	}
}

// TestReviewUnderAForeignFenceSalvagesAFleetSeatsWrittenAnswer (F20): a fleet seat that wrote the
// review and was cut at the completion budget deferred it. The fallthrough would wait out the very
// fence that sent the review to the fleet, to answer nothing; the lines the fleet seat wrote are
// published instead, through the same filters, naming the node that wrote them.
func TestReviewUnderAForeignFenceSalvagesAFleetSeatsWrittenAnswer(t *testing.T) {
	localCalls := 0
	s, _ := fenceServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		localCalls++
		return seatFindings(), nil
	})
	s.reviewFleet = func(context.Context, config.Config, delegate.LocalRunner, []core.AgentContract, string, []string, *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		return []delegate.PlacedResult{{
			Node: "node-b", Seat: "qwen3.8-27b-vllm",
			PlacementReason: "route=remote forced → node-b",
			Result: cutWire(strings.Join([]string{
				"severe | run.go:1 | the loop reads one past the end | index out of range at runtime",
				"minor | nowhere.go:9 | invented file | must be dropped as ungrounded",
				"moderate | run.go:3 | the comparison is not strict | a fence-post error",
				"severe | run.go:4 | the second loop never terminat", // cut by the budget
			}, "\n")),
		}}, delegate.Summary{Deferred: 1}, nil
	}
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	if localCalls != 0 {
		t.Errorf("the local seat is fenced and a salvage needs nothing from it (%d calls)", localCalls)
	}
	out := decodeResult(t, res)
	if out["deferred"] != nil {
		t.Fatalf("the fleet seat's written answer must be published, not deferred: %v", out)
	}
	if findings, _ := out["findings"].([]any); len(findings) != 2 {
		t.Fatalf("want the two grounded complete lines (the invented file and the fragment dropped): %v", out["findings"])
	}
	if out["dropped_ungrounded"] != float64(1) || out["salvaged"] != "output_truncated" {
		t.Errorf("the same filters and the salvage flag must apply on the fleet path: %v", out)
	}
	if out["node"] != "node-b" || out["executed_on"] != "fleet" || out["seat"] != "qwen3.8-27b-vllm" {
		t.Errorf("the executing node must be published beside a salvaged fleet review: %v", out)
	}
	if p, _ := out["placement"].(string); !strings.Contains(p, "route=remote") {
		t.Errorf("the placement must be published: %v", out)
	}
	if out["fence"] == nil {
		t.Errorf("the fence that moved the review must be published: %v", out)
	}
}

// TestReviewUnderAForeignFenceFallsThroughWhenTheFleetsSalvageComesToNothing: a fleet deferral
// whose lines are all hollow (or that holds no answer) is not a review, so the fallthrough stands
// exactly as it did - the local run - and the caller is still told what the fleet answered.
func TestReviewUnderAForeignFenceFallsThroughWhenTheFleetsSalvageComesToNothing(t *testing.T) {
	localCalls := 0
	s, _ := fenceServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		localCalls++
		return seatFindings("severe | run.go:1 | the loop reads one past the end | index out of range at runtime"), nil
	})
	fleet := cutWire("The loop now iterates over every element\nA bounds check was added")
	s.reviewFleet = func(context.Context, config.Config, delegate.LocalRunner, []core.AgentContract, string, []string, *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		return []delegate.PlacedResult{{Node: "node-b", Seat: "qwen3.8-27b-vllm", PlacementReason: "route=remote forced → node-b", Result: fleet}}, delegate.Summary{Deferred: 1}, nil
	}
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{
		"diff": reviewDiff, "task": "iterate over every element exactly once",
	})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	if localCalls != 1 {
		t.Errorf("a salvage that comes to no review must leave the local fallthrough alone (%d local calls)", localCalls)
	}
	out := decodeResult(t, res)
	if findings, _ := out["findings"].([]any); len(findings) != 1 || out["salvaged"] != nil || out["node"] != nil {
		t.Fatalf("the published review is the local one: %v", out)
	}
	if note, _ := out["fleet"].(string); !strings.Contains(note, "cut at the completion budget") {
		t.Errorf("the caller must still be told why the fleet's answer was not used: %v", out["fleet"])
	}
}

// fleetSeam hands the fleet dispatch seam one placed result from a node.
func fleetSeam(w core.AgentWireResult, summary delegate.Summary) func(context.Context, config.Config, delegate.LocalRunner, []core.AgentContract, string, []string, *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
	return func(context.Context, config.Config, delegate.LocalRunner, []core.AgentContract, string, []string, *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		return []delegate.PlacedResult{{
			Node: "node-b", Seat: w.Seat,
			PlacementReason: "route=remote forced → node-b",
			Result:          w,
		}}, summary, nil
	}
}

// A review the fleet delivered under a foreign fence is sealed like a local one: the
// outcome, the evidence bound to the diff, the seat receipt carrying where it ran, and the trace.
func TestReviewUnderAForeignFenceSealsTheFleetsReview(t *testing.T) {
	s, _ := fenceServer(t, neverSeat(t))
	s.reviewFleet = fleetSeam(fleetWire(), delegate.Summary{Succeeded: 1})
	m := reviewCall(t, s, map[string]any{"diff": reviewDiff, "task": "iterate over every element exactly once"})
	if m["outcome"] != reviewlane.OutcomeFindings {
		t.Fatalf("outcome = %v, want FINDINGS: %v", m["outcome"], m)
	}
	ev := evidenceOf(t, m)
	if ev["diff_sha256"] != reviewlane.Fingerprint(reviewDiff) || ev["prompt_sha256"] == nil {
		t.Errorf("the fleet review is bound to the diff and the prompt: %v", ev)
	}
	seat, _ := ev["seat"].(map[string]any)
	if seat["seat"] != "qwen3.8-27b-vllm" || seat["executed_on"] != "fleet" || seat["node"] != "node-b" || seat["answer_sha256"] == nil {
		t.Errorf("the seat receipt must say where the review ran: %v", seat)
	}
	if seat["deferred"] != nil || seat["salvaged"] != nil {
		t.Errorf("a delivered review is no deferral and no salvage: %v", seat)
	}
	rows := traceRowsOf(t, s)
	if len(rows) != 2 || rows[0]["kind"] != "review" || rows[0]["outcome"] != reviewlane.OutcomeFindings || rows[1]["kind"] != "finding" {
		t.Fatalf("want the review row and the one grounded finding's row: %v", rows)
	}
	if counts, _ := rows[0]["counts"].(map[string]any); counts["dropped_ungrounded"] != float64(1) || counts["findings"] != float64(1) {
		t.Errorf("the review row counts what the fleet path's filters took: %v", counts)
	}

	// A clean fleet review, nothing dropped, is a PASS that names the node.
	clean := seatFindings()
	clean.Seat = "qwen3.8-27b-vllm"
	s2, _ := fenceServer(t, neverSeat(t))
	s2.reviewFleet = fleetSeam(clean, delegate.Summary{Succeeded: 1})
	m2 := reviewCall(t, s2, map[string]any{"diff": reviewDiff, "task": "t"})
	if m2["outcome"] != reviewlane.OutcomePass {
		t.Fatalf("a clean fleet review: %v", m2)
	}
	if seat2, _ := evidenceOf(t, m2)["seat"].(map[string]any); seat2["executed_on"] != "fleet" || seat2["node"] != "node-b" {
		t.Errorf("the PASS carries the node it ran on: %v", seat2)
	}
	if rows := traceRowsOf(t, s2); len(rows) != 1 || rows[0]["outcome"] != reviewlane.OutcomePass {
		t.Errorf("a clean fleet review leaves its review row: %v", rows)
	}
}

// The salvage call site. A fleet seat that wrote the review and was cut deferred it;
// the lane publishes the written lines, and that publication is sealed too, with the receipt
// recording the attempt as the deferral it was.
func TestReviewUnderAForeignFenceSealsASalvagedFleetAnswer(t *testing.T) {
	s, _ := fenceServer(t, neverSeat(t))
	cut := cutWire(strings.Join([]string{
		"severe | run.go:1 | the loop reads one past the end | index out of range at runtime",
		"minor | nowhere.go:9 | invented file | must be dropped as ungrounded",
		"moderate | run.go:3 | the comparison is not strict | a fence-post error",
		"severe | run.go:4 | the second loop never terminat", // cut by the budget
	}, "\n"))
	cut.Seat = "qwen3.8-27b-vllm"
	s.reviewFleet = fleetSeam(cut, delegate.Summary{Deferred: 1})
	m := reviewCall(t, s, map[string]any{"diff": reviewDiff, "task": "iterate over every element exactly once"})
	if m["deferred"] != nil || m["salvaged"] != "output_truncated" {
		t.Fatalf("fixture: the written answer must be published as a salvaged review: %v", m)
	}
	if m["outcome"] != reviewlane.OutcomeFindings {
		t.Fatalf("outcome = %v, want FINDINGS: %v", m["outcome"], m)
	}
	ev := evidenceOf(t, m)
	seat, _ := ev["seat"].(map[string]any)
	if seat["executed_on"] != "fleet" || seat["node"] != "node-b" || seat["salvaged"] != "output_truncated" || seat["deferred"] != true {
		t.Errorf("the seat receipt records the fleet node and the salvage: %v", seat)
	}
	if ev["diff_sha256"] != reviewlane.Fingerprint(reviewDiff) {
		t.Errorf("the salvaged review is bound to the diff: %v", ev)
	}
	rows := traceRowsOf(t, s)
	if len(rows) != 3 || rows[0]["kind"] != "review" || rows[0]["outcome"] != reviewlane.OutcomeFindings {
		t.Fatalf("want the review row and two finding rows: %v", rows)
	}
	if counts, _ := rows[0]["counts"].(map[string]any); counts["findings"] != float64(2) || counts["dropped_ungrounded"] != float64(1) {
		t.Errorf("counts: %v", counts)
	}
}

// The salvage call site of the deferred-clean rule: a fleet seat that the node filed as a deferral and whose
// answer reads as a clean NONE is published, and is never a PASS.
func TestReviewUnderAForeignFenceNeverPassesASalvagedDeferral(t *testing.T) {
	s, _ := fenceServer(t, neverSeat(t))
	w := deferredCleanWire()
	w.Seat = "qwen3.8-27b-vllm"
	s.reviewFleet = fleetSeam(w, delegate.Summary{Deferred: 1})
	m := reviewCall(t, s, map[string]any{"diff": reviewDiff, "task": "t"})
	if m["salvaged"] != "wall" || m["outcome"] != reviewlane.OutcomeNotRun {
		t.Fatalf("a deferred fleet run read as clean: %v", m)
	}
	if seat, _ := evidenceOf(t, m)["seat"].(map[string]any); seat["executed_on"] != "fleet" || seat["deferred"] != true {
		t.Errorf("seat receipt: %v", seat)
	}
	if rows := traceRowsOf(t, s); len(rows) != 1 || rows[0]["outcome"] != reviewlane.OutcomeNotRun {
		t.Errorf("trace: %v", rows)
	}
}
