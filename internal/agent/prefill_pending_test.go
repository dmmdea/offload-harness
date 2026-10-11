package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// allowObserver records the stall allowance the loop published for each phase, in order.
type allowObserver struct{ list []phaseAllowance }

type phaseAllowance struct {
	phase string
	allow time.Duration
}

func (a *allowObserver) OnStep(int, int) {}
func (a *allowObserver) OnPhase(string)  {}
func (a *allowObserver) OnProgress(int)  {}
func (a *allowObserver) OnAllowance(phase string, allow time.Duration) {
	a.list = append(a.list, phaseAllowance{phase, allow})
}

// prefills is the allowances published for the prefill phase, one per seat call.
func (a *allowObserver) prefills() []time.Duration {
	var out []time.Duration
	for _, p := range a.list {
		if p.phase == string(PhasePrefill) {
			out = append(out, p.allow)
		}
	}
	return out
}

// prefillPolicy is a policy whose prefill arithmetic is easy to read: 1,000 tok/s, no slack, a
// 1 ms floor, so the allowance of a call is pending/1000 x 1.5 seconds.
func prefillPolicy() StallPolicy {
	return StallPolicy{Admission: time.Second, PrefillTokS: 1000, TokS: 100, ToolTimeout: time.Second,
		Repack: time.Second, Floor: time.Millisecond}
}

// bigTool is a tool whose spec block costs the seat roughly 2,000 tokens on every request.
func bigTool(name string) Tool {
	return Tool{
		ToolSpec: ToolSpec{Name: name, Description: strings.Repeat("describes what this tool does and when to use it. ", 140), Schema: json.RawMessage(`{"type":"object"}`)},
		Exec:     func(context.Context, string) (string, error) { return "ok", nil },
	}
}

func firstPrefillAllowance(t *testing.T, tools []Tool) time.Duration {
	t.Helper()
	c := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"}}}
	obs := &allowObserver{}
	ctx, m := NewMonitor(context.Background(), prefillPolicy(), 30*time.Second)
	defer m.Stop()
	l := NewLoop(c, tools, 2).WithObserver(obs).WithLiveness(m)
	if _, err := l.Run(ctx, "summarise the attached notes"); err != nil {
		t.Fatal(err)
	}
	p := obs.prefills()
	if len(p) == 0 {
		t.Fatalf("no prefill allowance was published: %+v", obs.list)
	}
	return p[0]
}

// The prefill clock is sized from the prompt the seat will actually prefill, and that prompt
// carries the tool-spec block on every request (register F10). The loop used to size it from
// the transcript alone, so a seat with a few thousand tokens of tool JSON in front of a short
// objective was given the floor: a 4,613-token prefill sat "silent 59 s of 60 s allowed" while
// the seat was working. The same transcript with a larger tool block must get a larger clock.
func TestPrefillClockCountsTheToolSpecBlock(t *testing.T) {
	bare := firstPrefillAllowance(t, nil)
	armed := firstPrefillAllowance(t, []Tool{bigTool("alpha"), bigTool("beta")})
	// two ~7 KB descriptions: well over 3,000 tokens of spec, 1.5 s per 1,000 at the policy's rate
	if armed-bare < 4*time.Second {
		t.Fatalf("prefill allowance with tools %s, without %s: the spec block is not in the clock", armed, bare)
	}
}

