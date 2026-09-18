package delegate

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The two clocks (register D-116). Until this build the delegator polled every
// timeout_auto contract at the wire CAP, although the node sized its own wall
// from its measured seat rate and publishes the numbers that sizing is made of
// on health. A node that acked and then died silently was therefore abandoned
// 900 s + grace later even when its own wall had been 300 s.

// compressWallUnit shrinks the unit a contract's wall (an integer number of
// SECONDS) is converted to wall clock with, and restores it. Needed because an
// auto contract's bound never falls below AgentTimeoutSecDefault: without it a
// deadline test would wait five real minutes for the cheapest case.
func compressWallUnit(t *testing.T, unit time.Duration) {
	t.Helper()
	old := pollSecond
	pollSecond = unit
	t.Cleanup(func() { pollSecond = old })
}

// autoContract is remoteContract() as the intake stamps one the caller left
// unsized: the wire default on the wall, the timeout_auto marker beside it.
func autoContract() core.AgentContract {
	c := remoteContract()
	c.MaxSteps = 8
	c.TimeoutSec, c.TimeoutAuto = core.AgentTimeoutSecDefault, true
	return c
}

// ratedView is a node view advertising seat_rate and seat_budget, the two
// blocks a node publishes together on health since 0.117.2.
func ratedView(id string, tokS, coldLoadSec float64, samples, stepTokens int, thinking string) NodeView {
	return NodeView{
		NodeID:     id,
		AgentSeat:  "remote-seat",
		SeatRate:   &SeatRateView{TokS: tokS, ColdLoadSec: coldLoadSec, Samples: samples},
		SeatBudget: &SeatBudgetView{StepTokens: stepTokens, FinalTokens: 4 * stepTokens, Thinking: thinking},
	}
}

// TestAutoPollBoundIsTheSizedWallNotTheCap (D-116, (a)): a view advertising a
// real rate sizes the bound from the SAME arithmetic the node sizes its wall
// with — cold load + one think block + the tool steps + the final answer + the
// re-pack a contract with an output_schema may pay — and the note names the
// source. The exact number is asserted against the node's own function in
// autopoll_drift_test.go; here the terms are pinned by hand so a silent change
// to any ONE of them is visible.
func TestAutoPollBoundIsTheSizedWallNotTheCap(t *testing.T) {
	view := ratedView("node-a", 20, 30, 5, 1024, "auto")
	bound, note := autoPollBound(view, autoContract(), "")
	// 30 cold + 1024/20 think + 7 x (128/20 + 6) steps + 4096/20 final
	// + 4096/20 re-pack = 577.6 -> 578 s, inside [300, 900].
	if want := 578 * time.Second; bound != want {
		t.Fatalf("bound = %s, want %s", bound, want)
	}
	if bound >= time.Duration(core.AgentTimeoutSecCap)*time.Second {
		t.Fatalf("bound = %s: the whole point is that it is UNDER the cap", bound)
	}
	for _, want := range []string{"node-a", "seat_rate", "20.0 tok/s", "5 samples", "578 s"} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to name %q", note, want)
		}
	}
}

// TestAutoPollBoundClampsToTheWireBounds: the bound is an estimate of the
// node's wall, and the node clamps its wall to [default, cap] — so this must
// clamp identically at both ends. A very fast seat cannot buy a 30 s poll
// window, and a very slow one cannot hold a delegation past the cap.
func TestAutoPollBoundClampsToTheWireBounds(t *testing.T) {
	fast, note := autoPollBound(ratedView("fast", 2000, 1, 9, 1024, "auto"), autoContract(), "")
	if want := time.Duration(core.AgentTimeoutSecDefault) * time.Second; fast != want {
		t.Errorf("fast seat: bound = %s, want the wire default %s (note %q)", fast, want, note)
	}
	slow, note := autoPollBound(ratedView("slow", 0.5, 120, 9, 4096, "on"), autoContract(), "")
	if want := time.Duration(core.AgentTimeoutSecCap) * time.Second; slow != want {
		t.Errorf("slow seat: bound = %s, want the cap %s (note %q)", slow, want, note)
	}
}

