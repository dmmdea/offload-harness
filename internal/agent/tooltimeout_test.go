package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// blockingTool returns a tool whose Exec ignores cancellation entirely and
// blocks until release is closed — the worst case a per-tool cap has to survive.
func blockingTool(name string, release <-chan struct{}) Tool {
	return Tool{
		ToolSpec: ToolSpec{Name: name, Description: name, Schema: json.RawMessage(`{"type":"object"}`)},
		Exec: func(_ context.Context, _ string) (string, error) {
			<-release // deliberately NOT select-ing on ctx.Done()
			return "finally", nil
		},
	}
}

// A tool that overruns its budget must become a REACTABLE is_error result, not a
// hung loop. Before this cap existed, dispatch handed t.Exec the whole run
// context with no deadline: the harness's own media routes default to 720-1800s
// against a 180s agent_run budget, so one call could swallow the entire run.
func TestDispatchCapsAToolThatOverrunsItsBudget(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // let the leaked goroutine finish when the test ends

	l := NewLoop(&fakeClient{}, []Tool{blockingTool("slow_tool", release)}, 5)
	l.WithToolTimeout(50 * time.Millisecond)

	start := time.Now()
	out, isErr, eff := l.dispatch(context.Background(), ToolCall{ID: "1", Name: "slow_tool"})
	elapsed := time.Since(start)

	if !isErr {
		t.Fatalf("an overrunning tool must be an is_error result, got ok: %q", out)
	}
	if eff != EffectUnknown {
		t.Errorf("an abandoned-mid-flight tool must ledger as unknown (effects may exist), got %q", eff)
	}
	if !strings.Contains(out, "exceeded its") {
		t.Errorf("result should name the budget so the planner can react, got %q", out)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("dispatch took %s — the cap did not preempt a ctx-ignoring tool", elapsed)
	}
}

// The cap must NOT misreport the run's own deadline as the tool overrunning:
// when the parent context is already done the run ended, and blaming the tool
// would send the planner chasing the wrong problem.
func TestDispatchDistinguishesRunCancellationFromToolOverrun(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	l := NewLoop(&fakeClient{}, []Tool{blockingTool("slow_tool", release)}, 5)
	l.WithToolTimeout(10 * time.Second) // generous: the RUN is what expires

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	out, isErr, eff := l.dispatch(ctx, ToolCall{ID: "1", Name: "slow_tool"})
	if !isErr {
		t.Fatalf("expected an error result, got ok: %q", out)
	}
	if eff != EffectUnknown {
		t.Errorf("a call started before the run died must ledger as unknown, got %q", eff)
	}
	if strings.Contains(out, "exceeded its") {
		t.Errorf("the RUN expired, not the tool's budget — message blames the tool: %q", out)
	}
	if !strings.Contains(out, "run cancelled") {
		t.Errorf("result should name the run cancellation, got %q", out)
	}
}

// A per-tool Timeout overrides the loop default, so a genuinely long media route
// can be granted more than a read_file without loosening the cap for everything.
func TestPerToolTimeoutOverridesTheLoopDefault(t *testing.T) {
	release := make(chan struct{})
	close(release) // returns immediately

	slow := blockingTool("quick_tool", release)
	slow.Timeout = 5 * time.Second // generous per-tool budget

	l := NewLoop(&fakeClient{}, []Tool{slow}, 5)
	l.WithToolTimeout(1 * time.Millisecond) // a default this tool must NOT inherit

	out, isErr, eff := l.dispatch(context.Background(), ToolCall{ID: "1", Name: "quick_tool"})
	if isErr {
		t.Fatalf("per-tool Timeout should have overridden the tiny default, got error: %q", out)
	}
	if eff != EffectCommitted {
		t.Errorf("a completed tool must ledger as committed, got %q", eff)
	}
	if out != "finally" {
		t.Errorf("out = %q, want the tool's real result", out)
	}
}

// Fast tools are unaffected — the cap must not add latency or change results.
func TestDispatchLeavesAFastToolAlone(t *testing.T) {
	l := NewLoop(&fakeClient{}, mkTools("list_dir"), 5)
	out, isErr, eff := l.dispatch(context.Background(), ToolCall{ID: "1", Name: "list_dir"})
	if isErr || out != "ok" {
		t.Fatalf("fast tool result changed: out=%q isErr=%v", out, isErr)
	}
	if eff != EffectCommitted {
		t.Errorf("fast tool must ledger as committed, got %q", eff)
	}
}
