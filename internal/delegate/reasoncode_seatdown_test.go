package delegate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// The two reasons of ADR 0066 reach the ledger as reason_code seat_down, built here
// from what the node REALLY writes (the lesson of reasoncode_texts_test.go): a
// seat-down outcome is classified by the constant core owns, never read as the
// stall whose arithmetic a wedge's text carries or as the re-pack failure whose
// suffix a seat lost in the structured re-pack carries, and the status-aware
// "seat not serving:" is the seat itself failing to serve, not contention.

// repackDuringSuffix is what the node appends to a seat-down reason filed for a
// re-pack that failed (internal/pipeline: repackDuring).
const repackDuringSuffix = " (during the structured re-pack)"

// wedgeReason is the reason a real monitor files for an engine whose counters froze
// under a request waiting in ph: a seat-down verdict wrapping the engine-flat stall
// it replaced, exactly as the loop and the re-pack file it.
func wedgeReason(t *testing.T, ph agent.Phase) string {
	t.Helper()
	pol := agent.StallPolicy{Floor: 40 * time.Millisecond, PrefillTokS: 1000, TokS: 100, Slack: 10 * time.Millisecond,
		Repack: 120 * time.Millisecond, EngineFlat: 150 * time.Millisecond, EnginePoll: 20 * time.Millisecond, EngineProbeTimeout: time.Second}
	frozen := func(context.Context) (agent.EngineReading, error) {
		return agent.EngineReading{Fingerprint: "v|1", TokenFingerprint: "vt|1", Summary: "vllm-metrics: 4 running, 0 waiting", Running: 4}, nil
	}
	ctx, m := agent.NewMonitor(context.Background(), pol, 10*time.Second)
	defer m.Stop()
	m.WithEngineProbe(frozen)
	m.Phase(ph, 0)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("a frozen engine under %s was never declared down", ph)
	}
	cause := m.Cause()
	var sd *agent.SeatDownError
	if !errors.As(cause, &sd) || !strings.HasPrefix(cause.Error(), core.SeatDownReason) {
		t.Fatalf("monitor cause = %T %v, want a *SeatDownError that opens %q (premise)", cause, cause, core.SeatDownReason)
	}
	return cause.Error()
}

func TestSeatDownReasonCodeHoldsForTheTextsTheNodeReallyFiles(t *testing.T) {
	reasons := map[string]string{
		"wedged in prefill":       wedgeReason(t, agent.PhasePrefill),
		"wedged in decoding":      wedgeReason(t, agent.PhaseDecoding),
		"wedged in the re-pack":   wedgeReason(t, agent.PhaseRepack),
		"died, never came back":   (&agent.SeatDownError{Kind: agent.SeatDownDied, Phase: agent.PhasePrefill, Note: "llama-swap lists the seat stopping", GaveUp: true, Waited: 200 * time.Second, Attempts: 4, LastSeen: "starting"}).Error(),
		"died, went down again":   (&agent.SeatDownError{Kind: agent.SeatDownDied, Note: "the stream was cut", Reissued: true, LastSeen: "ready"}).Error(),
		"died, not waited for":    (&agent.SeatDownError{Kind: agent.SeatDownDied, Note: "llama-swap does not list the seat", GaveUp: true}).Error(),
		"no evidence text at all": (&agent.SeatDownError{Kind: agent.SeatDownDied}).Error(),
	}
	for name, reason := range reasons {
		for _, suffix := range []string{"", repackDuringSuffix} {
			r := reason + suffix
			if !strings.HasPrefix(r, core.SeatDownReason) {
				t.Fatalf("%s: %q does not open %q (premise: the node's reason opens with the constant)", name, r, core.SeatDownReason)
			}
			pr := stallResult(r)
			if !SeatDownDefer(pr.Result) {
				t.Errorf("%s%s: the delegator does not read %q as the seat-down defer", name, suffix, r)
			}
			if got := reasonCodeFor(pr); got != ledger.ReasonSeatDown {
				t.Errorf("%s%s: reason_code = %q, want %q for %q", name, suffix, got, ledger.ReasonSeatDown, r)
			}
		}
	}
}

