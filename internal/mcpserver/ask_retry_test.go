// offload_ask's cross-seat retry. A first attempt that ran on this box's seat and failed its
// acceptance used to be terminal, while agent_delegate retries a failed verification on another
// seat. The ask now gets the same second chance, from the fleet, and publishes it only when it
// is clean.

package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/pipeline"
)

// retryClock is the fake clock a test's local seat advances to say how long its attempt took.
type retryClock struct{ at time.Time }

func (c *retryClock) now() time.Time { return c.at }

// retryFleet is the fleet seam of one test: it records what the ask handed the delegator and
// answers with whatever the test built from the contract it was given.
type retryFleet struct {
	calls    int
	route    string
	contract core.AgentContract
	answer   func(core.AgentContract) delegate.PlacedResult
}

func (f *retryFleet) dispatch(_ context.Context, _ config.Config, _ delegate.LocalRunner, contracts []core.AgentContract, route string, _ []string, _ *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
	f.calls++
	f.route = route
	f.contract = contracts[0]
	return []delegate.PlacedResult{f.answer(contracts[0])}, delegate.Summary{}, nil
}

// retryRig builds a server whose local seat takes `spent` of the fake clock and answers with a
// pair that cites nothing from the file (so the grounding check fails), and whose fleet answers
// with `answer`. retryMinSec is the configured retry floor (agent_retry_min_sec).
func retryRig(t *testing.T, retryMinSec int, spent time.Duration, answer func(core.AgentContract) delegate.PlacedResult) (*Server, *retryFleet, *int) {
	t.Helper()
	home := t.TempDir()
	cfg := config.Default()
	cfg.Home = home
	cfg.StateDir = t.TempDir()
	cfg.LedgerPath = filepath.Join(home, "ledger.jsonl")
	cfg.AgentRetryMinSec = retryMinSec
	clock := &retryClock{at: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	localCalls := 0
	s := New(pipeline.New(cfg, nil, nil, nil))
	s.askNow = clock.now
	s.localAgent = func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		localCalls++
		clock.at = clock.at.Add(spent)
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Seat: "local-seat", Steps: 2, StopReason: "done",
			Output:     "I think the cap is probably 32.",
			Structured: json.RawMessage(`{"answer":"probably 32","evidence":"it is in the config"}`)}, nil
	}
	fleet := &retryFleet{answer: answer}
	s.reviewFleet = fleet.dispatch
	return s, fleet, &localCalls
}

// citing is a fleet node's answer that quotes an anchor the contract's acceptance asks for.
func citing(t *testing.T, node string) func(core.AgentContract) delegate.PlacedResult {
	return func(c core.AgentContract) delegate.PlacedResult {
		anchor := anchorsOf(t, c)[0]
		return delegate.PlacedResult{Node: node, Seat: "seat-" + node, PlacementReason: "route=remote → " + node,
			Result: core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Seat: "seat-" + node, Steps: 1, StopReason: "done",
				Output:     "the cap is 32",
				Structured: json.RawMessage(`{"answer":"the cap is 32","evidence":"const ` + anchor + ` = 32"}`)}}
	}
}

// uncited is a fleet node's answer that cites nothing from the file, so it fails the same check.
func uncited(node string) func(core.AgentContract) delegate.PlacedResult {
	return func(core.AgentContract) delegate.PlacedResult {
		return delegate.PlacedResult{Node: node, Seat: "seat-" + node, PlacementReason: "route=remote → " + node,
			Result: core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Seat: "seat-" + node, Steps: 1, StopReason: "done",
				Output:     "maybe 33",
				Structured: json.RawMessage(`{"answer":"maybe 33","evidence":"somewhere in the config"}`)}}
	}
}

func askRetryOnce(t *testing.T, s *Server, question string) map[string]any {
	t.Helper()
	dir, p := askFixture(t)
	res, err := s.handleAsk(context.Background(), callReq(askArgs(question, p, dir)))
	if err != nil {
		t.Fatal(err)
	}
	return decodeResult(t, res)
}

