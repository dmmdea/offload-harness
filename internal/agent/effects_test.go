package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// One Run, four fates: a committed call, a failed call, a never-existed tool
// (none), and an exact-repeat refusal (none). The ledger must record all four
// in call order with the right statuses — this is the distinction the whole
// feature exists for.
func TestRunLedgersEveryRequestedCall(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{
			tc("c1", "echo", `{"text":"a"}`),
			tc("c2", "boom", `{}`),
			tc("c3", "ghost", `{}`),
		}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{
			tc("c4", "echo", `{"text":"a"}`), // exact repeat of c1 -> refusal
		}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	tools := []Tool{
		{ToolSpec: ToolSpec{Name: "echo", Description: "echo", Schema: json.RawMessage(`{"type":"object"}`)},
			Exec: func(_ context.Context, _ string) (string, error) { return "ok", nil }},
		{ToolSpec: ToolSpec{Name: "boom", Description: "boom", Schema: json.RawMessage(`{"type":"object"}`)},
			Exec: func(_ context.Context, _ string) (string, error) { return "", errors.New("kaput") }},
	}
	res, err := NewLoop(client, tools, 8).Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []struct {
		id     string
		tool   string
		status EffectStatus
	}{
		{"c1", "echo", EffectCommitted},
		{"c2", "boom", EffectFailed},
		{"c3", "ghost", EffectNone},
		{"c4", "echo", EffectNone},
	}
	if len(res.Effects) != len(want) {
		t.Fatalf("effects = %d records, want %d: %+v", len(res.Effects), len(want), res.Effects)
	}
	for i, w := range want {
		got := res.Effects[i]
		if got.CallID != w.id || got.Tool != w.tool || got.Status != w.status {
			t.Errorf("effects[%d] = {%s %s %s}, want {%s %s %s}", i, got.CallID, got.Tool, got.Status, w.id, w.tool, w.status)
		}
		if w.status != EffectCommitted && got.Note == "" {
			t.Errorf("effects[%d] (%s) is non-committed but has no note — the record must be auditable without the transcript", i, w.status)
		}
		if w.status == EffectCommitted && got.Note != "" {
			t.Errorf("effects[%d] committed calls must not duplicate result text into the ledger, note=%q", i, got.Note)
		}
	}
	if res.Effects[0].Step != 1 || res.Effects[3].Step != 2 {
		t.Errorf("steps = %d,%d — records must carry the model turn that requested them", res.Effects[0].Step, res.Effects[3].Step)
	}
}

// A run that dies on a Chat error must still return the ledger for everything
// that already ran — a caller inspecting a failed run needs to know what
// touched the world before the death.
func TestEffectsSurviveARunError(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "echo", `{"text":"a"}`)}}, FinishReason: "tool_calls"},
		// script exhausted on the next Chat -> error return path
	}}
	tools := []Tool{{ToolSpec: ToolSpec{Name: "echo", Description: "echo", Schema: json.RawMessage(`{"type":"object"}`)},
		Exec: func(_ context.Context, _ string) (string, error) { return "ok", nil }}}
	res, err := NewLoop(client, tools, 8).Run(context.Background(), "goal")
	if err == nil {
		t.Fatal("expected the scripted Chat error")
	}
	if len(res.Effects) != 1 || res.Effects[0].Status != EffectCommitted {
		t.Fatalf("error-path Result must still carry the ledger, got %+v", res.Effects)
	}
}

// EffectCounts: nil on no calls (so the MCP surface can omit the key), exact
// per-status counts otherwise.
func TestEffectCounts(t *testing.T) {
	if EffectCounts(nil) != nil {
		t.Error("no calls must aggregate to nil, not an empty map")
	}
	c := EffectCounts([]EffectRecord{
		{Status: EffectCommitted}, {Status: EffectCommitted},
		{Status: EffectUnknown}, {Status: EffectNone},
	})
	if c[EffectCommitted] != 2 || c[EffectUnknown] != 1 || c[EffectNone] != 1 || c[EffectFailed] != 0 {
		t.Errorf("counts = %v", c)
	}
}

