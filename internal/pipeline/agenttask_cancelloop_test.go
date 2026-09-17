package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestRunAgentTaskLoopCancelIsBudget (S-22/W-16, register: 2026-09-17 harness
// scheduling diagnosis §2(e), "a cancelled parent is filed as broken
// hardware"): when the DELEGATOR (or the node's own shutdown) cancels the
// PARENT context while the agent LOOP is still running — not the structured
// re-pack, which already has this arm — the failed request looks exactly
// like a dial refusal (a *url.Error), so it used to fall through to the
// generic "agent loop: "+rerr.Error() branch and defer as INFRASTRUCTURE: an
// operator told to fix a box that never misbehaved (12 "agent loop: context
// canceled" rows filed as broken hardware). Nothing on this box failed; the
// caller went away — a BUDGET shape, exactly like the re-pack's own arm
// thirty-odd lines below it.
func TestRunAgentTaskLoopCancelIsBudget(t *testing.T) {
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			time.Sleep(3 * time.Second) // well past the cancellation below
			return doneChat("too late")
		},
	}
	srv := fake.server(t)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(150*time.Millisecond, cancel)

	res := agentTestPipeline(t, srv.URL).Run(ctx, agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if !wire.Deferred {
		t.Fatalf("want deferred, got %+v", wire)
	}
	if wire.DeferClass != core.DeferClassBudget {
		t.Fatalf("defer_class = %q (reason %q), want %q — a ceiling outside the model's control stopped the run; it is neither a broken box nor a wrong answer",
			wire.DeferClass, wire.Reason, core.DeferClassBudget)
	}
	if !strings.Contains(wire.Reason, "agent loop") || !strings.Contains(wire.Reason, "cancel") {
		t.Fatalf("reason = %q, want the loop arm's own message naming the cancellation (not the generic \"agent loop: <opaque dial error>\")", wire.Reason)
	}
}

// TestRunAgentTaskLoopGenuine500StaysInfrastructure is the control: a REAL
// seat failure mid-loop (the endpoint answers, and answers badly) must still
// file as infrastructure — the new context.Canceled arm must not swallow a
// genuine broken-box signal.
func TestRunAgentTaskLoopGenuine500StaysInfrastructure(t *testing.T) {
	fake := &agentFake{
		rosterIDs:  []string{agentTestSeat},
		loop:       func(int64) string { return `{"error":{"message":"internal error"}}` },
		loopStatus: func(int64) int { return 500 },
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if !wire.Deferred {
		t.Fatalf("want deferred, got %+v", wire)
	}
	if wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("defer_class = %q (reason %q), want %q — a genuine 500 from the seat is a broken box, not a cancellation",
			wire.DeferClass, wire.Reason, core.DeferClassInfrastructure)
	}
}
