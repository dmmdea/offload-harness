package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
)

// The node-side half of D-114: a tool-call argument the completion budget cut
// is a BUDGET defect. Until 0.124.0 the loop's HTTP 500 reached the generic
// "agent loop:" branch and the run was filed as defer_class "infrastructure" —
// the stack blamed for a budget the seat could not fit a ~3 KB write into.

// cutToolCallChat is llama.cpp's refusal of a tool call whose JSON arguments
// the budget cut mid-string (served with status 500 by loopStatus below).
func cutToolCallChat(int64) string {
	return `{"error":{"code":500,"message":"Failed to parse tool call arguments as JSON: ` +
		`[json.exception.parse_error.101] parse error at line 1, column 2847: syntax error while parsing ` +
		`value - invalid string: missing closing quote","type":"server_error"}}`
}

func TestRunAgentTaskCutToolCallDefersAsBudgetNeverInfrastructure(t *testing.T) {
	fake := &agentFake{
		rosterIDs:  []string{agentTestSeat},
		loop:       cutToolCallChat,
		loopStatus: func(int64) int { return 500 }, // cut at the step budget, then cut again at the final budget
		repack:     func(int64) string { return `{"answer":"42"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	res := agentTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)

	if !wire.Deferred {
		t.Fatalf("deferred = false; a twice-cut tool call answered nothing (reason %q)", wire.Reason)
	}
	if wire.DeferClass != core.DeferClassBudget {
		t.Fatalf("defer_class = %q, want %q — the seat's completion budget, not a broken box", wire.DeferClass, core.DeferClassBudget)
	}
	if wire.StopReason != agent.StopToolCallCut {
		t.Fatalf("stop_reason = %q, want %q", wire.StopReason, agent.StopToolCallCut)
	}
	if !strings.Contains(wire.Reason, "tool-call argument cut") || !strings.Contains(wire.Reason, "2847") {
		t.Errorf("reason = %q, want the loop's arithmetic (both budgets and the partial argument size)", wire.Reason)
	}
	if wire.StopNote != wire.Reason {
		t.Errorf("stop_note %q must carry the same evidence as the defer reason %q", wire.StopNote, wire.Reason)
	}
	if n := fake.loopCalls.Load(); n != 2 {
		t.Errorf("planner completions = %d, want 2: the cut step and its ONE re-issue", n)
	}
	if fake.grammarCNT.Load() != 0 {
		t.Error("a run that answered nothing must never spend a re-pack completion")
	}
	if len(wire.Calls) != 2 {
		t.Errorf("calls = %d (%+v), want both cut attempts on the record", len(wire.Calls), wire.Calls)
	}
}
