package mcpserver

import (
	"context"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/delegate"
)

// A finished answer whose structured re-pack was not made (skipped because the time
// left could not buy it, cut by the clock, stalled) is not lost work (register C-80,
// C-66): the node flags the result schema_miss and keeps the answer in output. The
// doors that publish a bare defer used to drop it, and the re-pack's wall bound
// added a new way in: a loop that ends past the wall and its grace now files a budget
// defer without sending a request. These tests pin what each door does with it.

// schemaMissWire is the wire result of that shape.
func schemaMissWire(output string) core.AgentWireResult {
	return core.AgentWireResult{
		SchemaVersion: core.AgentWireSchemaVersion,
		Seat:          "fake-seat",
		Steps:         2,
		StopReason:    "done",
		Output:        output,
		Deferred:      true,
		DeferClass:    core.DeferClassBudget,
		SchemaMiss:    true,
		Reason:        "structured re-pack skipped: 0 s left to the wall + 30 s grace at 5.6 tok/s buys 0 tokens < the answer's 91",
	}
}

// offload_ask keeps the finished answer in a deferred result, flagged, beside the
// defer's own reason and class: the caller is told the structured pair is missing and
// is handed the prose, never graded and never presented as an answer.
func TestAskDeferredSchemaMissCarriesTheFinishedAnswer(t *testing.T) {
	dir, p := askFixture(t)
	s := askTestServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		return schemaMissWire("the cap is 32"), nil
	})
	res, err := s.handleAsk(context.Background(), callReq(askArgs("what is the queue cap", p, dir)))
	if err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true || m["defer_class"] != core.DeferClassBudget {
		t.Fatalf("a schema-miss defer must stay a defer with its class: %v", m)
	}
	if m["output"] != "the cap is 32" || m["schema_miss"] != true {
		t.Fatalf("the finished answer must ride the defer, flagged: %v", m)
	}
	if _, published := m["answer"]; published {
		t.Fatalf("a defer must not publish an answer field: %v", m)
	}
}

// A defer with no finished answer to keep (the seat was unreachable) adds nothing: the
// existing shape is untouched.
func TestAskDeferWithNoFinishedAnswerAddsNothing(t *testing.T) {
	dir, p := askFixture(t)
	s := askTestServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Seat: "fake-seat", Deferred: true,
			Reason: "endpoint unreachable", DeferClass: core.DeferClassInfrastructure}, nil
	})
	m := decodeRouteResult(t, mustAsk(t, s, askArgs("what is the queue cap", p, dir)))
	if _, has := m["output"]; has {
		t.Fatalf("a defer that kept no answer must not publish an output key: %v", m)
	}
	if _, has := m["schema_miss"]; has {
		t.Fatalf("a defer that is not a schema miss must not publish the flag: %v", m)
	}
}

// The same door through the fleet: contractOnFleet reports a deferred result as "the
// fleet took nothing" and used to hand back only its reason.
func TestAskRoutedDeferredSchemaMissCarriesTheFinishedAnswer(t *testing.T) {
	dir, p := askFixture(t)
	s := routeServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		t.Fatal("a routed ask must not run the local seat")
		return core.AgentWireResult{}, nil
	})
	s.reviewFleet = func(context.Context, config.Config, delegate.LocalRunner, []core.AgentContract, string, []string, *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		return []delegate.PlacedResult{{Node: "node-b", Seat: "seat-b", Result: schemaMissWire("the cap is 32")}}, delegate.Summary{Deferred: 1}, nil
	}
	m := decodeRouteResult(t, mustAsk(t, s, `{"question":"what is the queue cap","paths":[`+jsonString(p)+`],"read_root":`+jsonString(dir)+`,"route":"remote"}`))
	if m["deferred"] != true || m["output"] != "the cap is 32" || m["schema_miss"] != true || m["route"] != "remote" {
		t.Fatalf("a routed schema-miss defer must keep the finished answer and the route: %v", m)
	}
}

// offload_review_diff does NOT publish it: the lane's whole value is that what it
// publishes went through the grounding and dedupe filters, and the loop's raw prose
// has not. A defer there stays bare.
func TestReviewDeferredSchemaMissDoesNotPublishTheUnfilteredProse(t *testing.T) {
	s := routeServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		return schemaMissWire("major | nowhere.go:9 | an invented finding the filters would drop | never grounded"), nil
	})
	res, err := s.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{"diff": reviewDiff, "task": "iterate over every element exactly once"})))
	if err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	m := decodeResult(t, res)
	if m["deferred"] != true {
		t.Fatalf("a review whose re-pack failed must defer: %v", m)
	}
	if _, has := m["output"]; has {
		t.Fatalf("the review lane must not publish the loop's unfiltered prose: %v", m)
	}
}

// contractOnFleet hands the delegator engine the rescue of a finished answer whose
// re-pack failed (register C-66), as the other delegator doors do, except under a
// fence: the review lane's fleet path exists because the LOCAL seat is held by someone
// else's lease, and the rescue is a completion on that seat.
func TestRoutedDoorsWireTheDelegatorRescueExceptUnderAFence(t *testing.T) {
	var gotOpts *delegate.RunOptions
	capture := func(_ context.Context, _ config.Config, _ delegate.LocalRunner, _ []core.AgentContract, _ string, _ []string, opts *delegate.RunOptions) ([]delegate.PlacedResult, delegate.Summary, error) {
		gotOpts = opts
		return []delegate.PlacedResult{{Node: "node-b", Seat: "seat-b", Result: core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, Output: "x", StopReason: "done"}}}, delegate.Summary{Succeeded: 1}, nil
	}

	dir, p := askFixture(t)
	s := routeServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{}, nil
	})
	s.reviewFleet = capture
	mustAsk(t, s, `{"question":"what is the queue cap","paths":[`+jsonString(p)+`],"read_root":`+jsonString(dir)+`,"route":"remote"}`)
	if gotOpts == nil || gotOpts.Rescue == nil {
		t.Fatalf("a routed offload_ask must hand the engine the rescue of a finished answer: opts = %+v", gotOpts)
	}

	gotOpts = nil
	fs, _ := fenceServer(t, func(context.Context, core.AgentContract, delegate.LocalOptions) (core.AgentWireResult, error) {
		t.Fatal("the local seat is fenced")
		return core.AgentWireResult{}, nil
	})
	fs.reviewFleet = capture
	if _, err := fs.handleReviewDiff(context.Background(), callReq(reviewArgs(t, map[string]any{"diff": reviewDiff, "task": "iterate over every element exactly once"}))); err != nil {
		t.Fatalf("handleReviewDiff: %v", err)
	}
	if gotOpts == nil {
		t.Fatal("the fenced review was not offered to the fleet")
	}
	if gotOpts.Rescue != nil {
		t.Fatal("a fenced review must not get a rescue: it would run a completion on the seat the fence protects")
	}
}

func mustAsk(t *testing.T, s *Server, args string) any {
	t.Helper()
	res, err := s.handleAsk(context.Background(), callReq(args))
	if err != nil {
		t.Fatalf("handleAsk: %v", err)
	}
	return res
}
