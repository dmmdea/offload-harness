package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type recordingObserver struct {
	steps      []int
	tokens     []int
	phases     []string
	progress   []int    // OnProgress: run token totals per streamed delta
	allowances []string // OnAllowance: liveness phases as they were set
}

func (r *recordingObserver) OnStep(step, tokensOut int) {
	r.steps = append(r.steps, step)
	r.tokens = append(r.tokens, tokensOut)
}
func (r *recordingObserver) OnPhase(p string) { r.phases = append(r.phases, p) }
func (r *recordingObserver) OnProgress(tokensOut int) {
	r.progress = append(r.progress, tokensOut)
}
func (r *recordingObserver) OnAllowance(phase string, allowance time.Duration) {
	r.allowances = append(r.allowances, phase)
}

// The loop reports each completed step with the seat's running token total,
// and announces the forced final step — what the run registry publishes so a
// drain can see a run between its steps (0.117.0).
func TestLoopReportsStepsAndTheFinalPhaseToItsObserver(t *testing.T) {
	c := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "echo", `{"text":"hi"}`)}}, FinishReason: "tool_calls", Serve: &ServeStats{UsageCompletionTokens: 40}},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop", Serve: &ServeStats{UsageCompletionTokens: 60}},
	}}
	tools := []Tool{{
		ToolSpec: ToolSpec{Name: "echo", Description: "echo", Schema: json.RawMessage(`{"type":"object"}`)},
		Exec:     func(context.Context, string) (string, error) { return "echoed", nil },
	}}
	obs := &recordingObserver{}
	l := NewLoop(c, tools, 2).WithObserver(obs)
	if _, err := l.Run(context.Background(), "say hi"); err != nil {
		t.Fatal(err)
	}
	if len(obs.steps) != 2 || obs.steps[0] != 1 || obs.steps[1] != 2 || obs.tokens[0] != 40 || obs.tokens[1] != 100 {
		t.Fatalf("steps/tokens reported: %v %v", obs.steps, obs.tokens)
	}
	if len(obs.phases) != 1 || obs.phases[0] != "final" {
		t.Fatalf("the forced final step must be announced once, got %v", obs.phases)
	}
}

// No observer, no calls — the default path is unchanged.
func TestLoopWithoutAnObserverRunsAsBefore(t *testing.T) {
	c := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "ok"}, FinishReason: "stop"}}}
	if _, err := NewLoop(c, nil, 1).Run(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
}
