package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// reasoningServer: the entry (TriageModel) + workhorse (Model) tiers DEFER (empty output);
// the reasoning model returns reasonContent (a <think>..</think>-prefixed payload). reasonHits
// counts calls to the reasoning model so a defer can be distinguished from "reasoning never ran".
func reasoningServer(t *testing.T, triage, model, reason, reasonContent string, reasonHits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch body.Model {
		case triage, model:
			_, _ = w.Write(fakeChat{content: "", finishReason: "stop", promptTokens: 50}.marshal()) // empty -> defer
		case reason:
			if reasonHits != nil {
				*reasonHits++
			}
			_, _ = w.Write(fakeChat{content: reasonContent, finishReason: "stop", promptTokens: 60}.marshal())
		default:
			http.Error(w, "unexpected model "+body.Model, http.StatusBadRequest)
		}
	}))
}

func reasoningCfg(srv *httptest.Server, triage, model, reason string) config.Config {
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.TriageModel = triage
	cfg.Model = model
	cfg.EscalationModel = "" // chain = [triage, model]; both defer -> reasoning tier reached
	cfg.ReasoningModel = reason
	cfg.MaxRetries = 0
	cfg.ThresholdsPath = ""
	cfg.RouterWeightsPath = ""
	cfg.TierOverridesPath = ""
	cfg.ConfHeadLabelsPath = ""
	cfg.CachePath = ""
	cfg.LedgerPath = ""
	return cfg
}

func triageReq() core.Request {
	return core.Request{
		Task:   core.TaskTriage,
		Input:  "The customer reports the invoice was charged twice and wants a refund processed today.",
		Params: map[string]any{"question": "Is this a billing issue?"},
	}
}

func classifyReq() core.Request {
	return core.Request{
		Task:   core.TaskClassify,
		Input:  "I was double-charged this month and want the duplicate refunded.",
		Params: map[string]any{"labels": []string{"billing", "technical", "account"}},
	}
}

// TestReasoningSkippedOnTruncation: a truncation-TERMINAL cascade defer must NOT fire the
// reasoning tier (it would re-run the same oversized input and only truncate again).
func TestReasoningSkippedOnTruncation(t *testing.T) {
	const model, reason = "fake-model", "fake-reason"
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch body.Model {
		case model:
			_, _ = w.Write(fakeChat{content: `{"decision":"ye`, finishReason: "length", promptTokens: 50}.marshal()) // truncated -> terminal defer
		case reason:
			hits++
			_, _ = w.Write(fakeChat{content: `<think>x</think>{"decision":"yes","reason":"r"}`, finishReason: "stop"}.marshal())
		default:
			http.Error(w, "unexpected model "+body.Model, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	cfg := reasoningCfg(srv, "", model, reason) // TriageModel="" -> chain = [model]
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)
	res := p.Run(context.Background(), triageReq())
	if hits != 0 {
		t.Fatalf("reasoning tier must be SKIPPED after a truncation-terminal defer; it ran (%d hits)", hits)
	}
	if res.OK {
		t.Fatal("expected a defer (truncation)")
	}
}

// TestReasoningClassifyLowConfidenceDefers: a structurally-valid classify answer whose
// self-reported confidence is below the threshold must DEFER at the reasoning tier (same as
// the cascade would), not be accepted.
func TestReasoningClassifyLowConfidenceDefers(t *testing.T) {
	const triage, model, reason = "fake-triage", "fake-model", "fake-reason"
	hits := 0
	srv := reasoningServer(t, triage, model, reason, `<think>not sure</think>{"label":"billing","confidence":0.30}`, &hits)
	defer srv.Close()

	cfg := reasoningCfg(srv, triage, model, reason)
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)
	res := p.Run(context.Background(), classifyReq())
	if hits == 0 {
		t.Fatal("reasoning tier not reached")
	}
	if res.OK {
		t.Fatalf("expected a defer on low classify self-confidence (0.30 < 0.45), got OK: %s", res.Data)
	}
}

