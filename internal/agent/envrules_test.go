package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// envTools returns three read-only style tools whose Exec records the args it
// saw; "big" returns a large body with a banner line to strip.
func envTools(t *testing.T, seen map[string][]string) []Tool {
	t.Helper()
	mk := func(name string, exec func(args string) (string, error)) Tool {
		return Tool{
			ToolSpec: ToolSpec{Name: name, Description: name, Schema: json.RawMessage(`{"type":"object"}`)},
			Exec: func(_ context.Context, args string) (string, error) {
				seen[name] = append(seen[name], args)
				return exec(args)
			},
		}
	}
	return []Tool{
		mk("list_dir", func(string) (string, error) { return "a.txt\nb.txt", nil }),
		mk("read_file", func(args string) (string, error) {
			return "[DEBUG] banner line\n" + strings.Repeat("x", 5000), nil
		}),
		mk("run_shell", func(string) (string, error) { return "ran", nil }),
		mk("fail", func(string) (string, error) { return "", errorf("open foo: no such file or directory") }),
	}
}

type simpleErr string

func (e simpleErr) Error() string { return string(e) }
func errorf(s string) error       { return simpleErr(s) }

func compiled(t *testing.T, r core.AgentEnvRules) *CompiledEnvRules {
	t.Helper()
	c, err := CompileEnvRules(&r)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if c == nil {
		t.Fatal("compile returned nil for a non-zero table")
	}
	return c.(*CompiledEnvRules)
}

// A zero table compiles to a nil INTERFACE, so the idiomatic
// `loop.WithEnvRules(compiled)` with no guard neither installs hooks nor
// panics (a typed-nil pointer would have passed the nil check and crashed in
// DeniedTools — review finding 2026-09-07).
func TestCompileEnvRulesZeroTableIsSafeToInstall(t *testing.T) {
	c, err := CompileEnvRules(&core.AgentEnvRules{})
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"}}}
	loop := NewLoop(client, envTools(t, map[string][]string{}), 3).WithEnvRules(c)
	if loop.envRules != nil {
		t.Fatal("zero table installed hooks")
	}
	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
}

func TestCompileEnvRulesNilAndZeroYieldNoRules(t *testing.T) {
	if c, err := CompileEnvRules(nil); c != nil || err != nil {
		t.Fatalf("nil table: c=%v err=%v", c, err)
	}
	if c, err := CompileEnvRules(&core.AgentEnvRules{}); c != nil || err != nil {
		t.Fatalf("zero table: c=%v err=%v", c, err)
	}
	if _, err := CompileEnvRules(&core.AgentEnvRules{ObservationStrip: []string{"("}}); err == nil {
		t.Fatal("bad regexp must fail compile")
	}
}