// A local first attempt that fails its anchors is retried on the fleet, and a clean retry is the
// published answer: verified, the node it ran on, and a note saying what the first attempt did.
func TestOffloadAskRetriesAFailedLocalAttemptOnTheFleet(t *testing.T) {
	s, fleet, localCalls := retryRig(t, 0, 100*time.Second, citing(t, "node-b"))
	out := askRetryOnce(t, s, "what is the queue cap")

	if *localCalls != 1 || fleet.calls != 1 {
		t.Fatalf("local=%d fleet=%d, want one attempt on each: %v", *localCalls, fleet.calls, out)
	}
	if fleet.route != "remote" {
		t.Fatalf("the retry must go through route remote (never the local seat), got %q", fleet.route)
	}
	// The retry lives inside the contract's own wall: what the first attempt left, never a second full wall.
	if fleet.contract.TimeoutSec != 200 || fleet.contract.TimeoutAuto {
		t.Fatalf("the retry contract carries timeout_sec %d (auto %v), want the 200 s the first attempt left", fleet.contract.TimeoutSec, fleet.contract.TimeoutAuto)
	}
	if out["verified"] != true || out["answer"] != "the cap is 32" {
		t.Fatalf("a clean retry is the published answer: %v", out)
	}
	if out["acceptance_failures"] != nil {
		t.Fatalf("the published retry failed nothing: %v", out["acceptance_failures"])
	}
	if out["retried_on"] != "node-b" || out["node"] != "node-b" || out["seat"] != "seat-node-b" {
		t.Fatalf("the answer must say where the retry ran: %v", out)
	}
	note, _ := out["retry_note"].(string)
	if !strings.Contains(note, "first attempt on local-seat failed_verification: [") || !strings.HasSuffix(note, "this result is the retry") {
		t.Fatalf("retry_note = %q, want the first attempt's verdict and 'this result is the retry'", note)
	}
}

// Both attempts failing publishes the FIRST answer, unverified, with the retry's fate in the note.
// The stored entry carries the same fields, so an identical repeat reads the same story and does
// not spend either seat again.
func TestOffloadAskPublishesTheFirstAnswerWhenTheRetryAlsoFails(t *testing.T) {
	s, fleet, localCalls := retryRig(t, 0, 20*time.Second, uncited("node-b"))
	dir, p := askFixture(t)
	ask := func() map[string]any {
		res, err := s.handleAsk(context.Background(), callReq(askArgs("what is the queue cap", p, dir)))
		if err != nil {
			t.Fatal(err)
		}
		return decodeResult(t, res)
	}
	out := ask()

	if out["verified"] != false || out["answer"] != "probably 32" || out["seat"] != "local-seat" {
		t.Fatalf("the first answer stands, unverified: %v", out)
	}
	if fails, _ := out["acceptance_failures"].([]any); len(fails) == 0 {
		t.Fatalf("the first attempt's failures stay published: %v", out)
	}
	if _, ran := out["node"]; ran {
		t.Fatalf("a first answer carries no fleet provenance: %v", out)
	}
	note, _ := out["retry_note"].(string)
	if out["retried_on"] != "node-b" || !strings.HasPrefix(note, "retry on node-b also failed_verification: [") || !strings.HasSuffix(note, "this result is the first attempt") {
		t.Fatalf("retried_on = %v, retry_note = %q", out["retried_on"], note)
	}

	again := ask()
	if again["cache_hit"] != true || again["retried_on"] != "node-b" || again["retry_note"] != out["retry_note"] || *localCalls != 1 || fleet.calls != 1 {
		t.Fatalf("the cache entry carries the retry fields as they are (local=%d fleet=%d): %v", *localCalls, fleet.calls, again)
	}
}

// A fleet that takes nothing leaves the first answer with a note saying the retry was skipped and
// no retried_on, which would claim a node that never ran it.
func TestOffloadAskNotesASkippedRetryWhenTheFleetTakesNothing(t *testing.T) {
	declined := func(core.AgentContract) delegate.PlacedResult {
		return delegate.PlacedResult{Result: core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Deferred: true,
			DeferClass: core.DeferClassCapacity, Reason: "route=remote: no eligible remote"}}
	}
	s, fleet, _ := retryRig(t, 0, 20*time.Second, declined)
	out := askRetryOnce(t, s, "what is the queue cap")

	if fleet.calls != 1 {
		t.Fatalf("the fleet was never asked: %v", out)
	}
	if out["verified"] != false || out["answer"] != "probably 32" || out["deferred"] != nil {
		t.Fatalf("the first answer stands: %v", out)
	}
	if _, has := out["retried_on"]; has {
		t.Fatalf("no node ran the retry, so retried_on must be absent: %v", out)
	}
	if note, _ := out["retry_note"].(string); note != "retry skipped: route=remote: no eligible remote" {
		t.Fatalf("retry_note = %q", note)
	}
}

// A first attempt that verified never reaches the fleet.
func TestOffloadAskDoesNotRetryAVerifiedFirstAttempt(t *testing.T) {
	s, fleet, localCalls := retryRig(t, 0, 20*time.Second, citing(t, "node-b"))
	s.localAgent = func(_ context.Context, c core.AgentContract, _ delegate.LocalOptions) (core.AgentWireResult, error) {
		*localCalls++
		anchor := anchorsOf(t, c)[0]
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Seat: "local-seat", Steps: 2, StopReason: "done",
			Structured: json.RawMessage(`{"answer":"the cap is 32","evidence":"const ` + anchor + ` = 32"}`)}, nil
	}
	out := askRetryOnce(t, s, "what is the queue cap")

	if out["verified"] != true || fleet.calls != 0 || *localCalls != 1 {
		t.Fatalf("local=%d fleet=%d: %v", *localCalls, fleet.calls, out)
	}
	if _, has := out["retry_note"]; has {
		t.Fatalf("no retry was considered, so no retry_note: %v", out)
	}
}