// What the server says the previous prompt cost is the truth the next estimate is held to:
// the transcript only grows, so a step that prefilled U tokens is followed by one that needs at
// least that many. The 4-byte rule undercounts by 1.3-1.4x on dense text (the WithTokenCalibration
// fit), and nothing corrected the stall clock for it.
func TestPrefillClockFollowsTheServersCountOfThePreviousPrompt(t *testing.T) {
	c := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "echo", `{"text":"hi"}`)}}, FinishReason: "tool_calls",
			Serve: &ServeStats{UsagePromptTokens: 6000, UsageCompletionTokens: 3}},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop", Serve: &ServeStats{UsagePromptTokens: 6200, UsageCompletionTokens: 3}},
	}}
	echo := Tool{
		ToolSpec: ToolSpec{Name: "echo", Description: "echo", Schema: json.RawMessage(`{"type":"object"}`)},
		Exec:     func(context.Context, string) (string, error) { return "echoed", nil },
	}
	obs := &allowObserver{}
	pol := prefillPolicy()
	ctx, m := NewMonitor(context.Background(), pol, 30*time.Second)
	defer m.Stop()
	if _, err := NewLoop(c, []Tool{echo}, 3).WithObserver(obs).WithLiveness(m).Run(ctx, "say hi"); err != nil {
		t.Fatal(err)
	}
	p := obs.prefills()
	if len(p) < 2 {
		t.Fatalf("want two seat calls, got prefill allowances %v", p)
	}
	if need := pol.Allowance(PhasePrefill, 6000); p[1] < need {
		t.Fatalf("second call's prefill allowance %s is under what the 6,000 tokens the server counted for the first need (%s)", p[1], need)
	}
}

// The first call has no server count to learn from, so the transcript estimate is raised by the
// density the fit found (assumedPromptDensity) before the tool block is added.
func TestPrefillPendingArithmetic(t *testing.T) {
	if got := prefillPending(1000, 2000, assumedPromptDensity, 0); got != int(1000*assumedPromptDensity)+2000 {
		t.Fatalf("pending = %d, want estimate x density + spec block", got)
	}
	if got := prefillPending(1000, 0, 1, 0); got != 1000 {
		t.Fatalf("a toolless run with an exact estimate: %d", got)
	}
	// The previous call's server count is a floor: the transcript only grows.
	if got := prefillPending(1000, 0, 1, 6000); got != 6000 {
		t.Fatalf("pending = %d, want the previous prompt's 6,000 as the floor", got)
	}
	if got := prefillPending(9000, 0, 1, 6000); got != 9000 {
		t.Fatalf("pending = %d, a larger estimate must win over the floor", got)
	}
	// An observed density below 1 (the estimate over-counted) is not trusted to shrink the clock.
	if d := observedPromptDensity(6000, 2000, 5000); d != 0.8 {
		t.Fatalf("observed density = %v, want (6000-2000)/5000 = 0.8", d)
	}
	if got := prefillPending(1000, 0, observedPromptDensity(6000, 2000, 5000), 0); got != 1000 {
		t.Fatalf("a density below 1 must not shrink the estimate: %d", got)
	}
	// A server count far above the estimate is clamped, so one odd reading cannot make the clock unbounded.
	if d := observedPromptDensity(1_000_000, 0, 100); d > maxPromptDensity {
		t.Fatalf("observed density %v is not clamped to %v", d, maxPromptDensity)
	}
	// No reading (no usage block, or nothing sent) keeps the assumed density.
	if d := observedPromptDensity(0, 0, 100); d != assumedPromptDensity {
		t.Fatalf("no usage must keep the assumed density, got %v", d)
	}
}

