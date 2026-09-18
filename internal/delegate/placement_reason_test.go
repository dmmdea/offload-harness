// placement_reason_test.go: D-105 (PR-5 item 8) — placement_reason names
// EVERY reachable remote with a one-word verdict.

package delegate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestRunPlacementReasonNamesEveryRemoteWithAVerdict: three remotes with
// three different outcomes (chosen, slow, cap) — the published
// placement_reason must name all three, each with its own word.
func TestRunPlacementReasonNamesEveryRemoteWithAVerdict(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)

	_, chosenURL := acceptingNode(t, "node-chosen", "qube from chosen", nil)

	slow, slowURL := acceptingNode(t, "node-slow", "qube from slow (must not be dispatched)", func(f *fakeNode) {
		// 0.3 tok/s: one tool step and a 64-token answer need ~640 s, so a 300 s
		// wall cannot hold ANY answer — the rider's refusal, not a slow-but-able
		// seat (5.4 tok/s answers this class in ~40 s and is a ranking matter).
		f.seatRate = map[string]any{"tok_s": 0.3, "cold_load_sec": 69.0, "samples": 4, "min_turn_sec": 1594}
		f.seatBudget = map[string]any{"step_tokens": 8192, "thinking": "off"}
	})

	full, capURL := acceptingNode(t, "node-cap", "qube from cap (must not be dispatched)", func(f *fakeNode) {
		f.maxConcurrentJobs = 1
		f.jobsRunning = 1
		f.queueDepth = 1
	})

	contract := oneStepSchemaContract(300, false).Contract
	results, sum, err := Run(context.Background(), testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{chosenURL, slowURL, capURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v, want the one eligible node to have run it", sum)
	}
	if slow.dispatches.Load() != 0 || full.dispatches.Load() != 0 {
		t.Fatalf("only node-chosen must ever be dispatched to: slow=%d cap=%d", slow.dispatches.Load(), full.dispatches.Load())
	}
	pr := results[0]
	if pr.Node != "node-chosen" {
		t.Fatalf("Node = %q, want node-chosen", pr.Node)
	}
	for _, want := range []string{"node-chosen: chosen", "node-slow: slow", "node-cap: cap"} {
		if !strings.Contains(pr.PlacementReason, want) {
			t.Errorf("PlacementReason = %q, want it to contain %q", pr.PlacementReason, want)
		}
	}
	// The existing route=remote prefix stays byte-identical — fleet_smoke_cmd.go parses it.
	if !strings.HasPrefix(pr.PlacementReason, "route=remote → node-chosen") {
		t.Fatalf("PlacementReason = %q, want the route=remote prefix preserved", pr.PlacementReason)
	}
}

// TestOneWordVerdictVocabulary pins the vocabulary directly against
// synthetic NodeViews, independent of a full Run.
func TestOneWordVerdictVocabulary(t *testing.T) {
	st := oneStepSchemaContract(300, false)

	leased := eligibleRemote()
	leased.NodeID, leased.LeaseExclusive = "leased", true
	if got := oneWordVerdict(st, leased, "b-leased", "b-chosen", 0); !strings.HasPrefix(got, "lease") {
		t.Errorf("leased verdict = %q, want it to start with lease", got)
	}

	disabled := eligibleRemote()
	disabled.NodeID, disabled.AgentEnabled = "disabled", false
	if got := oneWordVerdict(st, disabled, "b-disabled", "b-chosen", 0); !strings.HasPrefix(got, "probe") {
		t.Errorf("disabled verdict = %q, want it to start with probe", got)
	}

	// AgentResident: true isolates the ctx-size check — eligibilityVerdict now
	// checks residency BEFORE adequate() (gate order), so an unresident
	// fixture would read "probe", not "unfit(ctx)".
	tooSmall := NodeView{NodeID: "too-small", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 100}
	if got := oneWordVerdict(st, tooSmall, "b-small", "b-chosen", 0); !strings.HasPrefix(got, "unfit(ctx)") {
		t.Errorf("too-small verdict = %q, want it to start with unfit(ctx)", got)
	}

	saturatedNode := eligibleRemote()
	saturatedNode.NodeID, saturatedNode.QueueDepth, saturatedNode.MaxQueueDepth = "saturated", 5, 5
	if got := oneWordVerdict(st, saturatedNode, "b-sat", "b-chosen", 0); !strings.HasPrefix(got, "queue") {
		t.Errorf("saturated verdict = %q, want it to start with queue", got)
	}

	capped := eligibleRemote()
	capped.NodeID, capped.MaxConcurrentJobs, capped.JobsRunning = "capped", 4, 4
	if got := oneWordVerdict(st, capped, "b-cap", "b-chosen", 0); !strings.HasPrefix(got, "cap") {
		t.Errorf("capped verdict = %q, want it to start with cap", got)
	}

	outranked := eligibleRemote()
	outranked.NodeID = "outranked"
	if got := oneWordVerdict(st, outranked, "b-outranked", "b-chosen", 0); !strings.HasPrefix(got, "cold") {
		t.Errorf("outranked verdict = %q, want it to start with cold", got)
	}

	winner := eligibleRemote()
	winner.NodeID = "winner"
	if got := oneWordVerdict(st, winner, "b-chosen", "b-chosen", 0); !strings.HasPrefix(got, "chosen") {
		t.Errorf("winner verdict = %q, want it to start with chosen", got)
	}
}

