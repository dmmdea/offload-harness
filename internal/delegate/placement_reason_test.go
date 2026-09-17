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
		f.seatRate = map[string]any{"tok_s": 5.4, "cold_load_sec": 69.0, "samples": 4, "min_turn_sec": 1594}
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

	tooSmall := NodeView{NodeID: "too-small", AgentEnabled: true, AgentCtxTokens: 100}
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
