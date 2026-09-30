package delegate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// researchRetryContract is a schema contract whose acceptance reads the word the
// fake nodes' answers do (or do not) contain.
func researchRetryContract() core.AgentContract {
	c := remoteContract()
	c.Acceptance = []string{"contains:answer", "nonempty:answer"}
	return c
}

// A research page that fails only its acceptance is not run a second time on
// another node (register C-74, PR-3). The retry is a whole second run of five to
// thirteen minutes, and for a research digest the checks are graded against the
// page: 209 of the week's 271 failed_verification rows were false failures
// (page chrome as an anchor, an empty list as a shape failure), which a second
// seat only repeats. Any other door keeps the retry: the 27B and the 4B each
// missing a different contract is what it was built for.
func TestRetryableSkipsAcceptanceOnlyFailuresForResearch(t *testing.T) {
	for _, door := range []string{"offload_research", "cli:research"} {
		t.Run(door, func(t *testing.T) {
			compressPolls(t, 10*time.Millisecond, 2*time.Second)
			nodeA, urlA := eligibleNode(t, "node-a", "wrong reply")       // fails acceptance
			nodeB, urlB := eligibleNode(t, "node-b", "the answer from B") // would pass it
			c := researchRetryContract()
			c.Door = door
			cfg := testCfg(t)
			cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

			results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{c}, "spread", []string{urlA, urlB})
			if err != nil {
				t.Fatal(err)
			}
			if sum.Retried != 0 || nodeA.dispatches.Load()+nodeB.dispatches.Load() != 1 {
				t.Fatalf("retried=%d dispatches=%d+%d, want the page run exactly once", sum.Retried, nodeA.dispatches.Load(), nodeB.dispatches.Load())
			}
			r := results[0]
			if len(r.AcceptanceFailures) == 0 || sum.FailedVerification != 1 {
				t.Fatalf("failures=%v failed_verification=%d, want the verified failure published as it is", r.AcceptanceFailures, sum.FailedVerification)
			}
			if !strings.Contains(r.RetryNote, "retry skipped") || !strings.Contains(r.RetryNote, "research") {
				t.Fatalf("retry_note = %q, want the skip and its reason named", r.RetryNote)
			}
		})
	}
}

// The guard on the other side, so the skip cannot widen: the same failure from
// any other door still goes to a second node and is recovered there.
func TestAcceptanceFailureFromAnotherDoorIsStillRetried(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	_, urlA := eligibleNode(t, "node-a", "wrong reply")
	_, urlB := eligibleNode(t, "node-b", "the answer from B")
	c := researchRetryContract()
	c.Door = "agent_delegate"
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{c}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Retried != 1 || sum.RetryRecovered != 1 || len(results[0].AcceptanceFailures) != 0 {
		t.Fatalf("retried=%d recovered=%d failures=%v note=%q, want the retry to run and recover", sum.Retried, sum.RetryRecovered, results[0].AcceptanceFailures, results[0].RetryNote)
	}
}

// An honest abstention on a research page still goes to a second node: only an
// acceptance-only failure is skipped. (The seat did not answer; another may.)
func TestResearchAbstentionIsStillRetried(t *testing.T) {
	first := PlacedResult{Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassAbstention}}
	c := researchRetryContract()
	c.Door = "offload_research"
	if !retryable(first) || skipsRetryAsResearchAcceptanceOnly(c, first) {
		t.Fatalf("an abstention on a research page must stay retryable (retryable=%v skip=%v)", retryable(first), skipsRetryAsResearchAcceptanceOnly(c, first))
	}
	failed := PlacedResult{AcceptanceFailures: []string{"regex: pattern did not match output"}}
	if !skipsRetryAsResearchAcceptanceOnly(c, failed) {
		t.Fatal("an acceptance-only failure on a research page must be skipped")
	}
	c.Door = "agent_delegate"
	if skipsRetryAsResearchAcceptanceOnly(c, failed) {
		t.Fatal("the skip must be research-only")
	}
}

// A failed DOCUMENT FINGERPRINT is not the checks' own fault: the answer is about
// another document, which is a fact about the node that wrote it (a node is
// quarantined after two of them), and a second node given the same page is the
// cure. It keeps its one retry on the research door, alone or beside any other
// failed check.
func TestResearchDocumentFingerprintFailureIsStillRetried(t *testing.T) {
	c := researchRetryContract()
	c.Door = "offload_research"
	off := "regex:(?i)(?P<docanchor>pinned|staging) not found in output"
	for name, failures := range map[string][]string{
		"alone":          {off},
		"beside a shape": {"min_items:key_facts:1: field key_facts has 0 items, want >= 1", off},
	} {
		pr := PlacedResult{AcceptanceFailures: failures}
		if skipsRetryAsResearchAcceptanceOnly(c, pr) || !retryable(pr) {
			t.Fatalf("%s: a failed document fingerprint must keep its retry (skip=%v retryable=%v)", name, skipsRetryAsResearchAcceptanceOnly(c, pr), retryable(pr))
		}
	}
	shapeOnly := PlacedResult{AcceptanceFailures: []string{"min_items:key_facts:1: field key_facts has 0 items, want >= 1"}}
	if !skipsRetryAsResearchAcceptanceOnly(c, shapeOnly) {
		t.Fatal("a shape failure alone must still be skipped")
	}
}

// Through the engine: the first node answers about the wrong document, the second
// would answer about the right one, and the research page goes to both.
func TestResearchOffDocumentAnswerIsRecoveredOnAnotherNode(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	_, urlA := eligibleNode(t, "node-a", "wrong reply")
	_, urlB := eligibleNode(t, "node-b", "the answer from B")
	c := researchRetryContract()
	c.Door = "offload_research"
	c.Acceptance = []string{"regex:(?i)(?P<docanchor>answer|verdict)"}
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{c}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Retried != 1 || sum.RetryRecovered != 1 || len(results[0].AcceptanceFailures) != 0 {
		t.Fatalf("retried=%d recovered=%d failures=%v note=%q, want the off-document answer re-run on the other node and recovered", sum.Retried, sum.RetryRecovered, results[0].AcceptanceFailures, results[0].RetryNote)
	}
}
