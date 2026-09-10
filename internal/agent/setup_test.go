package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

func setupActs(specs ...string) []core.AgentSetupAction {
	var out []core.AgentSetupAction
	for _, s := range specs {
		tool, args, _ := strings.Cut(s, " ")
		a := core.AgentSetupAction{Tool: tool}
		if args != "" {
			a.Args = json.RawMessage(args)
		}
		out = append(out, a)
	}
	return out
}

// The replay lands after the objective and before the first model turn, as
// one assistant turn carrying the calls plus one tool result each; the model's
// first Chat sees exactly that; no step is spent; the ledger says setup.
func TestSetupReplaySeedsTheFirstTurnWithoutSpendingASep(t *testing.T) {
	seen := map[string][]string{}
	client := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "answer: from a.txt"}, FinishReason: "stop"}}}
	loop := NewLoop(client, envTools(t, seen), 3).WithSystem("sys").
		WithSetupActions(setupActs(`list_dir {"path":"."}`, `read_file {"path":"a.txt"}`))
	res, err := loop.Run(context.Background(), "the objective")
	if err != nil {
		t.Fatal(err)
	}
	if res.Steps != 1 || res.StopReason != "done" {
		t.Fatalf("steps=%d stop=%s — setup must not spend a step", res.Steps, res.StopReason)
	}
	if len(seen["list_dir"]) != 1 || len(seen["read_file"]) != 1 {
		t.Fatalf("tools not executed once each: %v", seen)
	}
	first := client.seen[0]
	// system, objective, setup assistant turn, two tool results
	if len(first) != 5 {
		for i, m := range first {
			t.Logf("%d %s calls=%d", i, m.Role, len(m.ToolCalls))
		}
		t.Fatalf("first Chat saw %d messages, want 5", len(first))
	}
	if first[1].Role != "user" || first[1].Content != "the objective" {
		t.Fatalf("objective must precede the replay: %+v", first[1])
	}
	head := first[2]
	if head.Role != "assistant" || len(head.ToolCalls) != 2 || head.ToolCalls[0].ID != "setup-1" || head.ToolCalls[1].Name != "read_file" || !strings.HasPrefix(head.Content, "setup: 2 action(s)") {
		t.Fatalf("setup head = %+v", head)
	}
	if first[3].Role != "tool" || first[3].ToolCallID != "setup-1" || first[3].Content != "a.txt\nb.txt" {
		t.Fatalf("first result = %+v", first[3])
	}
	// The ledger: two Step-0 setup records, both committed; SetupRan = 2.
	if len(res.Effects) != 2 {
		t.Fatalf("effects = %+v", res.Effects)
	}
	for _, e := range res.Effects {
		if !e.Setup || e.Step != 0 || e.Status != EffectCommitted || e.ObsChars == 0 {
			t.Fatalf("setup record = %+v", e)
		}
	}
	if SetupRan(res.Effects) != 2 {
		t.Fatalf("SetupRan = %d", SetupRan(res.Effects))
	}
	// The second tool result is the read_file output, bounded by the loop cap
	// like any result.
	if first[4].Role != "tool" || first[4].ToolCallID != "setup-2" || len(first[4].Content) > loop.toolResultCapChars() {
		t.Fatalf("second result = role %s id %s len %d", first[4].Role, first[4].ToolCallID, len(first[4].Content))
	}
}

// No actions = the transcript the model sees is byte-identical to a loop
// that never had the option (the pre-key invariant).
func TestSetupReplayNoActionsIsByteIdentical(t *testing.T) {
	mk := func(withEmpty bool) [][]Msg {
		client := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"}}}
		loop := NewLoop(client, envTools(t, map[string][]string{}), 2).WithSystem("sys")
		if withEmpty {
			loop = loop.WithSetupActions(nil)
		}
		if _, err := loop.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		return client.seen
	}
	a, b := mk(false), mk(true)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("transcripts differ:\n%s\n%s", ja, jb)
	}
}

// A failing or unknown action is an observation, never an abort: the model
// sees the error text as the tool result and the run proceeds. Unknown tools
// ran nothing (none, not counted); a tool that errored ran (failed, counted).
func TestSetupReplayFailuresAreObservationsNotAborts(t *testing.T) {
	client := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "ok"}, FinishReason: "stop"}}}
	loop := NewLoop(client, envTools(t, map[string][]string{}), 3).
		WithSetupActions(setupActs(`nope {}`, `fail {}`, `list_dir {}`))
	res, err := loop.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("a failing setup action must not abort the run: %v", err)
	}
	if res.Steps != 1 {
		t.Fatalf("steps = %d", res.Steps)
	}
	first := client.seen[0]
	// objective, head, three results
	if len(first) != 5 {
		t.Fatalf("first Chat saw %d messages", len(first))
	}
	if !first[2].IsError || !strings.Contains(first[2].Content, `unknown tool "nope"`) {
		t.Fatalf("unknown tool result = %+v", first[2])
	}
	if !first[3].IsError || !strings.Contains(first[3].Content, "no such file") {
		t.Fatalf("failed tool result = %+v", first[3])
	}
	st := map[string]EffectStatus{}
	for _, e := range res.Effects {
		st[e.Tool] = e.Status
		if !e.Setup || e.Step != 0 {
			t.Fatalf("record = %+v", e)
		}
	}
	if st["nope"] != EffectNone || st["fail"] != EffectFailed || st["list_dir"] != EffectCommitted {
		t.Fatalf("statuses = %v", st)
	}
	if SetupRan(res.Effects) != 2 {
		t.Fatalf("SetupRan must count committed+failed only, got %d", SetupRan(res.Effects))
	}
}