// TestOneWordVerdictAgreesWithTheGateOnSchemaAndDepth (review round 1,
// BLOCKER item 1): oneWordVerdict must run the EXACT SAME predicate
// sequence remoteEligible does (gate.go's eligibilityVerdict), so a
// schema-less contract is narrated "noschema" on every node — never "slow"
// or "unfit(ctx)" from a feasibility/adequacy check the gate never reached
// — and a non-origin contract is narrated its own word.
func TestOneWordVerdictAgreesWithTheGateOnSchemaAndDepth(t *testing.T) {
	noSchema := Subtask{
		// TimeoutSec explicit and nonzero: without it feasibleFinal reads "no
		// opinion" (wallSec<=0) regardless of order, and this test would not
		// actually distinguish the old (broken) order from the fixed one.
		Contract:  core.AgentContract{SchemaVersion: core.AgentWireSchemaVersion, Goal: "summarize", Depth: 0, TimeoutSec: 300},
		EstTokens: 1000,
	}
	// Two remotes of very different rate/window — under the PRE-FIX order
	// (feasibility/adequacy checked before schema), the slow one would read
	// "slow (one step and a 64-token answer ...)" instead of "noschema", contradicting the
	// gate's own "no schema at all" refusal, which never even reaches
	// feasibility.
	fast := eligibleRemote()
	fast.NodeID, fast.AgentCtxTokens = "fast", 131072
	fast.SeatRate = &SeatRateView{TokS: 34, ColdLoadSec: 15, Samples: 5}
	slow := eligibleRemote()
	slow.NodeID, slow.AgentCtxTokens = "slow", 8192
	slow.SeatRate = &SeatRateView{TokS: 5.4, ColdLoadSec: 69, Samples: 4}
	for _, v := range []NodeView{fast, slow} {
		if got := oneWordVerdict(noSchema, v, v.NodeID, "b-chosen", 0); got != "noschema" {
			t.Errorf("%s verdict for a schema-less contract = %q, want exactly \"noschema\" (remoteEligible refuses on schema BEFORE feasibility/adequacy)", v.NodeID, got)
		}
		if remoteEligible(noSchema, v) {
			t.Errorf("%s: remoteEligible must also refuse a schema-less contract", v.NodeID)
		}
	}

	hop := Subtask{
		Contract:  core.AgentContract{SchemaVersion: core.AgentWireSchemaVersion, Goal: "summarize", OutputSchema: gateSchema, Depth: 1},
		EstTokens: 1000,
	}
	if got := oneWordVerdict(hop, fast, "fast", "b-chosen", 0); !strings.HasPrefix(got, "hop") {
		t.Errorf("depth!=0 verdict = %q, want it to start with hop", got)
	}
	if remoteEligible(hop, fast) {
		t.Fatal("remoteEligible must refuse a non-origin (depth != 0) contract")
	}
}
