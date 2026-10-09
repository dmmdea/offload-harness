// pin_gate_race_test.go: the hint's no-wait seat fallback (hintSeatWithoutAWait, ADR 0078 decision 5) reads the seat free
// and then runs it, and the seat's run-cap line can fill in between (another Run of this process takes the last slot). The
// seat's admission then declines the subtask as capacity with nothing run (ADR 0063 decision 3), the answer the capacity
// wait files as a refusal and re-places. The fallback published it as the seat's result instead: a sheddable run read as a
// capacity defer of the local seat, with the seat as the place it ran, where the shed was the truth; and with the wait
// switched off the defer named the seat as having run it. capacityDeferLocal is the seat's admission answer here, so the
// fresh reads pass and the run finds the line full, which is the race's outcome exactly.

package delegate

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestAGatedRemoteHintIsShedWhenTheSeatsLineFillsBetweenTheReadAndTheRun: the sheddable exit. The result is the shed,
// carrying the seat's refusal; the seat ran nothing and the result does not say it did.
func TestAGatedRemoteHintIsShedWhenTheSeatsLineFillsBetweenTheReadAndTheRun(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	node, url := gatedNode(t)
	cfg := pinCfg(t)
	cfg.AgentPlacementWaitSec = 3
	var localCalls atomic.Int64
	opts, _ := countedOpts("")
	opts.Priority = core.BandSheddable
	results, sum, err := RunWith(t.Context(), cfg, capacityDeferLocal(&localCalls), contracts(1), "remote", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if localCalls.Load() != 1 || node.dispatches.Load() != 0 {
		t.Fatalf("local runs %d, node dispatches %d, want the seat asked once and the gated node never dispatched", localCalls.Load(), node.dispatches.Load())
	}
	if sum.Shed != 1 || !pr.shed || pr.ranLocal || sum.Succeeded != 0 {
		t.Fatalf("summary %+v, shed %v, ran local %v: want the shed, not the seat's capacity defer published as its result (placement %q)",
			sum, pr.shed, pr.ranLocal, pr.PlacementReason)
	}
	for _, want := range []string{"shed (priority -1)", "1 refusal(s)", "(local seat): deferred (capacity)", "at its local run cap"} {
		if !strings.Contains(pr.PlacementReason, want) {
			t.Errorf("placement = %q, want it to carry %q: the seat was asked and declined", pr.PlacementReason, want)
		}
	}
	if strings.Contains(pr.PlacementReason, "the idle local seat takes it instead") {
		t.Errorf("placement = %q claims the seat took the subtask; it declined it", pr.PlacementReason)
	}
	if pr.Replacements != 1 {
		t.Errorf("replacements = %d, want the seat's refusal counted", pr.Replacements)
	}
}

// TestAGatedRemoteHintDefersAsCapacityWhenTheSeatsLineFillsWithTheWaitOff: the operator's off switch. The outcome is the
// capacity defer the subtask had with nowhere to wait, with the seat's refusal on it, and not a defer that says the seat ran it.
func TestAGatedRemoteHintDefersAsCapacityWhenTheSeatsLineFillsWithTheWaitOff(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, 2*time.Second)
	node, url := gatedNode(t)
	cfg := pinCfg(t)
	cfg.AgentPlacementWaitSec = -1
	var localCalls atomic.Int64
	opts, _ := countedOpts("")
	results, sum, err := RunWith(t.Context(), cfg, capacityDeferLocal(&localCalls), contracts(1), "remote", []string{url}, opts)
	if err != nil {
		t.Fatal(err)
	}
	pr := results[0]
	if localCalls.Load() != 1 || node.dispatches.Load() != 0 {
		t.Fatalf("local runs %d, node dispatches %d, want the seat asked once and the gated node never dispatched", localCalls.Load(), node.dispatches.Load())
	}
	if sum.Deferred != 1 || !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassCapacity || pr.ranLocal {
		t.Fatalf("summary %+v, deferred %v class %q, ran local %v: want the capacity defer of a subtask with nowhere to wait, not the seat's (placement %q)",
			sum, pr.Result.Deferred, pr.Result.DeferClass, pr.ranLocal, pr.PlacementReason)
	}
	if !strings.HasPrefix(pr.Result.Reason, "capacity wait: no node had room") || !strings.Contains(pr.Result.Reason, "(local seat): deferred (capacity)") {
		t.Errorf("reason = %q, want the wait's defer with the seat's refusal in its chain", pr.Result.Reason)
	}
	if strings.Contains(pr.PlacementReason, "the idle local seat takes it instead") {
		t.Errorf("placement = %q claims the seat took the subtask; it declined it", pr.PlacementReason)
	}
}
