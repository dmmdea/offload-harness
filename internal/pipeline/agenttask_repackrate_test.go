package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// With no decode rate known from any source (no seat-rates sample, no streamed rate
// this run, no completion of the loop long enough to measure one, no agent_seat_tok_s)
// the wall bound does no arithmetic at all, and a re-pack that fails is exactly the
// one it would have bounded. Failing open is right; failing open silently is not
// (register C-80), so the failed re-pack says its time bound was off.
func TestRunAgentTaskFailedRepackSaysItsTimeBoundWasOffWhenNoRateIsKnown(t *testing.T) {
	fake := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat("The answer is 42.") },
		repack:       func(int64) string { return `{"wrong":"shape"}` },
		chatFallback: func(int64) string { return doneChat(`{"wrong":"shape"}`) },
	}
	srv := fake.server(t)
	defer srv.Close()

	wire := decodeWire(t, agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || wire.DeferClass != core.DeferClassAbstention {
		t.Fatalf("deferred=%v class=%q reason=%q, want the wrong-shape abstention", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	for _, want := range []string{"re-pack time bound off", "no decode rate is known", "agent_seat_tok_s"} {
		if !strings.Contains(wire.RepackNote, want) {
			t.Errorf("repack_note = %q, want it to say %q", wire.RepackNote, want)
		}
	}
}

// A box that knows the rate has a bound and says nothing: the note is for the
// exception, and a clean success never carries one.
func TestRunAgentTaskRepackSaysNothingAboutItsTimeBoundWhenARateIsKnown(t *testing.T) {
	failing := &agentFake{
		rosterIDs:    []string{agentTestSeat},
		loop:         func(int64) string { return doneChat("The answer is 42.") },
		repack:       func(int64) string { return `{"wrong":"shape"}` },
		chatFallback: func(int64) string { return doneChat(`{"wrong":"shape"}`) },
	}
	srv := failing.server(t)
	defer srv.Close()
	wire := decodeWire(t, rateTestPipeline(t, srv.URL, 100).Run(context.Background(), agentTestRequest(t, testContract())))
	if !wire.Deferred || strings.Contains(wire.RepackNote, "time bound off") {
		t.Fatalf("deferred=%v repack_note=%q, want a failure with no word about a bound that was on", wire.Deferred, wire.RepackNote)
	}
}