// A seat that really is wedged still stalls, and the stall names the sized allowance: the
// scaling widens the bound, it does not remove it.
func TestWedgedSeatStillStallsUnderTheSizedPrefillClock(t *testing.T) {
	hang := &probeClient{fn: func(ctx context.Context) { <-ctx.Done() }}
	pol := StallPolicy{Admission: time.Second, PrefillTokS: 10000, TokS: 100, ToolTimeout: time.Second,
		Repack: time.Second, Floor: 30 * time.Millisecond, Slack: 10 * time.Millisecond}
	ctx, m := NewMonitor(context.Background(), pol, 30*time.Second)
	defer m.Stop()
	l := NewLoop(hang, []Tool{bigTool("alpha")}, 2).WithLiveness(m)
	done := make(chan struct{})
	go func() { _, _ = l.Run(ctx, "summarise"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a wedged seat was never stalled")
	}
	var se *StallError
	if !errors.As(m.Cause(), &se) || se.Phase != PhasePrefill {
		t.Fatalf("cause = %v, want a prefill stall", m.Cause())
	}
	if se.Allowed > 2*time.Second || se.Allowed < 30*time.Millisecond {
		t.Fatalf("stall allowance %s is not the sized bound", se.Allowed)
	}
}

// The density a call showed carries to the next: a seat whose tokenizer reads this text at 2
// tokens per estimated token gets a clock sized for that when the transcript grows, not the
// assumed 1.3. The floor alone cannot do it: the transcript grew by a large tool result.
func TestPrefillClockLearnsTheRunsDensity(t *testing.T) {
	big := strings.Repeat("x", 8000) // ~2,000 estimated tokens in one tool result
	echo := Tool{
		ToolSpec: ToolSpec{Name: "echo", Description: "echo", Schema: json.RawMessage(`{"type":"object"}`)},
		Exec:     func(context.Context, string) (string, error) { return big, nil },
	}
	c := &fakeClient{}
	obs := &allowObserver{}
	pol := prefillPolicy()
	ctx, m := NewMonitor(context.Background(), pol, 30*time.Second)
	defer m.Stop()
	l := NewLoop(c, []Tool{echo}, 3).WithObserver(obs).WithLiveness(m)
	l.resolveSpecReserve(ctx)
	spec := int(l.specReserve.Load())
	// First call: the server counts 2 tokens per estimated token of the transcript it was sent.
	// The estimate is only known once the call is made, so the script is built on first use.
	c.script = []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "echo", `{"text":"hi"}`)}}, FinishReason: "tool_calls",
			Serve: &ServeStats{UsagePromptTokens: 0, UsageCompletionTokens: 3}},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop", Serve: &ServeStats{UsageCompletionTokens: 3}},
	}
	// Probe the first transcript's estimate with a dry run of the same objective.
	probe := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"}}}
	if _, err := NewLoop(probe, []Tool{echo}, 3).Run(context.Background(), "say hi"); err != nil {
		t.Fatal(err)
	}
	est1 := estimateTokens(probe.seen[0])
	c.script[0].Serve.UsagePromptTokens = spec + 2*est1
	if _, err := l.Run(ctx, "say hi"); err != nil {
		t.Fatal(err)
	}
	p := obs.prefills()
	if len(p) < 2 || len(c.seen) < 2 {
		t.Fatalf("want two seat calls, got %v / %d", p, len(c.seen))
	}
	est2 := estimateTokens(c.seen[1])
	want := pol.Allowance(PhasePrefill, prefillPending(est2, spec, 2.0, spec+2*est1))
	assumed := pol.Allowance(PhasePrefill, prefillPending(est2, spec, assumedPromptDensity, spec+2*est1))
	if want <= assumed {
		t.Fatalf("test shape: the learned density must size a larger clock than the assumed one (%s vs %s)", want, assumed)
	}
	if p[1] < want-time.Millisecond || p[1] > want+time.Millisecond {
		t.Fatalf("second call's prefill allowance = %s, want %s (density 2.0 learned from the first call; assumed would give %s)", p[1], want, assumed)
	}
}

// Before any call of the run has reported its prompt size, the clock is sized with the assumed
// density: the transcript estimate x assumedPromptDensity + the tool block, exactly.
func TestFirstCallPrefillClockUsesTheAssumedDensity(t *testing.T) {
	// A long objective, so the 4-byte estimate is thousands of tokens and the density term is visible.
	objective := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 400)
	c := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"}}}
	obs := &allowObserver{}
	pol := prefillPolicy()
	ctx, m := NewMonitor(context.Background(), pol, 30*time.Second)
	defer m.Stop()
	l := NewLoop(c, []Tool{bigTool("alpha")}, 2).WithObserver(obs).WithLiveness(m)
	if _, err := l.Run(ctx, objective); err != nil {
		t.Fatal(err)
	}
	est := estimateTokens(c.seen[0])
	spec := int(l.specReserve.Load())
	want := pol.Allowance(PhasePrefill, int(float64(est)*assumedPromptDensity)+spec)
	got := obs.prefills()
	if len(got) == 0 || got[0] != want {
		t.Fatalf("first prefill allowance = %v, want %s (estimate %d x %.1f + spec %d)", got, want, est, assumedPromptDensity, spec)
	}
}