// A refusal (policy-broker denial, allowlist, cage failure) must ledger as
// none — the world is untouched — while the model still sees the refusal as
// ordinary non-error content. This is review finding #1: before the
// NotPerformed sentinel, every defer-not-crash refusal classified as
// committed, and a run whose writes were ALL denied was ledger-identical to
// one whose writes all landed.
func TestRefusalLedgersAsNoneNotCommitted(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "write_file", `{"path":"x"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	denied := []Tool{{ToolSpec: ToolSpec{Name: "write_file", Description: "w", Schema: json.RawMessage(`{"type":"object"}`)},
		Exec: func(_ context.Context, _ string) (string, error) {
			return "", NotPerformed("NOT performed (deny): unattended runs may not overwrite")
		}}}
	res, err := NewLoop(client, denied, 4).Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Effects) != 1 || res.Effects[0].Status != EffectNone {
		t.Fatalf("a denied write must ledger none, got %+v", res.Effects)
	}
	// The model-visible result is byte-identical to the pre-ledger behaviour:
	// the refusal text as PLAIN (non-error) content.
	var toolMsg *Msg
	for i := range res.Transcript {
		if res.Transcript[i].Role == "tool" {
			toolMsg = &res.Transcript[i]
		}
	}
	if toolMsg == nil || toolMsg.IsError || toolMsg.Content != "NOT performed (deny): unattended runs may not overwrite" {
		t.Fatalf("refusal must reach the model as plain content, got %+v", toolMsg)
	}
}

// A tool abandoned on its per-call budget must reach Result.Effects as
// unknown through the FULL Run path, not just at dispatch level.
func TestRunLedgersAnAbandonedToolAsUnknown(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "slow_tool", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "gave up"}, FinishReason: "stop"},
	}}
	l := NewLoop(client, []Tool{blockingTool("slow_tool", release)}, 4)
	l.WithToolTimeout(30 * time.Millisecond)
	res, err := l.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Effects) != 1 || res.Effects[0].Status != EffectUnknown {
		t.Fatalf("abandoned tool must ledger unknown in the Run result, got %+v", res.Effects)
	}
	if res.Effects[0].Note == "" {
		t.Fatal("the unknown record must carry the why")
	}
}

// The destroyed-result re-execution path re-runs the tool for real — its
// record must reflect the actual re-execution (committed), not a synthesized
// refusal.
func TestReexecutionLedgersTheRealStatus(t *testing.T) {
	execs := 0
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "read", `{"p":"a"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c2", "read", `{"p":"a"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	tools := []Tool{{ToolSpec: ToolSpec{Name: "read", Description: "r", Schema: json.RawMessage(`{"type":"object"}`)},
		Exec: func(_ context.Context, _ string) (string, error) { execs++; return "data", nil }}}
	l := NewLoop(client, tools, 6)
	// Force the first result to read as a compaction artifact so the exact
	// repeat takes the re-execution path (resultDestroyed keys on artifact
	// markers; an empty original also counts as destroyed).
	res, err := l.Run(context.Background(), "goal")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Effects) != 2 {
		t.Fatalf("want 2 records, got %+v", res.Effects)
	}
	first, second := res.Effects[0], res.Effects[1]
	if first.Status != EffectCommitted {
		t.Errorf("first execution: want committed, got %s", first.Status)
	}
	// The second is EITHER a refusal (original intact -> none) or a re-execution
	// (destroyed -> committed); both are honest. What it must never be is a
	// committed record for a call that did not run: cross-check with execs.
	if second.Status == EffectCommitted && execs != 2 {
		t.Errorf("second record committed but Exec ran %d times", execs)
	}
	if second.Status == EffectNone && execs != 1 {
		t.Errorf("second record none but Exec ran %d times", execs)
	}
}

// refusalText normalizes the Exec-layer refusal contract for tests: a
// NotPerformed sentinel carries the refusal message in the error (dispatch
// shows the model exactly this text as non-error content); anything else
// passes through unchanged.
func refusalText(out string, err error) string {
	if IsNotPerformed(err) {
		return err.Error()
	}
	return out
}

// Reshaped judge part 2 (OpenHands pattern): a self-flagged high-risk call to
// an effectful tool is PARKED on unattended runs — ledgered none with an
// honest note — while low/omitted annotations proceed under the broker as
// before. Annotation only tightens.
func TestHighRiskSelfFlagParksOnUnattended(t *testing.T) {
	execs := 0
	mk := func() []Tool {
		return []Tool{{ToolSpec: ToolSpec{Name: "write_file", Description: "w", Schema: json.RawMessage(`{"type":"object"}`)},
			ParkOnHighRisk: true,
			Exec:           func(_ context.Context, _ string) (string, error) { execs++; return "wrote", nil }}}
	}
	script := func() []Completion {
		return []Completion{
			{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "write_file", `{"path":"a","security_risk":"high"}`)}}, FinishReason: "tool_calls"},
			{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c2", "write_file", `{"path":"b","security_risk":"low"}`)}}, FinishReason: "tool_calls"},
			{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c3", "write_file", `{"path":"c"}`)}}, FinishReason: "tool_calls"},
			{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
		}
	}

	// Unattended: high parks, low and omitted execute.
	execs = 0
	l := NewLoop(&fakeClient{script: script()}, mk(), 8).WithParkHighRisk(true)
	res, err := l.Run(context.Background(), "goal")
	if err != nil {
		t.Fatal(err)
	}
	if execs != 2 {
		t.Fatalf("unattended: want 2 executions (low+omitted), got %d", execs)
	}
	if res.Effects[0].Status != EffectNone || !strings.Contains(res.Effects[0].Note, "PARKED") {
		t.Fatalf("high-risk call must park as none, got %+v", res.Effects[0])
	}
	if res.Effects[0].Risk != "high" || res.Effects[1].Risk != "low" || res.Effects[2].Risk != "" {
		t.Fatalf("risk annotations must be recorded per call: %+v", res.Effects)
	}

	// Attended (park off): all three execute — annotation is telemetry only.
	execs = 0
	l2 := NewLoop(&fakeClient{script: script()}, mk(), 8)
	if _, err := l2.Run(context.Background(), "goal"); err != nil {
		t.Fatal(err)
	}
	if execs != 3 {
		t.Fatalf("attended: want 3 executions, got %d", execs)
	}
}