// Setup calls feed no circuit breaker: the model's own identical call
// afterwards is a first call, not an exact repeat.
func TestSetupReplayFeedsTheExactRepeatBreakerOnly(t *testing.T) {
	seen := map[string][]string{}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "read_file", `{"path":"a.txt"}`)}}, FinishReason: "tool_calls"}, // byte-identical to setup: refused
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c2", "read_file", `{"path":"b.txt"}`)}}, FinishReason: "tool_calls"}, // a different read: runs
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	loop := NewLoop(client, envTools(t, seen), 5).
		WithSetupActions(setupActs(`read_file {"path":"a.txt"}`))
	res, err := loop.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	// 0.115.12 (D-48): the replayed read IS the first call of that (tool,
	// args) — the model repeating it byte for byte is refused with the
	// breaker's "you already have that result", and the transcript holds ONE
	// copy of the document, not two. A different read still runs, and the
	// same-name cap is not spent by the replay.
	if len(seen["read_file"]) != 2 {
		t.Fatalf("read_file executions = %v, want setup + the model's b.txt read only", seen)
	}
	var modelFirst, modelSecond *EffectRecord
	for i := range res.Effects {
		if res.Effects[i].Setup || res.Effects[i].Tool != "read_file" {
			continue
		}
		if modelFirst == nil {
			modelFirst = &res.Effects[i]
		} else if modelSecond == nil {
			modelSecond = &res.Effects[i]
		}
	}
	if modelFirst == nil || modelFirst.Status != EffectNone || !strings.Contains(modelFirst.Note, "already have that result") {
		t.Fatalf("model's repeat of the setup read = %+v, want it refused as an exact repeat", modelFirst)
	}
	if modelSecond == nil || modelSecond.Status != EffectCommitted {
		t.Fatalf("model's different read = %+v, want it executed", modelSecond)
	}
}

// TestSetupReplayReadFileEndsWithACompleteFileFooter: a replayed read that
// reached EOF says so where a continuation hint would sit, and a read that
// did NOT reach EOF keeps the hint and gets no footer.
func TestSetupReplayReadFileEndsWithACompleteFileFooter(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"}}}
	tools, err := ReadOnlyTools(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	loop := NewLoop(client, tools, 3).WithSetupActions(setupActs(`read_file {"path":"doc.txt"}`))
	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	var replay string
	for _, m := range client.seen[0] {
		if m.Role == "tool" && m.ToolCallID == "setup-1" {
			replay = m.Content
		}
	}
	if !strings.Contains(replay, "(complete file: 4 lines") || strings.Contains(replay, "use offset=") {
		t.Fatalf("replayed read = %q, want the complete-file footer and no continuation hint", replay)
	}
	client3 := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"}}}
	loop3 := NewLoop(client3, tools, 3).WithSetupActions(setupActs(`read_file {"path":"doc.txt","offset":99}`))
	if _, err := loop3.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	for _, m := range client3.seen[0] {
		if m.Role == "tool" && m.ToolCallID == "setup-1" && strings.Contains(m.Content, "complete file") {
			t.Fatalf("a past-EOF replayed read must not get the footer: %q", m.Content)
		}
	}
	client2 := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"}}}
	loop2 := NewLoop(client2, tools, 3).WithSetupActions(setupActs(`read_file {"path":"doc.txt","limit":2}`))
	if _, err := loop2.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	for _, m := range client2.seen[0] {
		if m.Role == "tool" && m.ToolCallID == "setup-1" && (!strings.Contains(m.Content, "use offset=") || strings.Contains(m.Content, "complete file")) {
			t.Fatalf("partial replayed read = %q, want the continuation hint and no footer", m.Content)
		}
	}
}

// TestRefusedCallRepeatedTwiceWithdrawsTheToolAndAsksForTheAnswer (D-49):
// the first list_dir runs; the second, byte-identical, is refused by the
// exact-repeat breaker; the third — the second refusal of the same call —
// withdraws the tool from the spec list and the next Chat opens on an
// answer-now user turn. Before 0.115.12 the model could burn every step on
// the same refused call.
func TestRefusedCallRepeatedTwiceWithdrawsTheToolAndAsksForTheAnswer(t *testing.T) {
	seen := map[string][]string{}
	same := func(id string) Completion {
		return Completion{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc(id, "list_dir", `{"path":"a"}`)}}, FinishReason: "tool_calls"}
	}
	client := &fakeClient{script: []Completion{
		same("c1"), same("c2"), same("c3"),
		{Msg: Msg{Role: "assistant", Content: "the answer"}, FinishReason: "stop"},
	}}
	loop := NewLoop(client, envTools(t, seen), 8)
	res, err := loop.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "the answer" || len(seen["list_dir"]) != 1 {
		t.Fatalf("output %q, list_dir executions %d (want 1)", res.Output, len(seen["list_dir"]))
	}
	// The 4th Chat: no list_dir spec, and the last message is the answer-now turn.
	for _, sp := range client.seenSpecs[3] {
		if sp.Name == "list_dir" {
			t.Fatal("list_dir still offered after two identical refusals")
		}
	}
	last := client.seen[3][len(client.seen[3])-1]
	if last.Role != "user" || !strings.Contains(last.Content, "list_dir") || !strings.Contains(last.Content, "Answer the task now") {
		t.Fatalf("the turn after the second refusal must open with the answer-now instruction, got %+v", last)
	}
	// The 3rd Chat still offered it (withdrawn only on the second refusal).
	offered := false
	for _, sp := range client.seenSpecs[2] {
		if sp.Name == "list_dir" {
			offered = true
		}
	}
	if !offered {
		t.Fatal("list_dir withdrawn before the second refusal")
	}
}

