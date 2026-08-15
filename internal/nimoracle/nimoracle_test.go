package nimoracle

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/nimclient"
)

// --- Adapt ---

func TestAdaptClassify_FencedReplyCanonicalCasing(t *testing.T) {
	req := core.Request{Task: core.TaskClassify, Params: map[string]any{"labels": []any{"Bug", "Feature"}}}
	res, ok := Adapt(core.TaskClassify, req, "```json\n{\"label\": \"bug\"}\n```")
	if !ok {
		t.Fatal("expected ok")
	}
	var m map[string]string
	if err := json.Unmarshal(res.Data, &m); err != nil {
		t.Fatalf("data unmarshal: %v", err)
	}
	if m["label"] != "Bug" {
		t.Fatalf("want canonical label Bug, got %q", m["label"])
	}
}

func TestAdaptClassify_LabelOutsideSetRejected(t *testing.T) {
	req := core.Request{Task: core.TaskClassify, Params: map[string]any{"labels": []string{"bug", "feature"}}}
	if _, ok := Adapt(core.TaskClassify, req, `{"label": "question"}`); ok {
		t.Fatal("label outside the set must be rejected, not recorded as disagreement")
	}
}

func TestAdaptClassify_NoLabelsParamRejected(t *testing.T) {
	// Without a label set, agreement is ill-defined over an open vocabulary and
	// a refusal could be laundered into a disagreement label — reject.
	req := core.Request{Task: core.TaskClassify}
	if _, ok := Adapt(core.TaskClassify, req, `{"label": "spam"}`); ok {
		t.Fatal("classify without a label set must be rejected")
	}
}

func TestAdaptClassify_ThinkSpanStripped(t *testing.T) {
	// A JSON-looking fragment inside <think> must not win over the real answer.
	req := core.Request{Task: core.TaskClassify, Params: map[string]any{"labels": []string{"bug", "feature"}}}
	for name, raw := range map[string]string{
		"prefix":   "<think>Maybe {\"label\": \"feature\"}? No — it reports breakage.</think>\n{\"label\": \"bug\"}",
		"preamble": "Sure, let me work through this.\n<think>Maybe {\"label\": \"feature\"}?</think>\n{\"label\": \"bug\"}",
	} {
		res, ok := Adapt(core.TaskClassify, req, raw)
		if !ok {
			t.Fatalf("%s: expected ok", name)
		}
		var m map[string]string
		_ = json.Unmarshal(res.Data, &m)
		if m["label"] != "bug" {
			t.Fatalf("%s: think-span fragment won over the real answer: got %q", name, m["label"])
		}
	}
	// An unclosed span means the answer never materialized — un-judgeable.
	if _, ok := Adapt(core.TaskClassify, req, "preamble <think>still thinking {\"label\": \"feature\"}"); ok {
		t.Fatal("unclosed think span must be rejected")
	}
}

func TestAdaptTriage_NormalizesCase(t *testing.T) {
	res, ok := Adapt(core.TaskTriage, core.Request{Task: core.TaskTriage}, `The answer is {"decision": "YES"}`)
	if !ok {
		t.Fatal("expected ok")
	}
	var m map[string]string
	_ = json.Unmarshal(res.Data, &m)
	if m["decision"] != "yes" {
		t.Fatalf("want normalized yes, got %q", m["decision"])
	}
}

func TestAdaptTriage_InvalidDecisionRejected(t *testing.T) {
	if _, ok := Adapt(core.TaskTriage, core.Request{Task: core.TaskTriage}, `{"decision": "maybe"}`); ok {
		t.Fatal("decision outside yes/no/unsure must be rejected")
	}
}

func TestAdaptExtract_ObjectPassthrough(t *testing.T) {
	res, ok := Adapt(core.TaskExtract, core.Request{Task: core.TaskExtract}, "```json\n{\"name\": \"ACME\", \"total\": \"42\"}\n```")
	if !ok {
		t.Fatal("expected ok")
	}
	var m map[string]any
	if err := json.Unmarshal(res.Data, &m); err != nil || m["name"] != "ACME" {
		t.Fatalf("want extracted object, got %s (err %v)", res.Data, err)
	}
}

