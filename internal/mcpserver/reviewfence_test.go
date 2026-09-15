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
