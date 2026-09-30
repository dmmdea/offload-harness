// retry_after_test.go: a dispatch 503 is a refusal that returns AT ONCE.
//
// Until ADR 0063 the delegator slept the refusing node's Retry-After inside
// runRemote (up to the 300 s the node clamps it to), retried that same node once,
// and charged the sleep to the contract's budget: on 2026-09-29 that was 232
// refusals and 16 caller-hours asleep on nodes that had just said no, while other
// nodes had room. Now the refusal re-places the subtask on a node with room at
// once, and the hint becomes a per-node cooldown that only the capacity wait
// consumes — the wait re-reads health every few seconds, credits the time it
// spends, and asks the node again when its cooldown ends.

package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestRun503WithAnotherNodeHavingRoomNeverSleeps: node A answers 503 with a
// long Retry-After, node B has room. B must be dispatched inside 10 % of the
// hint — before this change runRemote slept on A first (bounded only by the
// contract's budget) and retried A, and B was reached after the sleep.
func TestRun503WithAnotherNodeHavingRoomNeverSleeps(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWallUnit(t, 200*time.Millisecond) // a Retry-After of 60 "seconds" is 12 s of test time
	a, aURL := refusingNode(t, "node-a", http.StatusServiceUnavailable, func(f *fakeNode) { f.dispatchRetryAfter = "60" })
	var bFirst atomic.Int64
	b, bURL := acceptingNode(t, "node-b", "answer from b", func(f *fakeNode) {
		f.onDispatch = func(string, core.AgentContract) { bFirst.CompareAndSwap(0, time.Now().UnixNano()) }
	})

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	started := time.Now()
	results, sum, err := Run(ctx, testCfg(t), neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{aURL, bURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	hint := 12 * time.Second
	if bFirst.Load() == 0 {
		t.Fatalf("node-b was never dispatched; summary = %+v", sum)
	}
	if got := time.Duration(bFirst.Load() - started.UnixNano()); got > hint/10 {
		t.Fatalf("node-b was first dispatched %s after the start; want inside 10%% of the %s hint — the delegator slept on the node that refused it", got.Round(time.Millisecond), hint)
	}
	if sum.Succeeded != 1 || results[0].Node != "node-b" {
		t.Fatalf("summary = %+v node = %q, want the subtask done on node-b", sum, results[0].Node)
	}
	if got := a.dispatches.Load(); got != 1 {
		t.Fatalf("node-a saw %d dispatches, want exactly 1 — a 503 is not retried on the node that sent it", got)
	}
	if b.dispatches.Load() != 1 {
		t.Fatalf("node-b saw %d dispatches, want 1", b.dispatches.Load())
	}
}

// TestRun503RetryAfterAtOrAboveTheQueueDeadlineRePlacesInstead: a Retry-After
// at or above the delegator's own queue ceiling (maxQueuedWait, 300 s: the
// node clamps its hint there and says ">=300 s") is a backlog the delegator
// will not wait out, so nothing sleeps on the way to the other node. The
// discriminator is the delegator's constant compared with the header's number,
// never the node's prose.
func TestRun503RetryAfterAtOrAboveTheQueueDeadlineRePlacesInstead(t *testing.T) {
	for _, hint := range []string{"300", "900"} {
		t.Run("Retry-After "+hint, func(t *testing.T) {
			compressPolls(t, 5*time.Millisecond, time.Second)
			compressWallUnit(t, 100*time.Millisecond)
			a, aURL := refusingNode(t, "node-a", http.StatusServiceUnavailable, func(f *fakeNode) { f.dispatchRetryAfter = hint })
			var bFirst atomic.Int64
			_, bURL := acceptingNode(t, "node-b", "answer from b", func(f *fakeNode) {
				f.onDispatch = func(string, core.AgentContract) { bFirst.CompareAndSwap(0, time.Now().UnixNano()) }
			})
			contract := plainContract()
			contract.TimeoutSec = 600 // a budget far above the hint: only the sleep itself can delay node-b

			ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
			defer cancel()
			started := time.Now()
			_, sum, err := Run(ctx, testCfg(t), neverLocal(t), []core.AgentContract{contract}, "remote", []string{aURL, bURL})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if bFirst.Load() == 0 {
				t.Fatalf("node-b was never dispatched; summary = %+v", sum)
			}
			if got := time.Duration(bFirst.Load() - started.UnixNano()); got > 2*time.Second {
				t.Fatalf("node-b was first dispatched %s after the start — a Retry-After of %s s slept before the re-placement", got.Round(time.Millisecond), hint)
			}
			if a.dispatches.Load() != 1 || sum.Succeeded != 1 {
				t.Fatalf("node-a saw %d dispatches, summary = %+v, want 1 refusal and one success on node-b", a.dispatches.Load(), sum)
			}
		})
	}
}

// TestRun503RetryAfterIsCreditedNotCharged: the ONLY node refuses with a short
// Retry-After. The delegator waits it out — through the capacity wait, which
// credits the time it idles — and the subtask then keeps its whole execution
// budget. The old courtesy sleep sat inside runRemote, was never credited, and
// so shrank what every later placement and the verification retry were offered.
func TestRun503RetryAfterIsCreditedNotCharged(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := acceptingNode(t, "node-only", "answer after the hint", func(f *fakeNode) {
		f.dispatchHook = freesAfter(1, http.StatusServiceUnavailable)
		f.dispatchRetryAfter = "3"
	})
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10
	r := &runner{cfg: cfg, route: "remote", remotes: []string{url}, local: neverLocal(t)}
	pl := newPlacements()
	const budget = 30
	start := time.Now()
	pr := r.placeAndRun(t.Context(), 0, plainContract(), nil, start, budget, pl)
	if pr.Err != "" || pr.Result.Deferred {
		t.Fatalf("result = err %q deferred %v (%s), want the work done once the hint elapsed", pr.Err, pr.Result.Deferred, pr.Result.Reason)
	}
	if got := node.dispatches.Load(); got != 2 {
		t.Fatalf("node saw %d dispatches, want 2 (the refusal, then the ask after its cooldown)", got)
	}
	if elapsed := time.Since(start); elapsed < 2200*time.Millisecond {
		t.Fatalf("done after %s — the 3 s Retry-After (jittered +/-20%%) was not honoured", elapsed)
	}
	if pl.credit < 2200*time.Millisecond {
		t.Fatalf("credited %s of the wait, want about the 3 s hint — the sleep was charged to the budget", pl.credit)
	}
	if rem := pl.remaining(start, budget); rem < budget-2 {
		t.Fatalf("%d s of the %d s budget left after honouring a 3 s hint, want >= %d — the wait was charged", rem, budget, budget-2)
	}
}

// TestRun503RetryAfterLandsOnTheSameNode: with nowhere else to go the job still
// lands on the node that refused, after the hint, and the result says it waited.
func TestRun503RetryAfterLandsOnTheSameNode(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	dispatches, url := retryAfterNode(t)
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 10

	start := time.Now()
	results, sum, err := RunWith(context.Background(), cfg, neverLocal(t), []core.AgentContract{plainContract()}, "remote", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 700*time.Millisecond {
		t.Fatalf("elapsed = %s, want the Retry-After: 1 hint to have been honoured (>= ~0.8 s)", elapsed)
	}
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v, want the second ask to have succeeded", sum)
	}
	pr := results[0]
	if pr.Node != "node-retry-after" {
		t.Fatalf("Node = %q, want node-retry-after (the SAME node that refused)", pr.Node)
	}
	if !pr.waited || pr.CapacityWaitSec < 0.7 {
		t.Fatalf("waited = %v capacity_wait_sec = %.2f, want the hint reported as a capacity wait", pr.waited, pr.CapacityWaitSec)
	}
	if !strings.Contains(pr.PlacementReason, "capacity wait") {
		t.Fatalf("PlacementReason = %q, want it to say the subtask waited for capacity", pr.PlacementReason)
	}
	if !strings.Contains(pr.ReplacementNote, "503") {
		t.Fatalf("ReplacementNote = %q, want the refusal named", pr.ReplacementNote)
	}
	if dispatches.Load() != 2 {
		t.Fatalf("dispatch count = %d, want exactly 2 (the refusal, then the ask after the hint)", dispatches.Load())
	}
}

// TestNoteCooldownCapsAHintAtTheQueueCeiling: a node's Retry-After becomes a
// cooldown of that many seconds (jittered +/-20 %), a hint above the delegator's
// own queue ceiling is capped there (the node clamps its own at 300 s and says
// ">=300 s"), a refusal with no hint cools the node for refusalCooldown, and only a
// capacity refusal cools anything.
func TestNoteCooldownCapsAHintAtTheQueueCeiling(t *testing.T) {
	base := "http://192.0.2.7:18811"
	cooldown := func(pr PlacedResult) time.Duration {
		r := &runner{}
		r.noteCooldown(pr)
		until, held := r.cool.heldUntil(base, time.Now())
		if !held {
			return 0
		}
		return time.Until(until)
	}
	refusal := func(status, hint int) PlacedResult {
		return PlacedResult{refused: true, refusalStatus: status, retryAfterSec: hint, ranBase: base}
	}
	within := func(got, want time.Duration) bool {
		return got >= time.Duration(float64(want)*0.75) && got <= time.Duration(float64(want)*1.25)
	}
	if got := cooldown(refusal(503, 30)); !within(got, 30*time.Second) {
		t.Errorf("cooldown for a 30 s hint = %s, want about 30 s", got)
	}
	if got := cooldown(refusal(503, 900)); !within(got, maxQueuedWait) {
		t.Errorf("cooldown for a 900 s hint = %s, want it capped at the %s queue ceiling", got, maxQueuedWait)
	}
	if got := cooldown(refusal(503, 0)); !within(got, refusalCooldown) {
		t.Errorf("cooldown for a refusal with no hint = %s, want refusalCooldown (%s)", got, refusalCooldown)
	}
	if got := cooldown(refusal(409, 30)); got != 0 {
		t.Errorf("a non-capacity refusal (409) cooled the node for %s, want no cooldown", got)
	}
	if got := cooldown(PlacedResult{refused: true, refusalStatus: 503, retryAfterSec: 30}); got != 0 {
		t.Errorf("a refusal that names no dial base cooled something for %s", got)
	}
}

// retryAfterNode serves /fleet/health (agent-enabled, resident), refuses the
// FIRST /fleet/dispatch with 503 + Retry-After: 1, accepts every dispatch
// after that, and answers every poll "done" at once.
func retryAfterNode(t *testing.T) (dispatches *atomic.Int64, url string) {
	t.Helper()
	var n atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"node_id": "node-retry-after", "agent_enabled": true, "agent_seat": "remote-seat",
			"agent_seat_resident": true, "agent_ctx_tokens": 32768, "queue_depth": 0,
		})
	})
	mux.HandleFunc("POST /fleet/dispatch", func(w http.ResponseWriter, r *http.Request) {
		var env struct {
			JobID string `json:"job_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&env)
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": "queue full"})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": env.JobID, "status": "accepted"})
	})
	mux.HandleFunc("GET /fleet/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		wire := core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "node-retry-after", Seat: "remote-seat",
			Output: "answer from retry-after", Structured: json.RawMessage(`{"answer":"answer"}`), StopReason: "done"}
		data, _ := json.Marshal(wire)
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "done", "data": json.RawMessage(data), "job_id": r.PathValue("id")})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &n, srv.URL
}

// TestPollOnceSendsTheWaitParameter pins the wire shape: pollOnce's GET
// carries ?wait=<pollWaitSec>.
func TestPollOnceSendsTheWaitParameter(t *testing.T) {
	var sawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "running"})
	}))
	defer srv.Close()
	r := &runner{cfg: testCfg(t)}
	if _, err := r.pollOnce(context.Background(), srv.URL, "job-1"); err != nil {
		t.Fatal(err)
	}
	if sawQuery != "wait=12" {
		t.Fatalf("poll query = %q, want wait=12", sawQuery)
	}
}