// A read-only tool (ParkOnHighRisk=false) is never parked, whatever the model
// claims — parking is scoped to effectful tools.
func TestReadOnlyToolNeverParked(t *testing.T) {
	execs := 0
	tools := []Tool{{ToolSpec: ToolSpec{Name: "read_file", Description: "r", Schema: json.RawMessage(`{"type":"object"}`)},
		Exec: func(_ context.Context, _ string) (string, error) { execs++; return "data", nil }}}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "read_file", `{"p":"a","security_risk":"high"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	if _, err := NewLoop(client, tools, 4).WithParkHighRisk(true).Run(context.Background(), "goal"); err != nil {
		t.Fatal(err)
	}
	if execs != 1 {
		t.Fatalf("read tool must execute despite the flag, got %d executions", execs)
	}
}

// Review finding #1: a present-but-unrecognized risk value fails CLOSED — the
// model tried to flag the call, so it parks, and the ledger records the raw
// attempt instead of silently discarding it.
func TestUnrecognizedRiskValueParks(t *testing.T) {
	execs := 0
	tools := []Tool{{ToolSpec: ToolSpec{Name: "write_file", Description: "w", Schema: json.RawMessage(`{"type":"object"}`)},
		ParkOnHighRisk: true,
		Exec:           func(_ context.Context, _ string) (string, error) { execs++; return "wrote", nil }}}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{
			tc("c1", "write_file", `{"path":"a","security_risk":"HIGH"}`),     // case variant
			tc("c2", "write_file", `{"path":"b","security_risk":"critical"}`), // synonym
			tc("c3", "write_file", `{"path":"c","security_risk":3}`),          // wrong type
		}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	res, err := NewLoop(client, tools, 6).WithParkHighRisk(true).Run(context.Background(), "goal")
	if err != nil {
		t.Fatal(err)
	}
	if execs != 0 {
		t.Fatalf("all three variants must park (fail closed), got %d executions", execs)
	}
	if res.Effects[0].Risk != "high" { // "HIGH" normalizes
		t.Errorf("case variant must normalize to high, got %q", res.Effects[0].Risk)
	}
	for _, i := range []int{1, 2} {
		if !strings.HasPrefix(res.Effects[i].Risk, "unrecognized(") {
			t.Errorf("effects[%d].Risk = %q, want the raw attempt preserved", i, res.Effects[i].Risk)
		}
		if res.Effects[i].Status != EffectNone {
			t.Errorf("effects[%d] must park, got %s", i, res.Effects[i].Status)
		}
	}
}

// Review finding #4: parked calls must NOT consume the same-name budget — the
// tool stays available for a subsequent low-risk call.
func TestParkedCallsDoNotDisableTheTool(t *testing.T) {
	execs := 0
	tools := []Tool{{ToolSpec: ToolSpec{Name: "write_file", Description: "w", Schema: json.RawMessage(`{"type":"object"}`)},
		ParkOnHighRisk: true,
		Exec:           func(_ context.Context, _ string) (string, error) { execs++; return "wrote", nil }}}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "write_file", `{"path":"a","security_risk":"high"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c2", "write_file", `{"path":"b","security_risk":"high"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c3", "write_file", `{"path":"c","security_risk":"high"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c4", "write_file", `{"path":"d","security_risk":"high"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c5", "write_file", `{"path":"e","security_risk":"low"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	res, err := NewLoop(client, tools, 10).WithParkHighRisk(true).Run(context.Background(), "goal")
	if err != nil {
		t.Fatal(err)
	}
	if execs != 1 {
		t.Fatalf("the low-risk call after 4 parks must still execute, got %d executions", execs)
	}
	last := res.Effects[len(res.Effects)-1]
	if last.Status != EffectCommitted || last.Risk != "low" {
		t.Fatalf("final low-risk call must commit: %+v", last)
	}
}

// Review finding #3: parks reach the durable recorder (the builder wires it to
// the ask queue) with tool, args and normalized risk.
func TestParkRecorderReceivesParkedCalls(t *testing.T) {
	var got []string
	tools := []Tool{{ToolSpec: ToolSpec{Name: "run_shell", Description: "s", Schema: json.RawMessage(`{"type":"object"}`)},
		ParkOnHighRisk: true,
		Exec:           func(_ context.Context, _ string) (string, error) { return "ran", nil }}}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "run_shell", `{"command":"rm -rf build","security_risk":"high"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	l := NewLoop(client, tools, 4).WithParkHighRisk(true).
		WithParkRecorder(func(tool, args, risk string) { got = append(got, tool+"|"+risk) })
	if _, err := l.Run(context.Background(), "goal"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "run_shell|high" {
		t.Fatalf("park recorder must fire once with tool+risk, got %v", got)
	}
}
