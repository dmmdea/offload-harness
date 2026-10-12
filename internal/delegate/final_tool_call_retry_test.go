package delegate

// A run that produced NOTHING (a seat that went down under it, a workspace root that was lost, a forced final step the seat
// answered with a tool call even after it was re-asked) is retried on another node, and that retry is not "an optional second
// opinion on an answer the subtask already holds": declining it, or waiting out the placement wait and then declining it,
// loses the job. So it is placed the way the seat-down defer's second placement is (ADR 0066 decision 3): the busy check of
// the retry seat neither waits nor skips, the first seat's own min_turn is not the retry seat's floor, and a local line with
// no free slot is not joined while an untried remote could take it. The forced-final tool-call defer and the lost-root defer
// each had one of these three and not the others.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// nothingProduced names the defers whose second placement is the job's only way to be delivered, each as the wire a node files.
var nothingProduced = map[string]func(node string) core.AgentWireResult{
	"forced-final tool call": finalToolCallWire,
	"lost root":              rootGoneWire,
	"seat down":              func(node string) core.AgentWireResult { return seatDownWire(node, 6) },
}

// The other refusal a saturated fleet makes normal: the only alternative is at its ceiling (4 of 4 running, 4 queued). Busy is a
// place in line, never a refusal (INV-4): the node's own queue is the line.
func TestANothingProducedRetryIsNotHeldForABusyRetrySeat(t *testing.T) {
	for name, wire := range nothingProduced {
		t.Run(name, func(t *testing.T) {
			compressPolls(t, 10*time.Millisecond, 2*time.Second)
			nodeA, urlA := eligibleNode(t, "node-a", "unused")
			nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) { return doneWire(t, wire("node-a")), 200 }
			nodeB, urlB := eligibleNode(t, "node-b", "the answer from B")
			nodeB.maxConcurrentJobs, nodeB.jobsRunning, nodeB.queueDepth = 4, 4, 4
			contract := remoteContract()
			contract.Acceptance = []string{"nonempty:answer"}
			cfg := testCfg(t)
			cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

			results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "spread", []string{urlA, urlB})
			if err != nil {
				t.Fatal(err)
			}
			if sum.Retried != 1 || results[0].Result.Deferred || strings.Contains(results[0].RetryNote, "already running another job") {
				t.Fatalf("retried=%d deferred=%v dispatches A=%d B=%d note=%q, want the retry placed on the busy node, not skipped for it",
					sum.Retried, results[0].Result.Deferred, nodeA.dispatches.Load(), nodeB.dispatches.Load(), results[0].RetryNote)
			}
		})
	}
}

// The same with the placement wait ON: the retry does not stand in line for the retry seat for the wait's whole TTL first.
func TestANothingProducedRetryDoesNotWaitOutThePlacementWait(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) { return doneWire(t, finalToolCallWire("node-a")), 200 }
	nodeB, urlB := eligibleNode(t, "node-b", "the answer from B")
	nodeB.maxConcurrentJobs, nodeB.jobsRunning, nodeB.queueDepth = 4, 4, 4
	contract := remoteContract()
	contract.Acceptance = []string{"nonempty:answer"}
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 5
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	start := time.Now()
	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Retried != 1 || results[0].Result.Deferred {
		t.Fatalf("retried=%d deferred=%v note=%q after %s", sum.Retried, results[0].Result.Deferred, results[0].RetryNote, time.Since(start).Round(time.Millisecond))
	}
	if strings.Contains(results[0].RetryNote, "in line") {
		t.Fatalf("note %q: the retry stood in line for a busy retry seat", results[0].RetryNote)
	}
}