// TestAutoPollBoundWithoutARateIsTheCap (D-116, (b)): a node that advertises
// no usable rate has told the delegator nothing to bound by, so the cap stands
// — today's behaviour — and the note says WHY, in the wording the poll-deadline
// message quotes.
func TestAutoPollBoundWithoutARateIsTheCap(t *testing.T) {
	capBound := time.Duration(core.AgentTimeoutSecCap) * time.Second
	cases := []struct {
		name string
		view NodeView
	}{
		{"no seat_rate block at all", NodeView{NodeID: "old-node", AgentSeat: "remote-seat"}},
		{"seat_rate with no tok/s", NodeView{NodeID: "old-node", SeatRate: &SeatRateView{TokS: 0, Samples: 4}}},
		{"seat_rate with no samples", NodeView{NodeID: "old-node", SeatRate: &SeatRateView{TokS: 25, Samples: 0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bound, note := autoPollBound(tc.view, autoContract(), "")
			if bound != capBound {
				t.Fatalf("bound = %s, want the cap %s", bound, capBound)
			}
			if !strings.Contains(note, "cap: no seat rate advertised") {
				t.Fatalf("note = %q, want it to say the cap was used for want of a rate", note)
			}
		})
	}
}

// TestAutoPollBoundSizesAtHouseDefaultsWhenOnlyTheRateIsPublished: a node
// publishing seat_rate without seat_budget is sized at the house defaults, and
// the note SAYS so rather than implying the budgets were read.
func TestAutoPollBoundSizesAtHouseDefaultsWhenOnlyTheRateIsPublished(t *testing.T) {
	view := NodeView{NodeID: "rate-only", AgentSeat: "remote-seat", SeatRate: &SeatRateView{TokS: 20, ColdLoadSec: 30, Samples: 5}}
	bound, note := autoPollBound(view, autoContract(), "")
	if want := 578 * time.Second; bound != want {
		t.Fatalf("bound = %s, want %s (the 1024-token house default)", bound, want)
	}
	if !strings.Contains(note, "no seat_budget published") {
		t.Fatalf("note = %q, want it to say the budgets were defaulted", note)
	}
}

// TestExplicitTimeoutContractIsNeverAutoBounded (D-116, (c)): a contract that
// named its own timeout_sec is the caller's number and the node runs exactly
// it — no sizing, no note, and the poll budget stays timeout_sec + grace.
func TestExplicitTimeoutContractIsNeverAutoBounded(t *testing.T) {
	explicit := remoteContract() // TimeoutSec 30, TimeoutAuto false
	bound, note := autoPollBound(ratedView("node-a", 20, 30, 5, 1024, "auto"), explicit, "")
	if bound != 0 || note != "" {
		t.Fatalf("bound/note = %s/%q for an explicit contract, want 0 and no note", bound, note)
	}

	// End to end: the poll deadline of an explicit contract must read exactly
	// as it did before D-116 — no "poll bound" clause anywhere.
	compressPolls(t, 10*time.Millisecond, 100*time.Millisecond)
	// No seat_rate/seat_budget published here (unlike the unit-level check
	// above): with a rate published, W-05's feasibility gate (fit.go,
	// feasibleFinal) correctly refuses this node at PLACEMENT for the 1 s
	// explicit wall below — that gate is exercised on its own in
	// feasibility_test.go. This test's job is the D-116 poll-bound wiring
	// (wall_sec ignored, no bound clause), which needs the dispatch to
	// actually land and then time out; an unpublished rate is "no opinion"
	// (eligible), keeping this test's scope unchanged.
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		pollState: func(int64) (map[string]any, int) {
			// It also publishes a wall — an explicit contract must ignore it.
			return map[string]any{"state": "running", "wall_sec": 800}, http.StatusOK
		},
	}
	srv := node.server()
	c := remoteContract()
	c.TimeoutSec = 1
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{c}, "remote", []string{srv.URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Deferred: 1}) {
		t.Fatalf("summary = %+v, want exactly one defer", sum)
	}
	r := results[0]
	if !strings.HasPrefix(r.Result.Reason, "poll deadline") || strings.Contains(r.Result.Reason, "poll bound") {
		t.Fatalf("reason = %q, want the pre-D-116 poll-deadline wording with no bound clause", r.Result.Reason)
	}
	if r.PollNote != "" {
		t.Fatalf("poll note = %q, want none on an explicit contract", r.PollNote)
	}
}

