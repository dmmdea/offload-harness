package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// progressFakeClient invokes the context's ProgressFunc `ticks` times per
// call — what a streaming seat does — then answers from its script.
type progressFakeClient struct {
	ticks  int
	script []Completion
	calls  int
}

func (f *progressFakeClient) Chat(ctx context.Context, _ []Msg, _ []ToolSpec, _ int) (Completion, error) {
	if fn := ProgressFromContext(ctx); fn != nil {
		for i := 1; i <= f.ticks; i++ {
			fn(i)
		}
	}
	c := f.script[f.calls]
	f.calls++
	return c, nil
}

// The loop feeds every streamed delta to the observer (run totals) and the
// liveness monitor, sets the prefill phase before each seat call and the
// tool phase around a tool call (0.131.0).
func TestLoopReportsProgressAndLivenessPhases(t *testing.T) {
	c := &progressFakeClient{ticks: 3, script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "echo", `{"text":"hi"}`)}}, FinishReason: "tool_calls", Serve: &ServeStats{UsageCompletionTokens: 3}},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop", Serve: &ServeStats{UsageCompletionTokens: 3}},
	}}
	tools := []Tool{{
		ToolSpec: ToolSpec{Name: "echo", Description: "echo", Schema: json.RawMessage(`{"type":"object"}`)},
		Exec:     func(context.Context, string) (string, error) { return "echoed", nil },
	}}
	obs := &recordingObserver{}
	ctx, m := NewMonitor(context.Background(), msPolicy(), 5*time.Second)
	defer m.Stop()
	l := NewLoop(c, tools, 2).WithObserver(obs).WithLiveness(m)
	if _, err := l.Run(ctx, "say hi"); err != nil {
		t.Fatal(err)
	}
	// two calls x 3 deltas; the second call's totals continue from the first call's 3 tokens
	if len(obs.progress) != 6 || obs.progress[2] != 3 || obs.progress[3] != 4 || obs.progress[5] != 6 {
		t.Fatalf("progress totals = %v", obs.progress)
	}
	if !hasPhase(obs.allowances, "prefill") || !hasPhase(obs.allowances, "tool") || !hasPhase(obs.allowances, "decoding") {
		t.Fatalf("liveness phases = %v", obs.allowances)
	}
	tok, _, _, _ := m.Snapshot()
	if tok != 6 {
		t.Fatalf("monitor total = %d, want 6", tok)
	}
	if m.Cause() != nil || ctx.Err() != nil {
		t.Fatalf("a producing run was cancelled: %v", m.Cause())
	}
}

// Without a monitor or an observer the loop installs no ProgressFunc at all:
// the seat call's context is exactly what it was before 0.131.0.
func TestLoopWithoutLivenessInstallsNoProgressFunc(t *testing.T) {
	var saw bool
	c := &probeClient{fn: func(ctx context.Context) { saw = ProgressFromContext(ctx) != nil }}
	l := NewLoop(c, nil, 1)
	if _, err := l.Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if saw {
		t.Fatal("a ProgressFunc was installed with nothing listening")
	}
}

type probeClient struct{ fn func(context.Context) }

func (p *probeClient) Chat(ctx context.Context, _ []Msg, _ []ToolSpec, _ int) (Completion, error) {
	p.fn(ctx)
	return Completion{Msg: Msg{Role: "assistant", Content: "ok"}, FinishReason: "stop"}, nil
}

func hasPhase(list []string, p string) bool {
	for _, x := range list {
		if x == p {
			return true
		}
	}
	return false
}
