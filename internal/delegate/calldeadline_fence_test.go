package delegate

// The retry_note of a retry that found no node to run on, when the local seat is fenced (D-94) and the
// call's deadline (ADR 0065) has passed.
//
// A fenced local seat is read from the lease record, so that part of the note stays true whatever the
// clock says. "No other node is eligible" is a claim about the fleet, and the fleet read that would have
// named a node is the very thing the deadline ends: once the clock has run out the note names the
// deadline first and keeps only the fence behind it. It used to be the other way round, and a seat-down
// retry (ADR 0066) whose read the deadline cut was told that no node could take it.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// TestAFencedLocalSeatDoesNotOutrankTheCallDeadlineInTheRetryNote: the first attempt answers with a
// seat-down defer in time, so a re-placement on another node is owed. The local seat is fenced, and the
// fleet's health turns slow once the first attempt has answered, so the deadline ends the read that would
// have named a node. The note leads with the deadline, names the fence behind it, and says nothing of the
// fleet it never read.
func TestAFencedLocalSeatDoesNotOutrankTheCallDeadlineInTheRetryNote(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	script := &firstJobScript{}
	var slow atomic.Bool
	tune := func(id string) func(*fakeNode) {
		return func(f *fakeNode) {
			inner := script.poll(t, id, seatDownWire(id, 6), runningForever)
			f.pollByJob = func(jobID string, n int64) (map[string]any, int) {
				slow.Store(true) // the fleet's health turns slow once the first attempt has answered
				return inner(jobID, n)
			}
			f.healthDelayFn = func() time.Duration {
				if slow.Load() {
					return 30 * time.Second
				}
				return 0
			}
		}
	}
	_, urlA := acceptingNode(t, "node-a", "unused", tune("node-a"))
	_, urlB := acceptingNode(t, "node-b", "unused", tune("node-b"))
	cfg := testCfg(t)
	fenceTheLocalSeat(t, &cfg)

	results, sum, _ := runWithin(t, 15*time.Second, cfg, neverLocal(t),
		[]core.AgentContract{plainContract()}, "remote", []string{urlA, urlB}, deadlineIn(1500*time.Millisecond), nil)
	pr := results[0]

	if !SeatDownDefer(pr.Result) || pr.deadlineCut || pr.retried || sum.Deferred != 1 {
		t.Fatalf("summary %+v cut %v retried %v reason %q, want the seat-down defer published as it was, with no retry (premise)", sum, pr.deadlineCut, pr.retried, pr.Result.Reason)
	}
	lease := LocalLease(cfg.GPULockPath, cfg.StateDir)
	_, fence := Fenced(lease)
	want := "retry skipped: " + callDeadlinePrefix + " before a retry node was chosen; the local seat is fenced (" + fence + " — " + HolderLine(lease) +
		"), and a retry placed there would wait out agent_lease_wait_sec at the affinity cordon and defer as capacity anyway"
	if pr.RetryNote != want {
		t.Fatalf("retry_note = %q, want %q", pr.RetryNote, want)
	}
	if strings.Contains(pr.RetryNote, "no other node is eligible") {
		t.Fatalf("retry_note = %q accuses nodes the deadline kept the delegator from asking", pr.RetryNote)
	}
}

// TestTheFenceNoteWithoutADeadlineIsWhatItAlwaysWas: the same note when the clock has not run out is
// unchanged, byte for byte. The first attempt runs remote and fails verification, the only other seat is
// the fenced local one, and the read of the fleet really did find nobody.
func TestTheFenceNoteWithoutADeadlineIsWhatItAlwaysWas(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	_, urlA := eligibleNode(t, "node-a", "wrong answer")
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "weights A/B", Draining: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), contracts(1), "spread", []string{urlA})
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if sum.Retried != 0 || pr.RetriedOn != "" {
		t.Fatalf("retried=%d retried_on=%q, want nothing left to retry on (premise)", sum.Retried, pr.RetriedOn)
	}
	lease := LocalLease(cfg.GPULockPath, cfg.StateDir)
	_, fence := Fenced(lease)
	want := "retry skipped: the local seat is fenced (" + fence + " — " + HolderLine(lease) +
		") and no other node is eligible; a retry placed there would wait out agent_lease_wait_sec at the affinity cordon and defer as capacity anyway"
	if pr.RetryNote != want {
		t.Fatalf("retry_note = %q, want %q", pr.RetryNote, want)
	}
}
