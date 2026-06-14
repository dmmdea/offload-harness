package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/local-offload-pp-cli/internal/confhead"
	"github.com/dmmdea/local-offload-pp-cli/internal/config"
	"github.com/dmmdea/local-offload-pp-cli/internal/core"
	"github.com/dmmdea/local-offload-pp-cli/internal/ledger"
	"github.com/dmmdea/local-offload-pp-cli/internal/llamaclient"
)

// summaryInput is comfortably above the trivial-input floor so the call is
// actually offloaded (not deferred as too-small).
const summaryInput = "The quarterly report covers revenue, churn, hiring plans, and the roadmap for the next two product cycles across all three regional teams, with detailed appendices on cost structure and competitive positioning relative to the incumbent vendors."

// fitDeterministicSummarizeHead builds a confhead whose Predict("summarize", ...)
// is deterministic: rows with margin>=0.9 are correct, margin<=0.1 are wrong.
// The head's exact value isn't asserted — the test chooses τ relative to the
// returned p so the gate decision is unambiguous.
func fitDeterministicSummarizeHead(t *testing.T) *confhead.Model {
	t.Helper()
	var es []ledger.Entry
	for i := 0; i < 200; i++ {
		good := i%2 == 0
		m := 0.95
		g := true
		if !good {
			m = 0.05
			g = false
		}
		es = append(es, ledger.Entry{
			Task: "summarize", Margin: m, Retries: 0, InputChars: 200,
			Feat:     map[string]float64{"len_chars": 200, "n_words": 30, "n_numbers": 1, "n_caps": 2, "has_code": 0, "has_url": 0},
			Grounded: bptrP(g),
		})
	}
	m := confhead.Fit(es)
	if m.Predict("summarize", map[string]float64{}) == -1 {
		t.Fatal("expected a trained summarize head")
	}
	return m
}

func bptrP(v bool) *bool { return &v }

// confheadServer returns an httptest server whose workhorse (cfg.Model) and
// escalation tier (cfg.EscalationModel) emit DISTINGUISHABLE valid summaries, so
// the test can tell which tier produced the accepted result.
func confheadServer(t *testing.T, workhorse, escalation string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch body.Model {
		case workhorse:
			_, _ = w.Write(fakeChat{content: `{"summary":"WORKHORSE summary"}`, finishReason: "stop", promptTokens: 100}.marshal())
		case escalation:
			_, _ = w.Write(fakeChat{content: `{"summary":"ESCALATION summary"}`, finishReason: "stop", promptTokens: 120}.marshal())
		default:
			http.Error(w, "unexpected model "+body.Model, http.StatusBadRequest)
		}
	}))
}

func summaryOf(t *testing.T, data json.RawMessage) string {
	t.Helper()
	var s struct {
		Summary string `json:"summary"`
	}
	if json.Unmarshal(data, &s) != nil {
		t.Fatalf("unmarshal result: %s", data)
	}
	return s.Summary
}

// baseConfheadCfg returns a Default config wired to the test server with self-
// learning file paths disabled so only the confhead path under test is active.
func baseConfheadCfg(srv *httptest.Server, workhorse, escalation string) config.Config {
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.Model = workhorse
	cfg.EscalationModel = escalation
	cfg.MaxRetries = 0
	cfg.ThresholdsPath = ""
	cfg.RouterWeightsPath = ""
	cfg.TierOverridesPath = ""
	cfg.ConfHeadLabelsPath = ""
	cfg.CachePath = ""
	cfg.LedgerPath = ""
	return cfg
}

// TestConfheadGateEscalates: enabled + head predicts p(correct) below τ for a
// summarize call -> Run escalates and returns the ESCALATION tier's output.
func TestConfheadGateEscalates(t *testing.T) {
	const workhorse, escalation = "fake-e4b", "fake-26b"
	srv := confheadServer(t, workhorse, escalation)
	defer srv.Close()

	cfg := baseConfheadCfg(srv, workhorse, escalation)
	cfg.ConfHeadEnabled = true

	head := fitDeterministicSummarizeHead(t)
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)
	// test seam: inject the head + a threshold ABOVE the predicted p(correct) so
	// the gate fires. Summaries carry no logprobs => the head's prediction (not
	// the margin gate, which is summarize-inert) is the only escalation trigger.
	p.confhead = head
	p.confThresholds = map[string]float64{"summarize": 1.0} // p(correct) < 1.0 always -> fire

	res := p.Run(context.Background(), core.Request{Task: core.TaskSummarize, Input: summaryInput})
	if !res.OK {
		t.Fatalf("expected escalation to accept, got defer: %s", res.Reason)
	}
	if got := summaryOf(t, res.Data); got != "ESCALATION summary" {
		t.Fatalf("expected the escalation tier's output, got %q", got)
	}
	if res.Meta.Escalations == 0 {
		t.Fatalf("expected an escalation (Escalations>0), got %d", res.Meta.Escalations)
	}
}

// TestConfheadDisabledNoEscalation: same wiring but ConfHeadEnabled=false ->
// New leaves the head nil, the workhorse output is returned unchanged.
func TestConfheadDisabledNoEscalation(t *testing.T) {
	const workhorse, escalation = "fake-e4b", "fake-26b"
	srv := confheadServer(t, workhorse, escalation)
	defer srv.Close()

	cfg := baseConfheadCfg(srv, workhorse, escalation)
	cfg.ConfHeadEnabled = false

	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)
	if p.confhead != nil {
		t.Fatalf("disabled: confhead must be nil, got %v", p.confhead)
	}

	res := p.Run(context.Background(), core.Request{Task: core.TaskSummarize, Input: summaryInput})
	if !res.OK {
		t.Fatalf("expected accept, got defer: %s", res.Reason)
	}
	if got := summaryOf(t, res.Data); got != "WORKHORSE summary" {
		t.Fatalf("disabled: expected the workhorse output, got %q", got)
	}
	if res.Meta.Escalations != 0 {
		t.Fatalf("disabled: expected no escalation, got %d", res.Meta.Escalations)
	}
}

// TestConfheadAboveThresholdNoEscalation: head predicts p(correct) ABOVE τ ->
// the workhorse output is accepted, no escalation.
func TestConfheadAboveThresholdNoEscalation(t *testing.T) {
	const workhorse, escalation = "fake-e4b", "fake-26b"
	srv := confheadServer(t, workhorse, escalation)
	defer srv.Close()

	cfg := baseConfheadCfg(srv, workhorse, escalation)
	cfg.ConfHeadEnabled = true

	head := fitDeterministicSummarizeHead(t)
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, nil)
	p.confhead = head
	// τ = 0.0 -> p(correct) >= 0 always, so pc < τ is never true -> never fires.
	p.confThresholds = map[string]float64{"summarize": 0.0}

	res := p.Run(context.Background(), core.Request{Task: core.TaskSummarize, Input: summaryInput})
	if !res.OK {
		t.Fatalf("expected accept, got defer: %s", res.Reason)
	}
	if got := summaryOf(t, res.Data); got != "WORKHORSE summary" {
		t.Fatalf("above threshold: expected the workhorse output, got %q", got)
	}
	if res.Meta.Escalations != 0 {
		t.Fatalf("above threshold: expected no escalation, got %d", res.Meta.Escalations)
	}
}