// Env rules apply to setup the same way they apply to a model call — a denied
// tool is refused with the rule's reason, a strip runs on the output, and the
// per-tool execution cap is NOT spent by the replay.
func TestSetupReplayHonoursEnvRulesWithoutSpendingCaps(t *testing.T) {
	seen := map[string][]string{}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "read_file", `{"path":"b.txt"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	loop := NewLoop(client, envTools(t, seen), 5).WithEnvRules(compiled(t, core.AgentEnvRules{
		DenyTools:        []string{"run_shell"},
		MaxCallsPerTool:  map[string]int{"read_file": 1},
		ObservationStrip: []string{`(?m)^\[DEBUG\].*\n`},
	})).WithSetupActions(setupActs(`run_shell {}`, `read_file {"path":"a.txt"}`))
	res, err := loop.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	first := client.seen[0]
	// deny_tools withholds STRUCTURALLY (the tool leaves the loop's set at
	// install time), so a setup call on it is an "unknown tool" observation
	// with nothing executed — the same thing the model would see.
	if len(seen["run_shell"]) != 0 || !first[2].IsError || !strings.Contains(first[2].Content, "unknown tool") || res.Effects[0].Status != EffectNone || !res.Effects[0].Setup {
		t.Fatalf("denied setup tool must not execute: msgs=%+v eff=%+v", first[2], res.Effects[0])
	}
	if strings.Contains(first[3].Content, "[DEBUG]") {
		t.Fatalf("observation_strip must run on setup results: %q", first[3].Content[:40])
	}
	// The cap of 1 on read_file was NOT spent by setup: the model's own first
	// read_file executes.
	if len(seen["read_file"]) != 2 {
		t.Fatalf("read_file executions = %d (setup + model), want 2: %v", len(seen["read_file"]), seen)
	}
	for _, e := range res.Effects {
		if !e.Setup && e.Tool == "read_file" && e.Status != EffectCommitted {
			t.Fatalf("model's read_file under cap 1 after one setup read = %+v (cap was spent by setup)", e)
		}
	}
}

// Past the replay budget the remaining actions are not run and say so; the
// run proceeds. The budget is half the compaction budget, so a tiny window
// admits one bounded read and not eight.
func TestSetupReplayStopsAtItsBudget(t *testing.T) {
	seen := map[string][]string{}
	client := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "ok"}, FinishReason: "stop"}}}
	loop := NewLoop(client, envTools(t, seen), 3).WithContextTokens(1200).
		WithSetupActions(setupActs(`read_file {"path":"a"}`, `read_file {"path":"b"}`, `read_file {"path":"c"}`, `read_file {"path":"d"}`, `read_file {"path":"e"}`, `read_file {"path":"f"}`, `read_file {"path":"g"}`, `read_file {"path":"h"}`))
	res, err := loop.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	ran := SetupRan(res.Effects)
	if ran == 0 || ran == 8 {
		t.Fatalf("expected the budget to stop the replay partway, ran=%d effects=%+v", ran, res.Effects)
	}
	skipped := 0
	for _, e := range res.Effects {
		if e.Setup && e.Status == EffectNone {
			skipped++
			if !strings.Contains(e.Note, "setup budget") {
				t.Fatalf("skipped record must say why: %+v", e)
			}
		}
	}
	if ran+skipped != 8 {
		t.Fatalf("ran %d + skipped %d != 8", ran, skipped)
	}
	// Exactly `ran` calls appear on the head, and only those results follow.
	head := client.seen[0][1]
	if head.Role != "assistant" || len(head.ToolCalls) != ran {
		t.Fatalf("head = %+v (ran=%d)", head, ran)
	}
}

// Under real compaction pressure the setup result SURVIVES byte-for-byte
// because it is pinned: a tight window, a bulky setup read, then three bulky
// model reads — the lossy rungs must eat the model's older bodies (or fail to
// fit) before they touch the pinned setup body. Unpinned, the oldest tool
// body is the first thing the ladder elides.
func TestSetupReplayResultSurvivesCompactionBecausePinned(t *testing.T) {
	seen := map[string][]string{}
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "read_file", `{"path":"b"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c2", "read_file", `{"path":"c"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c3", "read_file", `{"path":"d"}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	loop := NewLoop(client, envTools(t, seen), 6).WithSystem("sys").WithContextTokens(2600).WithSkeletonPrune(true).
		WithSetupActions(setupActs(`read_file {"path":"a"}`))
	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	first := client.seen[0]
	var setupBody string
	for _, m := range first {
		if m.Role == "tool" && m.ToolCallID == "setup-1" {
			setupBody = m.Content
		}
	}
	if setupBody == "" || len(setupBody) < 1000 {
		t.Fatalf("setup body missing or too small to pressure compaction: %d", len(setupBody))
	}
	last := client.seen[len(client.seen)-1]
	total := 0
	var got string
	for _, m := range last {
		total += len(m.Content)
		if m.Role == "tool" && m.ToolCallID == "setup-1" {
			got = m.Content
		}
	}
	if total >= len(setupBody)*4 {
		t.Fatalf("compaction never ran (last transcript %d chars) — the test cannot discriminate", total)
	}
	if got != setupBody {
		t.Fatalf("pinned setup result was compacted: got %d chars, want the original %d", len(got), len(setupBody))
	}
}

// The replay sits OUTSIDE the protected preamble: preambleLen (system +
// objective) is unchanged, so compaction can still act on the run, and the
// setup results are pinned (the lossy rungs keep them) rather than protected.
func TestSetupReplayIsPinnedNotPreamble(t *testing.T) {
	client := &fakeClient{script: []Completion{
		{Msg: Msg{Role: "assistant", ToolCalls: []ToolCall{tc("c1", "list_dir", `{}`)}}, FinishReason: "tool_calls"},
		{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"},
	}}
	loop := NewLoop(client, envTools(t, map[string][]string{}), 4).WithSystem("sys").
		WithSetupActions(setupActs(`read_file {"path":"a"}`))
	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	// Second Chat: system, objective, setup head, setup result, model call, model result.
	second := client.seen[1]
	if len(second) != 6 || second[3].ToolCallID != "setup-1" || second[5].ToolCallID != "c1" {
		for i, m := range second {
			t.Logf("%d %s id=%s", i, m.Role, m.ToolCallID)
		}
		t.Fatalf("second Chat saw %d messages", len(second))
	}
}

// The setup footer counts only the lines read_file itself cut (0.115.19,
// reviewer finding on PR #303), through the REAL read_file: a line over
// maxLineChars is named as cut, and a document that merely contains the marker
// text (tools.go does) is not — it gets the clean "all there is" footer.
func TestSetupFooterNamesTruncatedLinesAndMidTextEnds(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("long.txt", "short line\n"+strings.Repeat("y", maxLineChars+50)+"\nlast line")
	write("marker.txt", "line one says (line truncated) in its text\nline two ends mid-wo")
	tools, err := ReadOnlyTools(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	footer := func(file string) string {
		t.Helper()
		client := &fakeClient{script: []Completion{{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop"}}}
		if _, err := NewLoop(client, tools, 3).WithContextTokens(65536).WithSetupActions(setupActs(`read_file {"path":"`+file+`"}`)).Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		for _, m := range client.seen[0] {
			if m.Role == "tool" {
				return m.Content[strings.LastIndex(m.Content, "\n")+1:]
			}
		}
		t.Fatal("no setup result in the first turn")
		return ""
	}
	if got := footer("long.txt"); !strings.Contains(got, "1 over-long line(s) were cut at 2000 characters") {
		t.Fatalf("a line read_file cut must be named, footer %q", got)
	}
	if got := footer("marker.txt"); strings.Contains(got, "were cut") || !strings.Contains(got, "even where it begins or ends mid-sentence") {
		t.Fatalf("a document that only contains the marker text is not cut, footer %q", got)
	}
}