// The retry has to fit what the first attempt left of the contract's wall. The floor is the one
// agent_delegate applies (the configured agent_retry_min_sec, at least 10 s), held to at most
// half of the ask's fixed 300 s wall: a floor sized for a delegation's wall would otherwise
// switch the lane off.
func TestOffloadAskSkipsTheRetryPastTheTimeFloor(t *testing.T) {
	for _, tc := range []struct {
		name        string
		retryMinSec int
		spent       time.Duration
		wantRetry   bool
		wantNote    string
	}{
		{"default floor, wall nearly gone", 0, 295 * time.Second, false, "retry skipped: 5s of the 300s timeout_sec budget left after the first attempt (floor 10s)"},
		{"configured floor above the ask's wall, half left", 400, 100 * time.Second, true, ""},
		{"configured floor above the ask's wall, under half left", 400, 160 * time.Second, false, "retry skipped: 140s of the 300s timeout_sec budget left after the first attempt (floor 150s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fleet, _ := retryRig(t, tc.retryMinSec, tc.spent, citing(t, "node-b"))
			out := askRetryOnce(t, s, "what is the queue cap")
			if (fleet.calls == 1) != tc.wantRetry {
				t.Fatalf("fleet calls = %d, want retry=%v: %v", fleet.calls, tc.wantRetry, out)
			}
			if tc.wantRetry {
				return
			}
			if out["verified"] != false || out["retried_on"] != nil {
				t.Fatalf("a skipped retry leaves the first answer, with no retried_on: %v", out)
			}
			if note, _ := out["retry_note"].(string); note != tc.wantNote {
				t.Fatalf("retry_note = %q, want %q", note, tc.wantNote)
			}
		})
	}
}

// The fleet's own engine may already have retried inside the call (a failed verification on its
// first node). The ask's note is the published one and keeps the fleet's story after it.
func TestOffloadAskKeepsTheFleetsOwnRetryNote(t *testing.T) {
	inner := "first attempt on node-b failed_verification: [x]; this result is the retry"
	answer := func(c core.AgentContract) delegate.PlacedResult {
		pr := citing(t, "node-c")(c)
		pr.RetriedOn, pr.RetryNote = "node-c", inner
		return pr
	}
	s, _, _ := retryRig(t, 0, 20*time.Second, answer)
	out := askRetryOnce(t, s, "what is the queue cap")

	note, _ := out["retry_note"].(string)
	if out["retried_on"] != "node-c" || !strings.HasPrefix(note, "first attempt on local-seat failed_verification: [") || !strings.HasSuffix(note, "the fleet's own retry: "+inner) {
		t.Fatalf("retried_on = %v, retry_note = %q", out["retried_on"], note)
	}
}

// The retry the delegator already ran inside a routed call was dropped before it reached any door:
// a fleet result that carries one surfaces both fields, and one that did not retry carries neither.
func TestAFleetResultSurfacesTheRetryTheDelegatorRan(t *testing.T) {
	cfg := config.Default()
	cfg.Home = t.TempDir()
	cfg.LedgerPath = filepath.Join(cfg.Home, "ledger.jsonl")
	s := New(pipeline.New(cfg, nil, nil, nil))
	var placed delegate.PlacedResult
	s.reviewFleet = func(context.Context, config.Config, delegate.LocalRunner, []core.AgentContract, string, []string, *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		return []delegate.PlacedResult{placed}, delegate.Summary{}, nil
	}
	wire := core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "ok", Seat: "seat-b"}

	placed = delegate.PlacedResult{Node: "node-b", Result: wire, RetriedOn: "node-b", RetryNote: "first attempt on node-a failed_verification: [x]; this result is the retry"}
	_, extra, _, ok := s.contractOnFleet(context.Background(), core.AgentContract{Goal: "g"}, "remote", "")
	if !ok || extra["retried_on"] != "node-b" || extra["retry_note"] != placed.RetryNote {
		t.Fatalf("ok=%v, a retried result must surface both fields: %v", ok, extra)
	}

	placed = delegate.PlacedResult{Node: "node-b", Result: wire}
	_, extra, _, ok = s.contractOnFleet(context.Background(), core.AgentContract{Goal: "g"}, "remote", "")
	if !ok {
		t.Fatalf("a clean result is accepted: %v", extra)
	}
	for _, k := range []string{"retried_on", "retry_note"} {
		if _, has := extra[k]; has {
			t.Fatalf("%s must be absent when the delegator ran no retry: %v", k, extra)
		}
	}
}
