// autopoll_admission_test.go — THE OTHER HALF OF THE TWO CLOCKS (register
// D-116, review findings 1 and 2).
//
// autopoll_test.go pins the SIZE of the delegator's bound. This file pins the
// two things that size alone got wrong:
//
//   - WHERE the clock starts. The node stamps a job `running` the moment it
//     claims it, and everything before its wall — the cordon wait, the
//     llama-swap pre-flight, a vLLM seat's 125–250 s cold load, the coherence
//     probe, up to core.AgentAdmissionSecDefault — happens in that state and
//     earns no queued credit. A clock started at dispatch and sized at wall +
//     grace abandoned an auto contract on a COLD seat while the node was still
//     inside its own wall.
//   - WHICH SEAT it is sized from. On a composite node the delegator
//     dispatches a LAYER and the node sizes its wall from that layer's seat;
//     health advertises a rate for the agent seat alone.
package delegate

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestAutoPollBoundFallsBackToTheCapOnALayerSeat (review finding 2): a long
// layer seat is slower than the planner seat BY DESIGN, so sizing the
// delegator's clock from the advertised agent seat's rate is the same
// arithmetic over the wrong numbers — and the node's wall then outlives the
// bound by up to the whole default..cap span. The bound must fall back to the
// cap, and the note must name the seat nobody published a rate for.
func TestAutoPollBoundFallsBackToTheCapOnALayerSeat(t *testing.T) {
	view := ratedView("node-a", 20, 30, 5, 1024, "auto") // AgentSeat "remote-seat"
	capBound := time.Duration(core.AgentTimeoutSecCap) * time.Second

	bound, note := autoPollBound(view, autoContract(), "long-27b")
	if bound != capBound {
		t.Fatalf("bound = %s for a run on an unadvertised layer seat, want the cap %s (note %q)", bound, capBound, note)
	}
	for _, want := range []string{"cap:", "long-27b", "remote-seat"} {
		if !strings.Contains(note, want) {
			t.Errorf("note = %q, want it to name %q", note, want)
		}
	}

	// The decided seat IS the advertised one (the plain node, and a composite
	// decision that landed on the agent seat): sized exactly as before — the
	// fallback must not swallow every composite placement.
	same, note := autoPollBound(view, autoContract(), "remote-seat")
	if want := 578 * time.Second; same != want {
		t.Fatalf("bound = %s when the run seat IS the advertised seat, want the sized %s (note %q)", same, want, note)
	}
	none, note := autoPollBound(view, autoContract(), "")
	if none != same {
		t.Fatalf("bound = %s when no seat was decided, want the sized %s (note %q)", none, same, note)
	}
}

// TestAutoContractIsNotAbandonedDuringTheNodesAdmissionWindow (review finding
// 1): the node COMPLETES this contract inside its own wall, having spent 250
// units admitting it first. Pre-fix the delegator's whole clock was 300 + 50
// units from dispatch, so the answer below arrived at a delegation that had
// already published a manufactured "node accepted the job but did not reach a
// terminal state" defer.
func TestAutoContractIsNotAbandonedDuringTheNodesAdmissionWindow(t *testing.T) {
	compressPolls(t, 2*time.Millisecond, 50*time.Millisecond)
	compressWallUnit(t, time.Millisecond)
	const admission, work = 250 * time.Millisecond, 150 * time.Millisecond
	var mu sync.Mutex
	var firstPoll time.Time
	since := func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		if firstPoll.IsZero() {
			firstPoll = time.Now()
		}
		return time.Since(firstPoll)
	}
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		// A fast seat with NO cold load recorded — entirely ordinary under the
		// house 5-minute idle-unload rule, because a cold load is written only
		// when the warm-up actually waited for one. The estimate lands under
		// the floor, so the bound is the wire default 300.
		seatRate:   map[string]any{"tok_s": 200.0, "cold_load_sec": 0.0, "samples": 6, "min_turn_sec": 22},
		seatBudget: map[string]any{"step_tokens": 1024, "final_tokens": 4096, "thinking": "auto"},
		pollState: func(int64) (map[string]any, int) {
			switch e := since(); {
			case e < admission:
				// ADMITTING: the node has claimed the job (`running`) but its
				// wall has not started, so it publishes no wall_sec.
				return map[string]any{"state": "running"}, http.StatusOK
			case e < admission+work:
				return map[string]any{"state": "running", "wall_sec": core.AgentTimeoutSecDefault}, http.StatusOK
			default:
				return doneWire(t, remoteWire("the qube answer", `{"answer":"42"}`)), http.StatusOK
			}
		},
	}

	start := time.Now()
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{autoContract()}, "remote", []string{node.server().URL})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Succeeded: 1}) {
		t.Fatalf("summary = %+v, want the node's own answer (results[0] = %+v)", sum, results[0])
	}
	// The pass is only meaningful if the run really did outlive the clock that
	// used to bound it: the sized wall + grace, measured from dispatch.
	preFix := time.Duration(core.AgentTimeoutSecDefault)*time.Millisecond + pollGrace
	if elapsed <= preFix {
		t.Fatalf("the fixture finished in %s, inside the pre-fix bound %s — it cannot show the regression is closed", elapsed, preFix)
	}
	if want := "from where the node started it"; !strings.Contains(results[0].PollNote, want) {
		t.Fatalf("poll note = %q, want it to say the clock was anchored on the node's wall start", results[0].PollNote)
	}
}