// TestAutoContractIsAbandonedAtTheSizedBoundNotTheCap (D-116, (d)): a node
// that acks an auto contract and then answers `running` forever is abandoned
// at the bound its OWN advertisement implies — not 900 s later. The wall unit
// is compressed so the 300 s floor is testable; the arithmetic is real.
func TestAutoContractIsAbandonedAtTheSizedBoundNotTheCap(t *testing.T) {
	compressPolls(t, 2*time.Millisecond, 50*time.Millisecond)
	compressWallUnit(t, time.Millisecond)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		// A fast seat: the estimate lands under the floor, so the bound is the
		// wire default 300 — a THIRD of the cap, which is the whole claim.
		seatRate:   map[string]any{"tok_s": 200.0, "cold_load_sec": 1.0, "samples": 6, "min_turn_sec": 22},
		seatBudget: map[string]any{"step_tokens": 1024, "final_tokens": 4096, "thinking": "auto"},
		pollState: func(int64) (map[string]any, int) {
			return map[string]any{"state": "running"}, http.StatusOK
		},
	}
	srv := node.server()

	start := time.Now()
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{autoContract()}, "remote", []string{srv.URL})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Deferred: 1}) {
		t.Fatalf("summary = %+v, want exactly one defer", sum)
	}
	bound := time.Duration(core.AgentTimeoutSecDefault) * time.Millisecond // the compressed 300 s
	capBudget := time.Duration(core.AgentTimeoutSecCap)*time.Millisecond + 50*time.Millisecond
	if elapsed < bound {
		t.Fatalf("abandoned after %s, before the sized bound %s — the delegator cut the node short", elapsed, bound)
	}
	if elapsed >= capBudget {
		t.Fatalf("abandoned after %s: that is the CAP budget %s, not the sized bound %s", elapsed, capBudget, bound)
	}
	r := results[0]
	if !r.Result.Deferred || !strings.HasPrefix(r.Result.Reason, "poll deadline") {
		t.Fatalf("deferred/reason = %v/%q, want the poll-deadline defer", r.Result.Deferred, r.Result.Reason)
	}
}

// TestPollDeadlineNamesTheSourceOfTheBound (D-116, (f)): the message an
// operator reads must say WHERE the number came from, in all three shapes.
func TestPollDeadlineNamesTheSourceOfTheBound(t *testing.T) {
	t.Run("sized from the node's seat rate", func(t *testing.T) {
		compressPolls(t, 2*time.Millisecond, 20*time.Millisecond)
		compressWallUnit(t, time.Millisecond)
		node := &fakeNode{
			t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
			seatRate:   map[string]any{"tok_s": 200.0, "cold_load_sec": 1.0, "samples": 6, "min_turn_sec": 22},
			seatBudget: map[string]any{"step_tokens": 1024, "final_tokens": 4096, "thinking": "auto"},
			pollState:  func(int64) (map[string]any, int) { return map[string]any{"state": "running"}, http.StatusOK },
		}
		results, _, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{autoContract()}, "remote", []string{node.server().URL})
		if err != nil {
			t.Fatal(err)
		}
		want := "poll bound: sized from fake-node's seat_rate 200.0 tok/s (6 samples): 300 s"
		if !strings.Contains(results[0].Result.Reason, want) {
			t.Fatalf("reason = %q, want it to contain %q", results[0].Result.Reason, want)
		}
	})

	t.Run("the cap, for want of a rate", func(t *testing.T) {
		compressPolls(t, 2*time.Millisecond, 20*time.Millisecond)
		compressWallUnit(t, time.Millisecond)
		node := &fakeNode{
			t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "old-node",
			pollState: func(int64) (map[string]any, int) { return map[string]any{"state": "running"}, http.StatusOK },
		}
		results, _, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{autoContract()}, "remote", []string{node.server().URL})
		if err != nil {
			t.Fatal(err)
		}
		if want := "poll bound: cap: no seat rate advertised by old-node"; !strings.Contains(results[0].Result.Reason, want) {
			t.Fatalf("reason = %q, want it to contain %q", results[0].Result.Reason, want)
		}
	})

	t.Run("a node that never owned the job: the FAILURE names it too", func(t *testing.T) {
		compressPolls(t, 2*time.Millisecond, 20*time.Millisecond)
		compressWallUnit(t, time.Millisecond)
		node := &fakeNode{
			t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
			seatRate:   map[string]any{"tok_s": 200.0, "cold_load_sec": 1.0, "samples": 6, "min_turn_sec": 22},
			seatBudget: map[string]any{"step_tokens": 1024, "final_tokens": 4096, "thinking": "auto"},
			pollState: func(int64) (map[string]any, int) {
				return map[string]any{"status": "error", "error": "vram snapshot stale"}, http.StatusServiceUnavailable
			},
		}
		results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{autoContract()}, "remote", []string{node.server().URL})
		if err != nil {
			t.Fatal(err)
		}
		if sum != (Summary{Failed: 1}) {
			t.Fatalf("summary = %+v, want a failure — no answer ever owned the job", sum)
		}
		if want := "poll bound: sized from fake-node's seat_rate"; !strings.Contains(results[0].Err, want) {
			t.Fatalf("err = %q, want it to contain %q", results[0].Err, want)
		}
	})
}