// "seat not serving:" (a 5xx from llama-swap that outlived the busy-seat wait while the
// seat was not known to be down) is coded seat_down wherever the node writes it: bare
// on the loop's arm, behind the re-pack's own prefix, and inside the parentheses the
// re-pack's transport arm puts it in. "seat contended:" (a 429: peers hold the slots)
// stays the infrastructure it was: the distinction ADR 0066 draws.
func TestSeatNotServingIsTheSeatItselfAndContentionIsNot(t *testing.T) {
	notServing := core.SeatNotServingReason + "llama-swap answered HTTP 500 for agent-pool on 7 attempt(s), waited 90s in total (its engine was starting, had crashed or failed its health check — this is not contention: raising concurrencyLimit will not help)"
	contended := "seat contended: agent-pool answered HTTP 429 on 3 attempt(s), waited 90s in total (peers hold its slots — raise concurrencyLimit or retry)"
	for _, tc := range []struct {
		name   string
		reason string
		want   string
	}{
		{"the loop's arm", notServing, ledger.ReasonSeatDown},
		{"the loop's arm, the wall expired during the wait", notServing + "; the wall expired during the wait", ledger.ReasonSeatDown},
		{"the loop's arm, with the failed request", notServing + " — chat: HTTP 500", ledger.ReasonSeatDown},
		{"behind the re-pack's prefix", "structured re-pack unreachable: " + notServing + "; the wall expired during the wait", ledger.ReasonSeatDown},
		{"inside the re-pack's transport arm", "structured re-pack unreachable: HTTP 502 (" + notServing + ")", ledger.ReasonSeatDown},
		{"contention is not the seat failing", contended, ledger.ReasonInfrastructure},
		{"contention behind the re-pack's prefix", "structured re-pack unreachable: " + contended + "; the wall expired during the wait", ledger.ReasonInfrastructure},
	} {
		if got := reasonCodeFor(stallResult(tc.reason)); got != tc.want {
			t.Errorf("%s: reason_code = %q, want %q for %q", tc.name, got, tc.want, tc.reason)
		}
	}
	// Only an infrastructure defer is read this way: the same words under another class
	// are whatever that class is.
	other := PlacedResult{Result: core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassBudget, Reason: notServing}}
	if got := reasonCodeFor(other); got != ledger.ReasonBudget {
		t.Errorf("the words under the budget class coded %q, want %q", got, ledger.ReasonBudget)
	}
}

// A seat-down defer is a job that RAN and failed on a seat that went down, observed by
// the node that filed it: it is never the never-started job of ADR 0064 (withdrawn,
// reaped, not started), which the delegator re-places for a different reason and
// credits a different wait for. The two are told apart by what the node said (a
// terminal job error with a never-ran prefix against a finished job carrying a defer)
// and by the marks the delegator sets on the result.
func TestASeatDownDeferIsNotANeverStartedJob(t *testing.T) {
	for _, reason := range []string{
		seatDownWire("node-a", 6).Reason,
		seatDownDuringTheRepack("node-a").Reason,
		wedgeReason(t, agent.PhaseDecoding),
	} {
		if neverRan(reason) {
			t.Errorf("neverRan(%q) = true: a seat-down reason must not read as a job the node never ran", reason)
		}
	}
	pr := PlacedResult{Result: seatDownWire("node-a", 6)}
	if pr.refused || pr.withdrawn || pr.nodeNeverRan != "" || pr.queuedWait != 0 {
		t.Fatalf("a seat-down defer carries a never-started mark: refused=%v withdrawn=%v never_ran=%q queued_wait=%v", pr.refused, pr.withdrawn, pr.nodeNeverRan, pr.queuedWait)
	}
	if got := reasonCodeFor(pr); got != ledger.ReasonSeatDown {
		t.Errorf("reason_code = %q, want %q (and never queue_withdrawn)", got, ledger.ReasonSeatDown)
	}
	// The converse: the never-started shapes are not seat-down defers.
	for _, reason := range []string{"withdrawn: the delegator took the job back", "reaped: nobody polled it within the poll lease", "not started: the node shut down while it was queued"} {
		if SeatDownDefer(core.AgentWireResult{Deferred: true, DeferClass: core.DeferClassInfrastructure, Reason: reason}) {
			t.Errorf("%q read as a seat-down defer", reason)
		}
	}
}
