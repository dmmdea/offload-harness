package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/shadow"
	"github.com/dmmdea/offload-harness/internal/tasks"
)

// newTestPipelineWithLedger builds a Pipeline wired to the given httptest server,
// writing to a temp ledger file at ledgerPath. Returns the pipeline and a cleanup func.
func newTestPipelineWithLedger(t *testing.T, srv *httptest.Server, ledgerPath string) (*Pipeline, func()) {
	t.Helper()
	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.Model = "gemma4-e4b"
	cfg.TriageModel = "gemma4-e2b"
	cfg.EscalationModel = "gemma4-26b"
	cfg.MaxRetries = 0
	cfg.ThresholdsPath = ""
	cfg.RouterWeightsPath = ""
	cfg.TierOverridesPath = ""
	cfg.ConfHeadLabelsPath = ""
	cfg.CachePath = ""
	cfg.LedgerPath = ledgerPath

	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, nil, led)
	return p, func() { led.Close() }
}

// newTestPipelineWithSideEffects builds a Pipeline with a real cache and a shadow
// queue path configured, in addition to a ledger, so that tests can assert RunTier
// leaves all three untouched.
func newTestPipelineWithSideEffects(t *testing.T, srv *httptest.Server, tmp string) (*Pipeline, func()) {
	t.Helper()
	ledgerPath := filepath.Join(tmp, "ledger.jsonl")
	cachePath := filepath.Join(tmp, "cache.bolt")
	shadowPath := filepath.Join(tmp, "shadow-queue.jsonl")

	cfg := config.Default()
	cfg.Endpoint = srv.URL
	cfg.Model = "gemma4-e4b"
	cfg.TriageModel = "gemma4-e2b"
	cfg.EscalationModel = "gemma4-26b"
	cfg.MaxRetries = 0
	cfg.ThresholdsPath = ""
	cfg.RouterWeightsPath = ""
	cfg.TierOverridesPath = ""
	cfg.ConfHeadLabelsPath = ""
	cfg.CachePath = cachePath
	cfg.LedgerPath = ledgerPath
	cfg.ShadowEnabled = true
	cfg.ShadowRate = 1.0
	cfg.ShadowQueuePath = shadowPath

	led, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	ca, err := cache.Open(cachePath)
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	client := llamaclient.New(srv.URL, cfg.CompletionPath, "", 10*time.Second)
	p := New(cfg, client, ca, led)
	return p, func() {
		led.Close()
		ca.Close()
	}
}

// TestRunTier_NoLedgerWrite asserts that RunTier forces the named tier's full
// grammar+verify gate but writes NOTHING to the savings ledger.
func TestRunTier_NoLedgerWrite(t *testing.T) {
	const forcedModel = "gemma4-e2b"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model != forcedModel {
			http.Error(w, "wrong model: "+body.Model, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Return a valid classify result regardless of which model is asked.
		_, _ = w.Write(fakeChat{
			content:      `{"label":"animal","confidence":0.92}`,
			finishReason: "stop",
			promptTokens: 50,
		}.marshal())
	}))
	defer srv.Close()

	tmp := t.TempDir()
	ledgerPath := filepath.Join(tmp, "ledger.jsonl")

	p, cleanup := newTestPipelineWithLedger(t, srv, ledgerPath)
	defer cleanup()

	req := core.Request{
		Task:   core.TaskClassify,
		Input:  "the cat sat on the mat and looked at the dog with curiosity",
		Params: map[string]any{"labels": []string{"animal", "finance"}},
	}

	// Capture ledger size before (file may not exist yet).
	sizeBefore := int64(0)
	if fi, err := os.Stat(ledgerPath); err == nil {
		sizeBefore = fi.Size()
	}

	res, ok := p.RunTier(context.Background(), req, forcedModel)
	if !ok {
		t.Fatalf("RunTier: expected the forced tier to accept, got defer: %s", res.Reason)
	}

	// THE LOAD-BEARING ASSERTION: ledger must be byte-unchanged.
	sizeAfter := int64(0)
	if fi, err := os.Stat(ledgerPath); err == nil {
		sizeAfter = fi.Size()
	}
	if sizeAfter != sizeBefore {
		t.Fatalf("RunTier wrote to the savings ledger (before=%d bytes, after=%d bytes) — must not", sizeBefore, sizeAfter)
	}
}

