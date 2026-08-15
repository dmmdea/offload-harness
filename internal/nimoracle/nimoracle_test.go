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

func TestAdaptClassify_NoLabelsParamAcceptsAsIs(t *testing.T) {
	req := core.Request{Task: core.TaskClassify}
	res, ok := Adapt(core.TaskClassify, req, `{"label": "spam"}`)
	if !ok || !strings.Contains(string(res.Data), "spam") {
		t.Fatalf("want label passthrough without a label set, got ok=%v data=%s", ok, res.Data)
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

func TestAdaptSummarize_ProseFallback(t *testing.T) {
	res, ok := Adapt(core.TaskSummarize, core.Request{Task: core.TaskSummarize}, "  A plain prose summary.  ")
	if !ok {
		t.Fatal("expected ok")
	}
	var m map[string]string
	_ = json.Unmarshal(res.Data, &m)
	if m["summary"] != "A plain prose summary." {
		t.Fatalf("got %q", m["summary"])
	}
}

func TestAdaptSummarize_EmptyRejected(t *testing.T) {
	if _, ok := Adapt(core.TaskSummarize, core.Request{Task: core.TaskSummarize}, "   "); ok {
		t.Fatal("empty summary must be rejected")
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

func TestWrapRunTier_OracleAliasGoesRemote(t *testing.T) {
	localCalled := false
	local := func(ctx context.Context, req core.Request, model string) (core.Result, bool) {
		localCalled = true
		return core.Result{}, false
	}
	rt := WrapRunTier(chatReturning(`{"decision":"no"}`, false, nil), "big-model", 512, "esc-alias", local)
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
	rt := WrapRunTier(chat, "big-model", 512, "esc-alias", local)
	res, ok := rt(context.Background(), core.Request{Task: core.TaskTriage}, "gemma-4-e2b")
	if !ok || remoteCalled {
		t.Fatalf("E2B counterfactual must stay local (ok=%v remoteCalled=%v)", ok, remoteCalled)
	}
	if !strings.Contains(string(res.Data), "yes") {
		t.Fatalf("local result not passed through: %s", res.Data)
	}
}

func TestWrapRunTier_ChatErrorSkips(t *testing.T) {
	rt := WrapRunTier(chatReturning("", false, errors.New("boom")), "m", 512, "esc", nil)
	if _, ok := rt(context.Background(), core.Request{Task: core.TaskTriage}, "esc"); ok {
		t.Fatal("chat error must yield ok=false")
	}
}

func TestWrapRunTier_TruncatedSkips(t *testing.T) {
	rt := WrapRunTier(chatReturning(`{"decision":"yes"}`, true, nil), "m", 512, "esc", nil)
	if _, ok := rt(context.Background(), core.Request{Task: core.TaskTriage}, "esc"); ok {
		t.Fatal("truncated reply must yield ok=false")
	}
}

func TestWrapRunTier_UnsupportedTaskSkipsWithoutChat(t *testing.T) {
	chatCalled := false
	chat := func(ctx context.Context, model, system, user string, maxTokens int, temperature float64) (nimclient.ChatResult, error) {
		chatCalled = true
		return nimclient.ChatResult{Content: "x"}, nil
	}
	rt := WrapRunTier(chat, "m", 512, "esc", nil)
	if _, ok := rt(context.Background(), core.Request{Task: core.TaskVQA}, "esc"); ok || chatCalled {
		t.Fatalf("unsupported task must skip before any remote call (ok=%v chatCalled=%v)", ok, chatCalled)
	}
}

func TestWrapRunTier_NilLocalNonOracleSkips(t *testing.T) {
	rt := WrapRunTier(chatReturning("x", false, nil), "m", 512, "esc", nil)
	if _, ok := rt(context.Background(), core.Request{Task: core.TaskTriage}, "other"); ok {
		t.Fatal("nil local + non-oracle model must yield ok=false, not panic")
	}
}