func TestEnvRulesDenyAndAllowWithholdSpecsNarrowOnly(t *testing.T) {
	seen := map[string][]string{}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "run_shell", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	loop := NewLoop(client, envTools(t, seen), 5).WithEnvRules(compiled(t, core.AgentEnvRules{
		DenyTools:  []string{"run_shell", "not_registered"},
		AllowTools: []string{"list_dir", "read_file", "run_shell", "web_fetch"},
	}))
	res, err := loop.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	// Offered specs: allow keeps list_dir/read_file/run_shell, deny removes run_shell;
	// "fail" is outside allow; web_fetch/not_registered were never registered.
	var names []string
	for _, s := range client.seenSpecs[0] {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "list_dir,read_file" {
		t.Fatalf("offered specs = %s, want list_dir,read_file", got)
	}
	// A call to a withheld tool lands as unknown — never executed.
	if len(seen["run_shell"]) != 0 {
		t.Fatal("denied tool executed")
	}
	if len(res.Effects) != 1 || res.Effects[0].Status != EffectNone || !strings.Contains(res.Effects[0].Note, "unknown tool") {
		t.Fatalf("effects = %+v", res.Effects)
	}
	if got := loop.EnvRulesDenied([]string{"list_dir", "read_file", "run_shell", "fail"}); strings.Join(got, ",") != "fail,run_shell" {
		t.Fatalf("EnvRulesDenied = %v", got)
	}
}

func TestEnvRulesMaxCallsPerToolBlocksAfterCapAndRecordsHits(t *testing.T) {
	seen := map[string][]string{}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{"path":"a"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c2", "list_dir", `{"path":"b"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c3", "list_dir", `{"path":"c"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	loop := NewLoop(client, envTools(t, seen), 10).WithEnvRules(compiled(t, core.AgentEnvRules{MaxCallsPerTool: map[string]int{"list_dir": 2}}))
	res, err := loop.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen["list_dir"]) != 2 {
		t.Fatalf("list_dir executed %d times, want 2", len(seen["list_dir"]))
	}
	third := client.seen[3][len(client.seen[3])-1]
	if third.Role != "tool" || !third.IsError || !strings.Contains(third.Content, "NOT executed") || !strings.Contains(third.Content, "limit") {
		t.Fatalf("blocked call must reach the model as an is_error refusal: %+v", third)
	}
	if len(res.RuleHits) != 1 || res.RuleHits[0].Rule != "max_calls_per_tool" || res.RuleHits[0].Effect != "blocked" || res.RuleHits[0].Step != 3 {
		t.Fatalf("rule hits = %+v", res.RuleHits)
	}
	// Structural withholding: after the block, the tool is no longer OFFERED
	// (the 4th Chat's specs lack it) — a text refusal alone would let a
	// fixated seat burn the whole step budget on blocked calls.
	for _, sp := range client.seenSpecs[3] {
		if sp.Name == "list_dir" {
			t.Fatal("capped tool still offered after the block")
		}
	}
	for _, sp := range client.seenSpecs[2] {
		if sp.Name == "list_dir" {
			return // offered right up to the block, as it should be
		}
	}
	t.Fatal("list_dir withheld BEFORE the cap was reached")
	if e := res.Effects[2]; e.Status != EffectNone || e.Rule != "max_calls_per_tool" || e.ObsChars == 0 {
		t.Fatalf("third effect = %+v", e)
	}
	if res.Effects[0].Rule != "" || res.Effects[0].ObsChars != len("a.txt\nb.txt") {
		t.Fatalf("first effect = %+v", res.Effects[0])
	}
}

// The cap counts EXECUTIONS: a call the loop's own breaker refused (exact
// repeat) must not spend the env cap, and env counters never leak between
// runs of one shared Loop (--serve).
func TestEnvRulesCapCountsExecutionsAndIsPerRun(t *testing.T) {
	seen := map[string][]string{}
	script := func() []Completion {
		return []Completion{
			{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{"path":"a"}`)}}, FinishReason: "tool_calls"},
			{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c2", "list_dir", `{"path":"a"}`)}}, FinishReason: "tool_calls"}, // exact repeat → breaker refusal, not an execution
			{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c3", "list_dir", `{"path":"b"}`)}}, FinishReason: "tool_calls"}, // 2nd execution: allowed
			{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
		}
	}
	client := &fakeClient{script: script()}
	loop := NewLoop(client, envTools(t, seen), 10).WithEnvRules(compiled(t, core.AgentEnvRules{MaxCallsPerTool: map[string]int{"list_dir": 2}}))
	res, err := loop.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen["list_dir"]) != 2 || len(res.RuleHits) != 0 {
		t.Fatalf("executions=%d hits=%+v; a breaker refusal must not spend the env cap", len(seen["list_dir"]), res.RuleHits)
	}
	// Second run on the SAME loop starts from zero.
	client.script, client.calls = script(), 0
	res, err = loop.Run(context.Background(), "go again")
	if err != nil {
		t.Fatal(err)
	}
	if len(seen["list_dir"]) != 4 || len(res.RuleHits) != 0 {
		t.Fatalf("second run: executions=%d hits=%+v; counters leaked across runs", len(seen["list_dir"]), res.RuleHits)
	}
}