// TestTheNodesOwnWallRaisesThePollBound (D-116, (e)): the node's wall is
// authoritative. When a RUNNING job publishes a wall larger than the
// delegator's estimate, the delegator must adopt it — abandoning a job the
// node is still running inside its own wall is the one thing this change must
// never do.
func TestTheNodesOwnWallRaisesThePollBound(t *testing.T) {
	compressPolls(t, 2*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, time.Millisecond)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		// The estimate clamps to the 300 s floor; the node says it is running
		// under 800 s. 800 wins.
		seatRate:   map[string]any{"tok_s": 200.0, "cold_load_sec": 1.0, "samples": 6, "min_turn_sec": 22},
		seatBudget: map[string]any{"step_tokens": 1024, "final_tokens": 4096, "thinking": "auto"},
		pollState: func(int64) (map[string]any, int) {
			return map[string]any{"state": "running", "wall_sec": 800}, http.StatusOK
		},
	}
	srv := node.server()

	start := time.Now()
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{autoContract()}, "remote", []string{srv.URL})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Deferred: 1}) {
		t.Fatalf("summary = %+v, want exactly one defer", sum)
	}
	sized := time.Duration(core.AgentTimeoutSecDefault) * time.Millisecond
	if elapsed <= sized+20*time.Millisecond {
		t.Fatalf("abandoned after %s: the node published an 800 s wall and the delegator cut it at its own %s estimate", elapsed, sized)
	}
	r := results[0]
	if want := "poll bound: the node's own wall 800 s"; !strings.Contains(r.Result.Reason, want) {
		t.Fatalf("reason = %q, want it to name %q", r.Result.Reason, want)
	}
	// The note names the wall AND where it was measured from: the node's own
	// wall start, which is the instant the delegator's clock is anchored on.
	if r.PollNote != "the node's own wall 800 s, from where the node started it" {
		t.Fatalf("poll note = %q, want the node's wall named", r.PollNote)
	}
}

// TestNodeWallNeverLowersThePollBound: the adoption is a RAISE only. A node
// publishing a wall shorter than the delegator's bound must not shrink the
// clock — the delegator's bound already holds the node's own arithmetic, and a
// short wall on the wire is not a licence to abandon early.
func TestNodeWallNeverLowersThePollBound(t *testing.T) {
	for _, wallSec := range []int{0, 30, 300} {
		if got, note, ok := raisedPollBound(600*time.Second, wallSec); ok || got != 600*time.Second || note != "" {
			t.Errorf("wall_sec %d: raisedPollBound = %s/%q/%v, want the bound untouched", wallSec, got, note, ok)
		}
	}
	got, note, ok := raisedPollBound(400*time.Second, 5000)
	if !ok || got != time.Duration(core.AgentTimeoutSecCap)*time.Second+pollGrace {
		t.Errorf("a nonsense wall must clamp to the cap: %s/%q/%v", got, note, ok)
	}
}

// TestWirePublishesThePollNote (D-116, (g)): the bound the delegator used
// rides the published result as `poll_note` for an auto contract, and is
// ABSENT for a contract that named its own timeout_sec — a pre-D-116 result
// marshals byte-identically.
func TestWirePublishesThePollNote(t *testing.T) {
	auto := WireResponse([]PlacedResult{{Node: "n", PollNote: "sized from node-a's seat_rate 20.0 tok/s (5 samples): 578 s"}}, Summary{Succeeded: 1}, nil)
	blob, err := json.Marshal(auto)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"poll_note":"sized from node-a's seat_rate 20.0 tok/s (5 samples): 578 s"`) {
		t.Fatalf("published JSON carries no poll_note: %s", blob)
	}
	if auto.Results[0].PollNote == "" {
		t.Fatal("ResultWire.PollNote is empty for an auto contract")
	}
	clean, err := json.Marshal(WireResponse([]PlacedResult{{Node: "n"}}, Summary{Succeeded: 1}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(clean), "poll_note") {
		t.Fatalf("an explicit contract's result must publish no poll_note: %s", clean)
	}
}

// TestAutoPollNoteRidesASuccessfulResultToo: the note is not a failure
// artefact — a caller reading a green result must still be able to see what
// clock its unsized contract was held to.
func TestAutoPollNoteRidesASuccessfulResultToo(t *testing.T) {
	compressPolls(t, 2*time.Millisecond, 20*time.Millisecond)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		seatRate:   map[string]any{"tok_s": 20.0, "cold_load_sec": 30.0, "samples": 5, "min_turn_sec": 235},
		seatBudget: map[string]any{"step_tokens": 1024, "final_tokens": 4096, "thinking": "auto"},
		pollByJob: func(string, int64) (map[string]any, int) {
			return doneWire(t, remoteWire("the qube answer", `{"answer":"42"}`)), http.StatusOK
		},
	}
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{autoContract()}, "remote", []string{node.server().URL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 {
		t.Fatalf("summary = %+v, want a success", sum)
	}
	if want := "sized from fake-node's seat_rate 20.0 tok/s (5 samples): 578 s"; results[0].PollNote != want {
		t.Fatalf("poll note = %q, want %q", results[0].PollNote, want)
	}
}