func TestAdaptExtract_NonObjectRejected(t *testing.T) {
	if _, ok := Adapt(core.TaskExtract, core.Request{Task: core.TaskExtract}, `["a", "b"]`); ok {
		t.Fatal("non-object extraction must be rejected (grounding.Check needs an object)")
	}
}

func TestAdaptSummarize_JSONShape(t *testing.T) {
	res, ok := Adapt(core.TaskSummarize, core.Request{Task: core.TaskSummarize}, `{"summary": "short version"}`)
	if !ok {
		t.Fatal("expected ok")
	}
	var m map[string]string
	_ = json.Unmarshal(res.Data, &m)
	if m["summary"] != "short version" {
		t.Fatalf("got %q", m["summary"])
	}
}

func TestAdaptSummarize_InstructedBulletsShape(t *testing.T) {
	// The shape the prompt actually asks for: summary + bullets. Only the
	// summary field feeds the judge; bullets must not break adaptation.
	raw := "```json\n{\"summary\": \"Latency fell after the migration.\", \"bullets\": [\"12 ms median\", \"no data loss\"]}\n```"
	res, ok := Adapt(core.TaskSummarize, core.Request{Task: core.TaskSummarize}, raw)
	if !ok {
		t.Fatal("expected ok")
	}
	var m map[string]string
	_ = json.Unmarshal(res.Data, &m)
	if m["summary"] != "Latency fell after the migration." {
		t.Fatalf("got %q", m["summary"])
	}
}

func TestAdaptSummarize_ProseRejected(t *testing.T) {
	// No prose fallback: raw prose (including guardrail refusals like "I can't
	// summarize this content.") must be rejected, never recorded as a
	// near-zero-similarity disagreement label.
	if _, ok := Adapt(core.TaskSummarize, core.Request{Task: core.TaskSummarize}, "I can't summarize this content."); ok {
		t.Fatal("plain prose must be rejected (refusal-poisoning guard)")
	}
}

func TestAdaptSummarize_EmptyRejected(t *testing.T) {
	if _, ok := Adapt(core.TaskSummarize, core.Request{Task: core.TaskSummarize}, "   "); ok {
		t.Fatal("empty summary must be rejected")
	}
	if _, ok := Adapt(core.TaskSummarize, core.Request{Task: core.TaskSummarize}, `{"summary": ""}`); ok {
		t.Fatal("empty summary field must be rejected")
	}
}

func TestAdaptUnsupportedTask(t *testing.T) {
	if _, ok := Adapt(core.TaskVQA, core.Request{Task: core.TaskVQA}, `{"answer":"x"}`); ok {
		t.Fatal("unsupported task must be rejected")
	}
}

// --- Prompt ---

func TestPromptClassifyIncludesLabels(t *testing.T) {
	req := core.Request{Task: core.TaskClassify, Input: "some text", Params: map[string]any{"labels": []string{"bug", "feature"}}}
	system, user, ok := Prompt(core.TaskClassify, req)
	if !ok || !strings.Contains(system, "bug, feature") || user != "some text" {
		t.Fatalf("bad classify prompt: ok=%v system=%q user=%q", ok, system, user)
	}
}

func TestPromptClassify_NoLabelsRejected(t *testing.T) {
	if _, _, ok := Prompt(core.TaskClassify, core.Request{Task: core.TaskClassify, Input: "x"}); ok {
		t.Fatal("classify without a label set must not be promptable (saves the paid call)")
	}
}