func TestEnvRulesArgLimitsClampNumbersOnly(t *testing.T) {
	seen := map[string][]string{}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{
			tc("c1", "list_dir", `{"path":"a","limit":9000,"offset":"12","depth":3}`),
			tc("c2", "list_dir", `["not","an","object"]`),
			tc("c3", "list_dir", `{"path":"b","limit":10}`),
		}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	loop := NewLoop(client, envTools(t, seen), 10).WithEnvRules(compiled(t, core.AgentEnvRules{
		ArgLimits: map[string]map[string]float64{"list_dir": {"limit": 400, "offset": 1, "depth": 5}},
	}))
	res, err := loop.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(seen["list_dir"][0]), &got); err != nil {
		t.Fatalf("rewritten args not JSON: %s", seen["list_dir"][0])
	}
	if got["limit"] != 400.0 || got["offset"] != "12" || got["depth"] != 3.0 || got["path"] != "a" {
		t.Fatalf("clamp: %v (limit→400; string offset and under-cap depth untouched; other keys kept)", got)
	}
	if seen["list_dir"][1] != `["not","an","object"]` || seen["list_dir"][2] != `{"path":"b","limit":10}` {
		t.Fatalf("non-object / under-cap args must pass byte-for-byte: %q %q", seen["list_dir"][1], seen["list_dir"][2])
	}
	if len(res.RuleHits) != 1 || res.RuleHits[0].Rule != "arg_limits" || res.RuleHits[0].Effect != "rewrote_args" || !strings.Contains(res.RuleHits[0].Note, "limit 9000→400") {
		t.Fatalf("hits = %+v", res.RuleHits)
	}
	// The model is TOLD about the clamp on the result it reads (a silent clamp
	// makes its transcript lie, and the clamped args are the exact-repeat key).
	var clampedResult Msg
	for _, m := range client.seen[1] {
		if m.ToolCallID == "c1" {
			clampedResult = m
		}
	}
	if !strings.Contains(clampedResult.Content, "adjusted this call's arguments") || !strings.Contains(clampedResult.Content, "limit 9000→400") {
		t.Fatalf("clamp not surfaced to the model: %q", clampedResult.Content)
	}
	if seen["list_dir"][0] == "" || strings.Contains(seen["list_dir"][0], "400.") {
		t.Fatalf("cap must be marshalled as an integer: %s", seen["list_dir"][0])
	}
}

func TestEnvRulesObservationStripThenCapThenRewrite(t *testing.T) {
	seen := map[string][]string{}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "read_file", `{}`), tc("c2", "fail", `{}`), tc("c3", "list_dir", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	loop := NewLoop(client, envTools(t, seen), 10).WithEnvRules(compiled(t, core.AgentEnvRules{
		MaxObservationTokens: 300, // 1200 chars
		ObservationStrip:     []string{`(?m)^\[DEBUG\].*\n?`},
		RewriteError:         []core.AgentErrorRewrite{{Match: "no such file", Text: "That path does not exist. Call list_dir on its parent first."}},
	}))
	res, err := loop.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	msgs := client.seen[1]
	var readRes, failRes, listRes Msg
	for _, m := range msgs {
		switch m.ToolCallID {
		case "c1":
			readRes = m
		case "c2":
			failRes = m
		case "c3":
			listRes = m
		}
	}
	if strings.Contains(readRes.Content, "[DEBUG]") {
		t.Fatal("banner not stripped")
	}
	if len(readRes.Content) > 1200 || !strings.Contains(readRes.Content, "elided") {
		t.Fatalf("observation not capped: %d chars", len(readRes.Content))
	}
	if !failRes.IsError || failRes.Content != "That path does not exist. Call list_dir on its parent first." {
		t.Fatalf("error not rewritten: %+v", failRes)
	}
	if listRes.Content != "a.txt\nb.txt" {
		t.Fatalf("a clean, small, non-error result must pass byte-for-byte: %q", listRes.Content)
	}
	var rules []string
	for _, h := range res.RuleHits {
		rules = append(rules, h.Rule+":"+h.Effect)
	}
	if got := strings.Join(rules, " "); got != "observation_strip:stripped max_observation_tokens:truncated rewrite_error:rewrote_error" {
		t.Fatalf("hits = %s", got)
	}
	// The effect ledger carries the size the model actually read and the deciding rule.
	if res.Effects[0].ObsChars != len(readRes.Content) || res.Effects[0].Rule != "max_observation_tokens" || res.Effects[1].Rule != "rewrite_error" || res.Effects[2].Rule != "" {
		t.Fatalf("effects = %+v", res.Effects)
	}
}

