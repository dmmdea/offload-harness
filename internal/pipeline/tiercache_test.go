package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/tasks"
)

// fakeSeat is a minimal OpenAI-compatible completion endpoint that counts calls
// and answers every triage request with the same valid verdict, so a "did the
// model run?" assertion is exact rather than inferred from timing.
func fakeSeat(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"verdict\":\"yes\",\"reason\":\"ok\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func tierTestCfg(t *testing.T, endpoint string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Endpoint = endpoint
	cfg.Model = "workhorse"
	cfg.TriageModel = "small"
	cfg.EscalationModel = "big"
	cfg.MaxRetries = 0
	cfg.KNNPreFilterEnabled = false
	cfg.EmbedMemoPath = "" // not under test here
	cfg.ExemplarsDir = ""  // no harvest side-effects
	return cfg
}

func openTierCache(t *testing.T) *cache.Cache {
	t.Helper()
	dir := t.TempDir()
	c, err := cache.Open(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// triageReq is the package's shared fixture (pipeline_reasoning_test.go) — reused
// deliberately so this suite exercises the same request shape as the rest.

// T2-D, the point of the change: the in-loop pipeline must serve a byte-identical
// repeat from the cache instead of re-running the model. Before this, an agent
// that summarized the same file twice in one run paid twice.
func TestInLoopPipelineServesRunTierRepeatsFromCache(t *testing.T) {
	var calls atomic.Int64
	srv := fakeSeat(t, &calls)
	cfg := tierTestCfg(t, srv.URL)
	p := NewInLoopPipeline(cfg, 5*time.Second, openTierCache(t))

	req := triageReq()
	r1, ok1 := p.RunTier(context.Background(), req, cfg.TriageModel)
	if !ok1 {
		t.Fatalf("first RunTier failed: %+v", r1)
	}
	if r1.Meta.CacheHit {
		t.Fatal("the FIRST call cannot be a cache hit")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("model calls after first RunTier = %d, want 1", got)
	}

	r2, ok2 := p.RunTier(context.Background(), req, cfg.TriageModel)
	if !ok2 {
		t.Fatalf("second RunTier failed: %+v", r2)
	}
	if !r2.Meta.CacheHit {
		t.Fatal("the second identical RunTier must be a cache hit")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("model calls after second RunTier = %d, want still 1 — the cache did not prevent the call", got)
	}
	if string(r2.Data) != string(r1.Data) {
		t.Errorf("cached data %s != original %s", r2.Data, r1.Data)
	}
}

// THE LOAD-BEARING INVARIANT. RunTier is shared with the shadow-labelling
// flywheel, which drives it on the MAIN pipeline — the one with an open cache —
// to evaluate what a counterfactual tier WOULD have answered. If that read the
// cache, the flywheel would grade a stored answer instead of the tier, and if it
// wrote, it would fill the store with counterfactual results. Cache
// participation must therefore be a property of the pipeline, never of RunTier.
func TestMainPipelineRunTierNeverTouchesTheCache(t *testing.T) {
	var calls atomic.Int64
	srv := fakeSeat(t, &calls)
	cfg := tierTestCfg(t, srv.URL)
	ca := openTierCache(t)

	// A main-style pipeline: cache present, ledger nil, tierCache NOT set.
	oc := llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 5*time.Second)
	p := New(cfg, oc, ca, nil)
	if p.tierCache {
		t.Fatal("a pipeline built with New must not opt into per-tier caching")
	}

	req := triageReq()
	for i := 0; i < 3; i++ {
		r, ok := p.RunTier(context.Background(), req, cfg.TriageModel)
		if !ok {
			t.Fatalf("RunTier %d failed: %+v", i, r)
		}
		if r.Meta.CacheHit {
			t.Fatalf("call %d reported a cache hit; the flywheel would grade a stored answer, not the tier", i)
		}
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("model calls = %d, want 3 — every counterfactual evaluation must actually run the tier", got)
	}
	// And nothing may have been written either: a later in-loop caller must not
	// find a counterfactual result waiting for it.
	built, err := tasks.Build(req)
	if err != nil {
		t.Fatalf("tasks.Build: %v", err)
	}
	ck := cacheKeyFor(req.Task, req.Input, tasks.StableParamsKey(req.Params), cfg.TriageModel, built, nil)
	if _, ok := ca.Get(ck); ok {
		t.Fatal("the main pipeline WROTE a counterfactual RunTier result into the shared cache")
	}
}

// Two different tiers answering the same input are two different answers. The old
// key here hashed p.cfg.Model, so both tiers shared one entry — harmless only
// while nothing read the key, which is exactly what this change changes.
func TestRunTierKeyIsPerTierNotPerPrimaryModel(t *testing.T) {
	var calls atomic.Int64
	srv := fakeSeat(t, &calls)
	cfg := tierTestCfg(t, srv.URL)
	p := NewInLoopPipeline(cfg, 5*time.Second, openTierCache(t))

	req := triageReq()
	if _, ok := p.RunTier(context.Background(), req, cfg.TriageModel); !ok {
		t.Fatal("small-tier call failed")
	}
	r, ok := p.RunTier(context.Background(), req, cfg.EscalationModel)
	if !ok {
		t.Fatal("big-tier call failed")
	}
	if r.Meta.CacheHit {
		t.Fatal("the big tier was served the small tier's cached answer")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("model calls = %d, want 2 — each tier must be run on its own", got)
	}
}

// The T2-A bug class, on the T2-D path: editing a task's prompt template must
// make prior entries unreachable. The former hand-rolled key here predated that
// fix, so reviving it as-is would have reinstated stale-prompt serving on a new
// path. Mutating Built and re-deriving the key at the REAL call site is what
// proves the ingredient is live, rather than mirroring the formula.
func TestRunTierKeyBindsThePromptTemplate(t *testing.T) {
	req := triageReq()
	built, err := tasks.Build(req)
	if err != nil {
		t.Fatalf("tasks.Build: %v", err)
	}
	paramsKey := tasks.StableParamsKey(req.Params)
	base := cacheKeyFor(req.Task, req.Input, paramsKey, "small", built, nil)

	edited := built
	edited.User = built.User + "\nAlways answer in French."
	if got := cacheKeyFor(req.Task, req.Input, paramsKey, "small", edited, nil); got == base {
		t.Fatal("editing the USER template did not change the key — stale answers would be served forever")
	}
	edited2 := built
	edited2.System = built.System + " Be terse."
	if got := cacheKeyFor(req.Task, req.Input, paramsKey, "small", edited2, nil); got == base {
		t.Fatal("editing the SYSTEM prompt did not change the key")
	}
	edited3 := built
	edited3.Grammar = built.Grammar + " "
	if got := cacheKeyFor(req.Task, req.Input, paramsKey, "small", edited3, nil); got == base {
		t.Fatal("editing the grammar did not change the key")
	}
}

// A defer is not an answer. Caching one would make a single low-confidence or
// ungrounded outcome permanent for that input.
func TestDeferredRunTierResultIsNotCached(t *testing.T) {
	var calls atomic.Int64
	// This seat returns prose, which fails the verifier — the result is a defer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"I am not going to answer in JSON."},"finish_reason":"stop"}],"usage":{}}`))
	}))
	t.Cleanup(srv.Close)

	cfg := tierTestCfg(t, srv.URL)
	ca := openTierCache(t)
	p := NewInLoopPipeline(cfg, 5*time.Second, ca)

	req := triageReq()
	if r, ok := p.RunTier(context.Background(), req, cfg.TriageModel); ok {
		t.Fatalf("expected a defer, got ok result: %+v", r)
	}
	built, err := tasks.Build(req)
	if err != nil {
		t.Fatalf("tasks.Build: %v", err)
	}
	ck := cacheKeyFor(req.Task, req.Input, tasks.StableParamsKey(req.Params), cfg.TriageModel, built, nil)
	if _, ok := ca.Get(ck); ok {
		t.Fatal("a deferred result was cached — the defer would become permanent for this input")
	}
	// A retry must reach the model again.
	before := calls.Load()
	_, _ = p.RunTier(context.Background(), req, cfg.TriageModel)
	if calls.Load() <= before {
		t.Fatal("the retry did not reach the model")
	}
}