// TestRunTier_NormalRunStillRecords verifies that the no-record gate in attempt
// does NOT affect the normal Run path: a successful Run still writes to the ledger.
func TestRunTier_NormalRunStillRecords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeChat{
			content:      `{"label":"finance","confidence":0.88}`,
			finishReason: "stop",
			promptTokens: 60,
		}.marshal())
	}))
	defer srv.Close()

	tmp := t.TempDir()
	ledgerPath := filepath.Join(tmp, "ledger.jsonl")

	p, cleanup := newTestPipelineWithLedger(t, srv, ledgerPath)
	defer cleanup()

	req := core.Request{
		Task:   core.TaskClassify,
		Input:  "the quarterly revenue exceeded expectations this fiscal year across all product lines",
		Params: map[string]any{"labels": []string{"animal", "finance"}},
	}

	res := p.Run(context.Background(), req)
	if !res.OK {
		t.Fatalf("Run: expected accept, got defer: %s", res.Reason)
	}

	// Normal Run MUST write to the ledger.
	fi, err := os.Stat(ledgerPath)
	if err != nil {
		t.Fatalf("ledger file must exist after a successful Run: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("Run did not write to the savings ledger — normal record path broken")
	}
}

// TestRunTier_NoShadowCapture asserts that RunTier does NOT enqueue onto the
// shadow queue even when ShadowEnabled=true and ShadowRate=1. The shadow flywheel
// drives RunTier; feeding the queue it drains would be a feedback loop.
func TestRunTier_NoShadowCapture(t *testing.T) {
	const forcedModel = "gemma4-e2b"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.Model != forcedModel {
			http.Error(w, "wrong model: "+body.Model, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeChat{
			content:      `{"label":"animal","confidence":0.91}`,
			finishReason: "stop",
			promptTokens: 50,
		}.marshal())
	}))
	defer srv.Close()

	tmp := t.TempDir()
	p, cleanup := newTestPipelineWithSideEffects(t, srv, tmp)
	defer cleanup()

	req := core.Request{
		Task:   core.TaskClassify,
		Input:  "the cat sat on the mat and looked at the dog with curiosity",
		Params: map[string]any{"labels": []string{"animal", "finance"}},
	}

	res, ok := p.RunTier(context.Background(), req, forcedModel)
	if !ok {
		t.Fatalf("RunTier: expected accept, got defer: %s", res.Reason)
	}

	// Shadow queue must be empty after a RunTier call (feedback-loop guard).
	items, err := shadow.Drain(p.cfg.ShadowQueuePath)
	if err != nil {
		t.Fatalf("shadow.Drain: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("RunTier enqueued %d shadow item(s) — must not (feedback loop)", len(items))
	}
}

// TestRunTier_NoCacheWrite asserts that RunTier does NOT populate the cache under
// the request's default-model key, so a subsequent normal Run executes the model
// rather than being served a counterfactual-tier answer.
func TestRunTier_NoCacheWrite(t *testing.T) {
	const forcedModel = "gemma4-e2b" // NOT the pipeline default ("gemma4-e4b")

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.Model != forcedModel && body.Model != "gemma4-e4b" {
			http.Error(w, "unexpected model: "+body.Model, http.StatusBadRequest)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeChat{
			content:      `{"label":"animal","confidence":0.91}`,
			finishReason: "stop",
			promptTokens: 50,
		}.marshal())
	}))
	defer srv.Close()

	tmp := t.TempDir()
	p, cleanup := newTestPipelineWithSideEffects(t, srv, tmp)
	defer cleanup()

	req := core.Request{
		Task:   core.TaskClassify,
		Input:  "the cat sat on the mat and looked at the dog with curiosity",
		Params: map[string]any{"labels": []string{"animal", "finance"}},
	}

	// RunTier with a non-default tier.
	res, ok := p.RunTier(context.Background(), req, forcedModel)
	if !ok {
		t.Fatalf("RunTier: expected accept, got defer: %s", res.Reason)
	}

	// Assert no cache entry exists for the default-model key (the key Run uses).
	ck := computeCacheKey(p, req)
	if raw, hit := p.cache.Get(ck); hit {
		t.Fatalf("RunTier wrote cache entry under default-model key (len=%d) — must not; a subsequent Run would serve counterfactual-tier result", len(raw))
	}

	// Confirm a subsequent normal Run hits the model (not a cached RunTier answer).
	callsBefore := calls
	runRes := p.Run(context.Background(), req)
	if !runRes.OK {
		t.Fatalf("Run after RunTier: expected accept, got defer: %s", runRes.Reason)
	}
	if calls == callsBefore {
		t.Fatal("Run after RunTier did not call the model — it returned a cached RunTier result, which must not happen")
	}
}

// computeCacheKey replicates the cache key that Run/RunTier build for a text
// classify request so the test can probe the cache directly.
func computeCacheKey(p *Pipeline, req core.Request) string {
	built, err := tasks.Build(req)
	if err != nil {
		return ""
	}
	return cache.Key(string(req.Task), req.Input, tasks.StableParamsKey(req.Params), p.cfg.Model, built.Grammar)
}