// TestTheAnchoredClockDropsTheAdmissionAllowance: the allowance exists for a
// wall that has NOT started. The moment the node publishes one, the delegator
// knows exactly where its clock began, so the budget becomes the node's own
// wall plus transport slack FROM THAT INSTANT — D-116's tightening is kept,
// not traded away for the admission fix.
func TestTheAnchoredClockDropsTheAdmissionAllowance(t *testing.T) {
	compressPolls(t, 2*time.Millisecond, 50*time.Millisecond)
	compressWallUnit(t, time.Millisecond)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		seatRate:   map[string]any{"tok_s": 200.0, "cold_load_sec": 1.0, "samples": 6, "min_turn_sec": 22},
		seatBudget: map[string]any{"step_tokens": 1024, "final_tokens": 4096, "thinking": "auto"},
		// A warm seat: the wall starts at once — and never ends.
		pollState: func(int64) (map[string]any, int) {
			return map[string]any{"state": "running", "wall_sec": core.AgentTimeoutSecDefault}, http.StatusOK
		},
	}

	start := time.Now()
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{autoContract()}, "remote", []string{node.server().URL})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum != (Summary{Deferred: 1}) {
		t.Fatalf("summary = %+v, want exactly one defer", sum)
	}
	wall := time.Duration(core.AgentTimeoutSecDefault) * time.Millisecond
	if elapsed < wall {
		t.Fatalf("abandoned after %s, inside the node's own %s wall", elapsed, wall)
	}
	if withSlack := wall + admissionSlack(); elapsed >= withSlack {
		t.Fatalf("abandoned after %s: the admission allowance was still in the budget (%s) although the node had published a started wall", elapsed, withSlack)
	}
	if reason := results[0].Result.Reason; strings.Contains(reason, "allowed for the node's admission") {
		t.Fatalf("reason = %q, want no admission clause once the wall has started", reason)
	}
}

// TestPollDeadlineNamesTheAdmissionAllowance: an operator reading a deadline
// message must be able to tell the node's sized wall from the number the clock
// was actually held to — otherwise the allowance is invisible slack and the
// message under-reports the budget by five minutes.
func TestPollDeadlineNamesTheAdmissionAllowance(t *testing.T) {
	compressPolls(t, 2*time.Millisecond, 20*time.Millisecond)
	compressWallUnit(t, time.Millisecond)
	node := &fakeNode{
		t: t, agentEnabled: true, resident: true, ctxTokens: 8192, nodeID: "fake-node",
		seatRate:   map[string]any{"tok_s": 200.0, "cold_load_sec": 1.0, "samples": 6, "min_turn_sec": 22},
		seatBudget: map[string]any{"step_tokens": 1024, "final_tokens": 4096, "thinking": "auto"},
		// A node that acked and then went quiet: `running` forever, and never
		// a wall — so the allowance is still in force at the deadline.
		pollState: func(int64) (map[string]any, int) { return map[string]any{"state": "running"}, http.StatusOK },
	}
	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), []core.AgentContract{autoContract()}, "remote", []string{node.server().URL})
	if err != nil {
		t.Fatal(err)
	}
	if sum != (Summary{Deferred: 1}) {
		t.Fatalf("summary = %+v, want exactly one defer", sum)
	}
	reason := results[0].Result.Reason
	for _, want := range []string{
		"poll bound: sized from fake-node's seat_rate 200.0 tok/s (6 samples): 300 s",
		"allowed for the node's admission before its wall starts",
	} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason = %q, want it to contain %q", reason, want)
		}
	}
}
