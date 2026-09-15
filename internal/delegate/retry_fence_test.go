package delegate

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// Register D-94, live 2026-09-14 18:3x. A contract deferred on the Lenovo 4B
// (output_truncated) and the cross-seat retry was placed on the LOCAL Qube seat
// whose lease was EXCLUSIVE — held by a measurement that had drained the cards
// for itself. The retry dialled that seat anyway, sat at the model-affinity
// cordon for the full `gpu-lease timeout after 5m0s (bound 5m0s)`, and then
// deferred as capacity, while the Aorus node was eligible and idle the whole
// time. Retry note as recorded:
//
//	retry on Qube also deferred (capacity): gpu busy: gpu-lease timeout after 5m0s (bound 5m0s)
//
// Three things were wrong and only the third is about waiting: the retry chose a
// seat that could not admit it, it chose that seat over an idle remote, and it
// discovered the refusal by DIALLING rather than by reading the verdict that was
// already on disk.

// holdFence takes a TEXT lease with the given fence stamp in a fresh dir and
// returns the dir for cfg.GPULockPath — the same resolver the runner reads, so
// the test cannot fence a lease the code under test never sees.
func holdFence(t *testing.T, opts gpulease.Options) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "lease")
	m, err := gpulease.OpenAt(dir, "")
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	if opts.TTL == 0 {
		opts.TTL = time.Minute
	}
	lease, err := m.TryAcquire(gpulease.ClassText, opts)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	return dir
}

// TestFencedNamesTheHoldsThatRefuseANewRun pins the predicate itself against the
// admission rule it has to agree with (modelaffinity.BlocksNewRun): exclusive or
// draining fences, a plain reservation does not (it steers placement, and the
// affinity gate admits the load), and an idle lease fences nothing.
func TestFencedNamesTheHoldsThatRefuseANewRun(t *testing.T) {
	cases := []struct {
		name  string
		info  gpulease.Info
		want  bool
		names string
	}{
		{"zero info", gpulease.Info{}, false, ""},
		{"plain text hold", gpulease.Info{Held: true, Class: gpulease.ClassText}, false, ""},
		{"exclusive text hold", gpulease.Info{Held: true, Class: gpulease.ClassText, Exclusive: true}, true, "exclusive"},
		{"draining text hold", gpulease.Info{Held: true, Class: gpulease.ClassText, Draining: true}, true, "draining"},
		{"media hold", gpulease.Info{Held: true, Class: gpulease.ClassMedia}, true, "media"},
		{"released exclusive", gpulease.Info{Held: false, Class: gpulease.ClassText, Exclusive: true}, false, ""},
	}
	for _, c := range cases {
		got, why := Fenced(c.info)
		if got != c.want {
			t.Errorf("%s: Fenced = %v, want %v", c.name, got, c.want)
		}
		if got && !strings.Contains(why, c.names) {
			t.Errorf("%s: the fence must NAME itself, got %q, want it to contain %q", c.name, why, c.names)
		}
	}
}

// TestRetryPrefersAnIdleRemoteOverAFencedLocalSeat: the D-94 shape. The first
// attempt runs remote and comes back unacceptable; the local seat is fenced by an
// exclusive lease; an idle remote is eligible. The retry must land on the remote.
// neverLocal makes "landed on the fenced seat" a test failure rather than an
// assertion about a note.
func TestRetryPrefersAnIdleRemoteOverAFencedLocalSeat(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "wrong answer") // fails acceptance -> retryable
	nodeB, urlB := eligibleNode(t, "node-b", "qube from B")  // idle, passes acceptance
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "5070 bench", Exclusive: true})

	results, sum, err := Run(context.Background(), cfg, neverLocal(t), contracts(1), "spread", []string{urlA, urlB})
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if nodeA.dispatches.Load() != 1 {
		t.Fatalf("the first attempt must have run on node-a: dispatches=%d", nodeA.dispatches.Load())
	}
	if nodeB.dispatches.Load() != 1 || pr.RetriedOn != "node-b" || sum.Retried != 1 {
		t.Fatalf("the retry must land on the idle remote, not the fenced local seat: node-b dispatches=%d retried_on=%q retried=%d note=%q",
			nodeB.dispatches.Load(), pr.RetriedOn, sum.Retried, pr.RetryNote)
	}
	if !strings.Contains(pr.PlacementReason, "fenced") || !strings.Contains(pr.PlacementReason, "exclusive") {
		t.Errorf("the placement reason must say WHY the retry skipped local, got %q", pr.PlacementReason)
	}
}

// TestRetryDefersAtOnceWhenOnlyAFencedSeatIsLeft: with the fenced local seat the
// only thing left, the retry must be refused HERE, from the lease record, and the
// note must name the fence. It must not dial the seat and discover the refusal at
// the cordon five minutes later — so the run is also bounded in wall clock.
func TestRetryDefersAtOnceWhenOnlyAFencedSeatIsLeft(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	nodeA, urlA := eligibleNode(t, "node-a", "wrong answer")
	cfg := testCfg(t)
	cfg.GPULockPath = holdFence(t, gpulease.Options{Reason: "weights A/B", Draining: true})

	start := time.Now()
	results, sum, err := Run(context.Background(), cfg, neverLocal(t), contracts(1), "spread", []string{urlA})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if sum.Retried != 0 || pr.RetriedOn != "" || nodeA.dispatches.Load() != 1 {
		t.Fatalf("nothing was left to retry on: retried=%d retried_on=%q node-a dispatches=%d", sum.Retried, pr.RetriedOn, nodeA.dispatches.Load())
	}
	if !strings.Contains(pr.RetryNote, "fenced") || !strings.Contains(pr.RetryNote, "draining") {
		t.Fatalf("retry_note must name the fence that refused the seat, got %q", pr.RetryNote)
	}
	if !strings.Contains(pr.RetryNote, "gpu lease") {
		t.Errorf("retry_note must name the HOLDER, not just the state: %q", pr.RetryNote)
	}
	// The cordon wait is 5 minutes. Anything near it means the retry dialled the
	// seat instead of reading the verdict.
	if elapsed > 30*time.Second {
		t.Fatalf("the retry took %s — it waited at the cordon instead of reading the lease", elapsed.Round(time.Second))
	}
}

// TestRetryStillFallsToAnUnfencedLocalSeat is the control the fence must not
// break: with NO lease held, a remote first attempt still retries on the local
// seat, exactly as before.
func TestRetryStillFallsToAnUnfencedLocalSeat(t *testing.T) {
	compressPolls(t, 10*time.Millisecond, 2*time.Second)
	_, urlA := eligibleNode(t, "node-a", "wrong answer")
	var localCalls atomic.Int64
	results, sum, err := Run(context.Background(), testCfg(t), passingLocal(&localCalls), contracts(2), "spread", []string{urlA})
	if err != nil {
		t.Fatal(err)
	}
	// Two subtasks, local + node-a: node-a's answer fails acceptance and its
	// retry is the one that must still be allowed to land on the local seat.
	if sum.Retried != 1 {
		t.Fatalf("an unfenced local seat must still take the retry: retried=%d results=%+v", sum.Retried, results)
	}
	if localCalls.Load() != 2 {
		t.Fatalf("local calls = %d, want 2 (its own subtask plus the retry)", localCalls.Load())
	}
}