// NewInLoopOffload with a nil cache must be byte-for-byte the old behaviour, so
// callers that MUST stay cache-free (prompt A/B arms) have a safe construction.
func TestInLoopOffloadWithNilCacheDoesNotCache(t *testing.T) {
	var calls atomic.Int64
	srv := fakeSeat(t, &calls)
	cfg := tierTestCfg(t, srv.URL)
	p := NewInLoopPipeline(cfg, 5*time.Second, nil)
	if p.cache != nil {
		t.Fatal("a nil cache handle must leave the pipeline cache-free")
	}
	req := triageReq()
	for i := 0; i < 2; i++ {
		if _, ok := p.RunTier(context.Background(), req, cfg.TriageModel); !ok {
			t.Fatalf("call %d failed", i)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("model calls = %d, want 2", got)
	}
}

// The ledger must stay pristine on the in-loop path — that is invariant (a), the
// half of "recordless" this change deliberately keeps.
func TestInLoopPipelineStillHasNoLedger(t *testing.T) {
	cfg := tierTestCfg(t, "http://127.0.0.1:1")
	p := NewInLoopPipeline(cfg, time.Second, openTierCache(t))
	if p.led != nil {
		t.Fatal("the in-loop pipeline must keep a nil ledger; savings accounting is not the cache's business")
	}
	if !p.tierCache {
		t.Fatal("the in-loop pipeline must opt into per-tier caching")
	}
}

// Identity fields must be populated on this path too, or the loupe's duplicate
// analysis silently omits every agent-loop call.
func TestRunTierRecordsCallIdentity(t *testing.T) {
	var calls atomic.Int64
	srv := fakeSeat(t, &calls)
	cfg := tierTestCfg(t, srv.URL)
	p := NewInLoopPipeline(cfg, 5*time.Second, openTierCache(t))

	r, ok := p.RunTier(context.Background(), triageReq(), cfg.TriageModel)
	if !ok {
		t.Fatalf("RunTier failed: %+v", r)
	}
	if r.Meta.InputSHA256 == "" {
		t.Error("InputSHA256 not recorded on the RunTier path")
	}
	if r.Meta.PromptPrefixSHA256 == "" {
		t.Error("PromptPrefixSHA256 not recorded on the RunTier path")
	}
	if r.Meta.ContextHash == "" {
		t.Error("ContextHash not recorded on the RunTier path")
	}
	// And they must survive a cache hit, or the hit rows become identity-blind.
	r2, _ := p.RunTier(context.Background(), triageReq(), cfg.TriageModel)
	if !r2.Meta.CacheHit {
		t.Fatal("expected a cache hit")
	}
	if r2.Meta.InputSHA256 != r.Meta.InputSHA256 {
		t.Errorf("identity drifted across a cache hit: %q vs %q", r2.Meta.InputSHA256, r.Meta.InputSHA256)
	}
	_ = json.Valid(r2.Data)
}