// The first seat's number is not the retry seat's: a seat whose cold load and two long turns measure 1,112 s wrote the tool
// call, the retry goes to a node that publishes no rate, and the budget (300 s) is no reason to refuse the second chance.
func TestANothingProducedRetryIsNotRefusedForTheFailedSeatsMinTurn(t *testing.T) {
	for name, wire := range nothingProduced {
		t.Run(name, func(t *testing.T) {
			compressPolls(t, 10*time.Millisecond, 2*time.Second)
			nodeA, urlA := eligibleNode(t, "node-a", "unused")
			nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) {
				w := wire("node-a")
				w.MinTurnSec = 1112
				return doneWire(t, w), 200
			}
			nodeB, urlB := eligibleNode(t, "node-b", "the answer from B") // publishes no seat rate
			contract := remoteContract()
			contract.TimeoutSec = 300
			contract.Acceptance = []string{"nonempty:answer"}
			cfg := testCfg(t)
			cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

			results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{contract}, "spread", []string{urlA, urlB})
			if err != nil {
				t.Fatal(err)
			}
			if sum.Retried != 1 || results[0].Result.Deferred {
				t.Fatalf("retried=%d deferred=%v dispatches A=%d B=%d note=%q, want the retry placed although the failed seat measures 1,112 s",
					sum.Retried, results[0].Result.Deferred, nodeA.dispatches.Load(), nodeB.dispatches.Load(), results[0].RetryNote)
			}
		})
	}
}

// A verification retry keeps all three: it is a second opinion on an answer the subtask holds, and the busy seat check and the
// failed seat's floor stay where they were (ADR 0063 decision 10, D-46).
func TestAVerificationRetryStillKeepsTheBusyCheckAndTheFailedSeatsFloor(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "unused")
	nodeA.pollByJob = func(jobID string, n int64) (map[string]any, int) { return doneWire(t, wrongAnswerWire("node-a", 0)), 200 }
	nodeB, urlB := eligibleNode(t, "node-b", "zorblax from B")
	nodeB.maxConcurrentJobs, nodeB.jobsRunning, nodeB.queueDepth = 4, 4, 4
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), []core.AgentContract{remoteContract()}, "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Retried != 0 || !strings.Contains(results[0].RetryNote, "already running another job") {
		t.Fatalf("retried=%d note=%q, want a verification retry skipped for a busy retry seat as before", sum.Retried, results[0].RetryNote)
	}
}

// A defer that produced nothing leaves a local seat whose run-cap line has no free slot for an untried remote (a line the
// subtask cannot leave once it joins it), exactly as the seat-down defer's second placement does.
func TestANothingProducedRetryLeavesAFullLocalLineForAnUntriedRemote(t *testing.T) {
	for name, wire := range nothingProduced {
		t.Run(name, func(t *testing.T) {
			compressPolls(t, 10*time.Millisecond, 2*time.Second)
			nodeA, urlA := eligibleNode(t, "node-a", "unused")
			nodeB, urlB := eligibleNode(t, "node-b", "unused")
			scriptFirstJob(t, wire, "the answer from the second node", nodeA, nodeB)
			cfg := testCfg(t)
			cfg.FleetMaxConcurrentJobs = 1
			occupyTheLocalSeat(t, cfg)
			var localCalls atomic.Int64
			contract := remoteContract()
			contract.Acceptance = []string{"nonempty:answer"}

			results, sum, err := Run(context.Background(), cfg, passingLocal(&localCalls), []core.AgentContract{contract}, "spread", []string{urlA, urlB})
			if err != nil {
				t.Fatal(err)
			}
			if nodeA.dispatches.Load()+nodeB.dispatches.Load() == 0 {
				t.Fatalf("fixture: the first attempt ran on the local seat (local calls %d)", localCalls.Load())
			}
			if localCalls.Load() != 0 {
				t.Fatalf("the retry joined the local seat's full run-cap line (local calls %d) while an untried remote sat idle; note=%q", localCalls.Load(), results[0].RetryNote)
			}
			if sum.Retried != 1 || results[0].Result.Deferred {
				t.Fatalf("retried=%d deferred=%v note=%q reason=%q", sum.Retried, results[0].Result.Deferred, results[0].RetryNote, results[0].Result.Reason)
			}
		})
	}
}