func TestPromptSummarize_MirrorsLocalShape(t *testing.T) {
	// Must mirror tasks.buildSummarize: 1-2 sentence "summary" + up to N
	// "bullets" (default 5), so the B2 judge compares like against like.
	_, user, ok := Prompt(core.TaskSummarize, core.Request{Task: core.TaskSummarize, Input: "text here"})
	if !ok || !strings.Contains(user, `up to 5 key points in "bullets"`) || !strings.Contains(user, `1-2 sentence "summary"`) {
		t.Fatalf("summarize prompt must mirror the local summary/bullets split with default 5 points: %q", user)
	}
	_, user, _ = Prompt(core.TaskSummarize, core.Request{Task: core.TaskSummarize, Input: "text", Params: map[string]any{"max_points": 3}})
	if !strings.Contains(user, "up to 3 key points") {
		t.Fatalf("max_points not threaded: %q", user)
	}
}

func TestPromptExtract_NoVerbatimCoaching(t *testing.T) {
	// The extract label IS grounding.Check on the oracle's output — the prompt
	// must not coach the metric.
	system, _, ok := Prompt(core.TaskExtract, core.Request{Task: core.TaskExtract, Input: "x"})
	if !ok || strings.Contains(strings.ToUpper(system), "VERBATIM") {
		t.Fatalf("extract prompt must stay neutral about groundedness: %q", system)
	}
}

func TestPromptTriageIncludesQuestion(t *testing.T) {
	req := core.Request{Task: core.TaskTriage, Input: "body", Params: map[string]any{"question": "is it spam?"}}
	_, user, ok := Prompt(core.TaskTriage, req)
	if !ok || !strings.Contains(user, "is it spam?") || !strings.Contains(user, "body") {
		t.Fatalf("bad triage prompt: %q", user)
	}
}

func TestPromptExtractIncludesSchema(t *testing.T) {
	req := core.Request{Task: core.TaskExtract, Input: "text", Params: map[string]any{
		"schema": map[string]any{"properties": map[string]any{"name": map[string]any{"type": "string"}}},
	}}
	system, _, ok := Prompt(core.TaskExtract, req)
	if !ok || !strings.Contains(system, `"name"`) {
		t.Fatalf("schema missing from extract prompt: %q", system)
	}
}

func TestPromptUnsupportedTask(t *testing.T) {
	if _, _, ok := Prompt(core.TaskOCR, core.Request{Task: core.TaskOCR}); ok {
		t.Fatal("ocr must not be promptable")
	}
}

// --- WrapRunTier ---

func chatReturning(content string, truncated bool, err error) ChatFunc {
	return func(ctx context.Context, model, system, user string, maxTokens int, temperature float64) (nimclient.ChatResult, error) {
		return nimclient.ChatResult{Content: content, Truncated: truncated, Model: model}, err
	}
}

// stubLocal is a no-op local RunTier for tests that only exercise the remote path.
func stubLocal(ctx context.Context, req core.Request, model string) (core.Result, bool) {
	return core.Result{}, false
}

func TestWrapRunTier_OracleAliasGoesRemote(t *testing.T) {
	localCalled := false
	local := func(ctx context.Context, req core.Request, model string) (core.Result, bool) {
		localCalled = true
		return core.Result{}, false
	}
	rt := WrapRunTier(chatReturning(`{"decision":"no"}`, false, nil), "big-model", 512, "esc-alias", local, nil)
	req := core.Request{Task: core.TaskTriage, Input: "x"}
	res, ok := rt(context.Background(), req, "esc-alias")
	if !ok || localCalled {
		t.Fatalf("oracle call must go remote (ok=%v localCalled=%v)", ok, localCalled)
	}
	if !strings.Contains(string(res.Data), "no") {
		t.Fatalf("bad adapted data: %s", res.Data)
	}
}