// TestReasoningBudgetHeadroom: the wrapped call must get extra token budget so the think span
// plus the JSON both fit (classify's native budget is tiny, 64).
func TestReasoningBudgetHeadroom(t *testing.T) {
	const triage, model, reason = "fake-triage", "fake-model", "fake-reason"
	maxSeen := -1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch body.Model {
		case triage, model:
			_, _ = w.Write(fakeChat{content: "", finishReason: "stop", promptTokens: 50}.marshal())
		case reason:
			maxSeen = body.MaxTokens
			_, _ = w.Write(fakeChat{content: `<think>x</think>{"label":"billing","confidence":0.95}`, finishReason: "stop"}.marshal())
		default:
			http.Error(w, "unexpected model "+body.Model, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	cfg := reasoningCfg(srv, triage, model, reason)
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)
	_ = p.Run(context.Background(), classifyReq())
	if maxSeen < 512 {
		t.Fatalf("reasoning call needs token headroom for the think span; got max_tokens=%d (want >= 512)", maxSeen)
	}
}

// TestReasoningTierAccepts: entry + workhorse defer; the reasoning model reasons under a
// think-wrapped grammar and emits valid structured output, which the tier strips + accepts.
func TestReasoningTierAccepts(t *testing.T) {
	const triage, model, reason = "fake-triage", "fake-model", "fake-reason"
	hits := 0
	srv := reasoningServer(t, triage, model, reason,
		`<think>Charged twice and a refund is requested - that is a billing matter.</think>{"decision":"yes","reason":"charged twice, refund requested"}`, &hits)
	defer srv.Close()

	cfg := reasoningCfg(srv, triage, model, reason)
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)

	res := p.Run(context.Background(), triageReq())
	if !res.OK {
		t.Fatalf("expected the reasoning tier to accept, got defer: %s", res.Reason)
	}
	if hits == 0 {
		t.Fatal("reasoning model was never called")
	}
	if res.Meta.Model != reason {
		t.Fatalf("expected the result from the reasoning model, got model %q", res.Meta.Model)
	}
	var d struct {
		Decision string `json:"decision"`
	}
	if json.Unmarshal(res.Data, &d) != nil || d.Decision != "yes" {
		t.Fatalf("expected decision=yes (think span stripped), got %s", res.Data)
	}
}

// TestReasoningTierMarksMeta: a reclaim must flag Meta.Reasoning so the ledger entry (and the
// `ledger`/`stats` reports) can tell a reasoning-tier reclaim apart from a plain escalation-tier
// answer — both run on the SAME model (qwythos) and so share ModelTier.
func TestReasoningTierMarksMeta(t *testing.T) {
	const triage, model, reason = "fake-triage", "fake-model", "fake-reason"
	hits := 0
	srv := reasoningServer(t, triage, model, reason,
		`<think>charged twice, refund requested - billing</think>{"decision":"yes","reason":"charged twice"}`, &hits)
	defer srv.Close()

	cfg := reasoningCfg(srv, triage, model, reason)
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)
	res := p.Run(context.Background(), triageReq())
	if !res.OK {
		t.Fatalf("expected the reasoning tier to accept, got defer: %s", res.Reason)
	}
	if !res.Meta.Reasoning {
		t.Fatal("a reasoning-tier reclaim must set Meta.Reasoning=true (the ledger marker)")
	}
}

// TestReasoningTierGarbageDefers: the reasoning tier ran but its (stripped) output is invalid,
// so Run still DEFERS (the harness then goes to cloud) - it never fabricates a pass.
func TestReasoningTierGarbageDefers(t *testing.T) {
	const triage, model, reason = "fake-triage", "fake-model", "fake-reason"
	hits := 0
	srv := reasoningServer(t, triage, model, reason, `<think>hmm</think> not json at all`, &hits)
	defer srv.Close()

	cfg := reasoningCfg(srv, triage, model, reason)
	p := New(cfg, llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second), nil, nil)

	res := p.Run(context.Background(), triageReq())
	if hits == 0 {
		t.Fatal("reasoning model was never called (tier not reached)")
	}
	if res.OK {
		t.Fatalf("expected a defer on invalid reasoning output, got OK: %s", res.Data)
	}
}