func TestEnvRulesRewriteErrorLeavesSuccessAlone(t *testing.T) {
	c := compiled(t, core.AgentEnvRules{RewriteError: []core.AgentErrorRewrite{{Match: ".", Text: "rewritten"}}})
	o, hits := c.ModifyTransition(EnvAction{Tool: "x"}, EnvObservation{Content: "no such file", IsError: false})
	if o.Content != "no such file" || len(hits) != 0 {
		t.Fatalf("non-error rewritten: %+v %+v", o, hits)
	}
}

// Loop-authored texts are never rewritten or stripped: a rewrite_error /
// observation_strip pattern broad enough to match "NOT executed" or
// "unknown tool" must leave the breaker's refusal, the env rule's own block
// reason and the unknown-tool line byte-for-byte (review finding 2026-09-07).
func TestEnvRulesHooksNeverTouchLoopAuthoredText(t *testing.T) {
	seen := map[string][]string{}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{"path":"a"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{
			tc("c2", "list_dir", `{"path":"a"}`),   // exact repeat → breaker refusal
			tc("c3", "run_shell", `{}`),            // denied → unknown tool
			tc("c4", "read_file", `{"path":"x"}`),  // executes: cap 1 → next one blocked
			tc("c5", "read_file", `{"path":"y"}`)}, // env block reason
		}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	loop := NewLoop(client, envTools(t, seen), 10).WithEnvRules(compiled(t, core.AgentEnvRules{
		DenyTools:        []string{"run_shell"},
		MaxCallsPerTool:  map[string]int{"read_file": 1},
		ObservationStrip: []string{`(?i)NOT executed.*|error:.*|no longer offered.*`},
		RewriteError:     []core.AgentErrorRewrite{{Match: `(?i)executed|unknown|limit`, Text: "REWRITTEN"}},
	}))
	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range client.seen[2] {
		if m.Role == "tool" {
			got[m.ToolCallID] = m.Content
		}
	}
	if !strings.Contains(got["c2"], "NOT executed: you already called") {
		t.Fatalf("breaker refusal altered: %q", got["c2"])
	}
	if !strings.Contains(got["c3"], `unknown tool "run_shell"`) {
		t.Fatalf("unknown-tool line altered: %q", got["c3"])
	}
	if !strings.Contains(got["c5"], "NOT executed: read_file has already run 1 time(s)") {
		t.Fatalf("env block reason altered: %q", got["c5"])
	}
	if strings.Contains(got["c4"], "REWRITTEN") {
		t.Fatalf("a committed result was rewritten as an error: %q", got["c4"])
	}
}

func TestBuildRejectsAnInvalidEnvTableByName(t *testing.T) {
	_, err := Build(BuildConfig{PlannerBase: "http://127.0.0.1:1", Model: "m", ReadRoot: t.TempDir(),
		EnvRules: &core.AgentEnvRules{ObservationStrip: []string{"(unclosed"}}})
	if err == nil || !strings.Contains(err.Error(), "agent_env_rules") || !strings.Contains(err.Error(), "observation_strip[0]") {
		t.Fatalf("Build must fail by name on a bad table, got %v", err)
	}
}

func TestBuildNotesTheEnvTableAndWithheldTools(t *testing.T) {
	res, err := Build(BuildConfig{PlannerBase: "http://127.0.0.1:1", Model: "m", ReadRoot: t.TempDir(),
		EnvRules: &core.AgentEnvRules{DenyTools: []string{"list_dir"}, MaxObservationTokens: 100}})
	if err != nil {
		t.Fatal(err)
	}
	var note string
	for _, n := range res.Notes {
		if strings.HasPrefix(n, "agent env rules:") {
			note = n
		}
	}
	if !strings.Contains(note, "deny_tools=1") || !strings.Contains(note, "max_observation_tokens=100") || !strings.Contains(note, "withheld: list_dir") {
		t.Fatalf("note = %q", note)
	}
	for _, n := range res.Loop.AdvertisedTools() {
		if n == "list_dir" {
			t.Fatal("list_dir still advertised after deny")
		}
	}
}