func TestWrapRunTier_OtherModelStaysLocal(t *testing.T) {
	remoteCalled := false
	chat := func(ctx context.Context, model, system, user string, maxTokens int, temperature float64) (nimclient.ChatResult, error) {
		remoteCalled = true
		return nimclient.ChatResult{}, nil
	}
	local := func(ctx context.Context, req core.Request, model string) (core.Result, bool) {
		return core.Result{OK: true, Data: json.RawMessage(`{"decision":"yes"}`)}, true
	}
	rt := WrapRunTier(chat, "big-model", 512, "esc-alias", local, nil)
	res, ok := rt(context.Background(), core.Request{Task: core.TaskTriage}, "gemma-4-e2b")
	if !ok || remoteCalled {
		t.Fatalf("E2B counterfactual must stay local (ok=%v remoteCalled=%v)", ok, remoteCalled)
	}
	if !strings.Contains(string(res.Data), "yes") {
		t.Fatalf("local result not passed through: %s", res.Data)
	}
}

func TestWrapRunTier_ChatErrorSkipsAndCounts(t *testing.T) {
	stats := &Stats{}
	rt := WrapRunTier(chatReturning("", false, errors.New("NIM 401: bad key")), "m", 512, "esc", stubLocal, stats)
	if _, ok := rt(context.Background(), core.Request{Task: core.TaskTriage}, "esc"); ok {
		t.Fatal("chat error must yield ok=false")
	}
	if stats.ChatErr != 1 || stats.Remote != 1 || stats.FirstErr == nil || !strings.Contains(stats.FirstErr.Error(), "401") {
		t.Fatalf("chat error must be counted with FirstErr kept verbatim: %+v", stats)
	}
}

func TestWrapRunTier_TruncatedSkipsAndCounts(t *testing.T) {
	stats := &Stats{}
	rt := WrapRunTier(chatReturning(`{"decision":"yes"}`, true, nil), "m", 512, "esc", stubLocal, stats)
	if _, ok := rt(context.Background(), core.Request{Task: core.TaskTriage}, "esc"); ok {
		t.Fatal("truncated reply must yield ok=false")
	}
	if stats.Truncated != 1 {
		t.Fatalf("truncation must be counted: %+v", stats)
	}
}

func TestWrapRunTier_EmptyReplyCounts(t *testing.T) {
	stats := &Stats{}
	rt := WrapRunTier(chatReturning("   ", false, nil), "m", 512, "esc", stubLocal, stats)
	if _, ok := rt(context.Background(), core.Request{Task: core.TaskTriage}, "esc"); ok {
		t.Fatal("empty reply must yield ok=false")
	}
	if stats.Empty != 1 || stats.Remote != 1 {
		t.Fatalf("empty reply must be counted as a paid call: %+v", stats)
	}
}

func TestWrapRunTier_PaidUnadaptableCounts(t *testing.T) {
	// A well-formed reply Adapt rejects (label outside the set) is a PAID
	// unadaptable, distinct from the free pre-call unpromptable skip.
	stats := &Stats{}
	rt := WrapRunTier(chatReturning(`{"label": "banana"}`, false, nil), "m", 512, "esc", stubLocal, stats)
	req := core.Request{Task: core.TaskClassify, Params: map[string]any{"labels": []string{"bug", "feature"}}}
	if _, ok := rt(context.Background(), req, "esc"); ok {
		t.Fatal("out-of-set label must yield ok=false")
	}
	if stats.Unadaptable != 1 || stats.Unpromptable != 0 || stats.Remote != 1 {
		t.Fatalf("post-call rejection must count as unadaptable (paid): %+v", stats)
	}
}

func TestWrapRunTier_SummarizeBudgetGrows(t *testing.T) {
	// The summarize call must carry the bullets headroom on top of cfgMax —
	// a flat cfgMax is the truncation steady state that burns queues.
	var gotMax int
	chat := func(ctx context.Context, model, system, user string, maxTokens int, temperature float64) (nimclient.ChatResult, error) {
		gotMax = maxTokens
		return nimclient.ChatResult{Content: `{"summary": "s"}`}, nil
	}
	rt := WrapRunTier(chat, "m", 1024, "esc", stubLocal, nil)
	if _, ok := rt(context.Background(), core.Request{Task: core.TaskSummarize, Input: "x"}, "esc"); !ok {
		t.Fatal("expected ok")
	}
	want := 1024 + 384 + 160*5
	if gotMax != want {
		t.Fatalf("summarize budget: got %d want %d", gotMax, want)
	}
	if _, ok := rt(context.Background(), core.Request{Task: core.TaskTriage, Input: "x"}, "esc"); ok {
		t.Fatal("triage adapt of a summarize-shaped reply should fail (shape mismatch expected here)")
	}
	if gotMax != 1024 {
		t.Fatalf("non-summarize tasks must use the flat budget: got %d", gotMax)
	}
}

func TestMaxPointsFor_MirrorsLocalClamp(t *testing.T) {
	if n := maxPointsFor(nil); n != 5 {
		t.Fatalf("absent => 5, got %d", n)
	}
	if n := maxPointsFor(map[string]any{"max_points": 0}); n != 1 {
		t.Fatalf("explicit 0 => clamp 1 (mirrors tasks.buildSummarize), got %d", n)
	}
	if n := maxPointsFor(map[string]any{"max_points": 3.0}); n != 3 {
		t.Fatalf("JSON float => 3, got %d", n)
	}
}

func TestPreflight(t *testing.T) {
	// Error passthrough.
	if err := Preflight(context.Background(), chatReturning("", false, errors.New("NIM 401: nope")), "m", 512); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("chat error must surface verbatim, got %v", err)
	}
	// Truncation at the summarize budget fails loud.
	if err := Preflight(context.Background(), chatReturning(`{"summary":"s"}`, true, nil), "m", 512); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("truncated preflight must error, got %v", err)
	}
	// Non-JSON prose fails the adapt gate.
	if err := Preflight(context.Background(), chatReturning("a fine prose summary", false, nil), "m", 512); err == nil || !strings.Contains(err.Error(), "not adaptable") {
		t.Fatalf("unadaptable preflight must error, got %v", err)
	}
	// A clean instructed reply passes, and the budget carries bullets headroom.
	var gotMax int
	chat := func(ctx context.Context, model, system, user string, maxTokens int, temperature float64) (nimclient.ChatResult, error) {
		gotMax = maxTokens
		return nimclient.ChatResult{Content: `{"summary": "It worked.", "bullets": ["a"]}`}, nil
	}
	if err := Preflight(context.Background(), chat, "m", 512); err != nil {
		t.Fatalf("clean preflight must pass: %v", err)
	}
	if want := 512 + 384 + 160*5; gotMax != want {
		t.Fatalf("preflight budget: got %d want %d", gotMax, want)
	}
}

func TestWrapRunTier_UnsupportedTaskSkipsWithoutChat(t *testing.T) {
	chatCalled := false
	chat := func(ctx context.Context, model, system, user string, maxTokens int, temperature float64) (nimclient.ChatResult, error) {
		chatCalled = true
		return nimclient.ChatResult{Content: "x"}, nil
	}
	stats := &Stats{}
	rt := WrapRunTier(chat, "m", 512, "esc", stubLocal, stats)
	if _, ok := rt(context.Background(), core.Request{Task: core.TaskVQA}, "esc"); ok || chatCalled {
		t.Fatalf("unsupported task must skip before any remote call (ok=%v chatCalled=%v)", ok, chatCalled)
	}
	if stats.Unpromptable != 1 || stats.Unadaptable != 0 || stats.Remote != 0 {
		t.Fatalf("un-promptable task must count as unpromptable (free, no remote call): %+v", stats)
	}
}

func TestWrapRunTier_NilLocalPanics(t *testing.T) {
	// A nil local would silently starve the B1 router/kNN feed — a programming
	// error that must fail at wiring time, not impersonate un-judgeable items.
	defer func() {
		if recover() == nil {
			t.Fatal("nil local must panic at construction")
		}
	}()
	_ = WrapRunTier(chatReturning("x", false, nil), "m", 512, "esc", nil, nil)
}
