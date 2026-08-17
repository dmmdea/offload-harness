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

// seatAnswering returns a fake OpenAI-compatible endpoint that answers every
// request with the given verdict and counts calls, so "did the model run?" is an
// exact assertion rather than an inference from timing.
func seatAnswering(t *testing.T, verdict string, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	body := `{"choices":[{"message":{"content":"{\"verdict\":\"` + verdict + `\",\"reason\":\"ok\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fakeSeat(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	return seatAnswering(t, "yes", calls)
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

// mainStylePipeline builds a pipeline the way production does for the MAIN one:
// cache present, tierCache NOT set. This is what the shadow-labelling flywheel
// drives.
func mainStylePipeline(t *testing.T, cfg config.Config, ca *cache.Cache) *Pipeline {
	t.Helper()
	oc := llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 5*time.Second)
	return New(cfg, oc, ca, nil)
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

// THE LOAD-BEARING INVARIANT, against a POPULATED store.
//
// An earlier version of this test ran against a cache nothing ever wrote to, so
// the read assertion was unfalsifiable: with an empty store, a RunTier that
// ignores tierCache entirely still reports CacheHit=false. A mutation that
// dropped the tierCache guard from the read gate passed the whole package.
//
// The production topology shares ONE handle (mcpserver passes s.p.Cache() into
// NewInLoopOffload), so by the time the flywheel evaluates a counterfactual tier
// the store is already full of in-loop answers keyed for that very tier.
func TestMainPipelineRunTierIgnoresAPopulatedSharedCache(t *testing.T) {
	var seededCalls atomic.Int64
	seedSrv := seatAnswering(t, "yes", &seededCalls)
	ca := openTierCache(t)
	req := triageReq()

	// 1. Populate the shared cache through the in-loop pipeline.
	seedCfg := tierTestCfg(t, seedSrv.URL)
	inLoop := NewInLoopPipeline(seedCfg, 5*time.Second, ca)
	if r, ok := inLoop.RunTier(context.Background(), req, seedCfg.TriageModel); !ok {
		t.Fatalf("seeding call failed: %+v", r)
	}
	if seededCalls.Load() != 1 {
		t.Fatalf("seeding made %d calls, want 1", seededCalls.Load())
	}

	// 2. The flywheel now evaluates the SAME tier on the SAME input, through the
	//    main pipeline, against a store that already holds an answer. Point it at
	//    a seat with a DIFFERENT answer so a stale hit is visible in the data.
	var freshCalls atomic.Int64
	freshSrv := seatAnswering(t, "no", &freshCalls)
	mainCfg := tierTestCfg(t, freshSrv.URL)
	mainP := mainStylePipeline(t, mainCfg, ca)
	if mainP.tierCache {
		t.Fatal("a pipeline built with New must not opt into per-tier caching")
	}

	r, ok := mainP.RunTier(context.Background(), req, mainCfg.TriageModel)
	if !ok {
		t.Fatalf("flywheel RunTier failed: %+v", r)
	}
	if r.Meta.CacheHit {
		t.Fatal("the flywheel was served a STORED answer instead of running the tier")
	}
	if freshCalls.Load() != 1 {
		t.Fatalf("the counterfactual tier was not actually run (calls=%d)", freshCalls.Load())
	}
	var got map[string]any
	if err := json.Unmarshal(r.Data, &got); err != nil {
		t.Fatalf("result not JSON: %s", r.Data)
	}
	if got["verdict"] != "no" {
		t.Fatalf("verdict = %v, want the FRESH tier's answer %q — a stale entry was served", got["verdict"], "no")
	}
}

// Two different tiers answering the same input are two different answers.
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

// THE CROSS-PATH COLLISION. Run keys on p.cfg.Model whatever tier answered;
// RunTier keys on the tier it pinned. With ExemplarShots at its default of 0
// every other ingredient coincides, so the two paths land on the SAME key
// whenever the pinned tier is the primary model — which is the default for both
// in-loop drive modes.
//
// Without the recorded-producer check, an in-loop RunTier pinned to the workhorse
// is handed whatever tier the cascade happened to answer with, while meta.Model
// reports the workhorse that never ran.
func TestRunTierRefusesAnEntryProducedByADifferentTier(t *testing.T) {
	var calls atomic.Int64
	srv := seatAnswering(t, "yes", &calls)
	cfg := tierTestCfg(t, srv.URL)
	ca := openTierCache(t)
	req := triageReq()

	built, err := tasks.Build(req)
	if err != nil {
		t.Fatalf("tasks.Build: %v", err)
	}
	// Hand-seed the entry the CASCADE would write: same key RunTier computes when
	// pinned to cfg.Model, but produced by the small tier.
	ck := cacheKeyForTier(req.Task, req.Input, tasks.StableParamsKey(req.Params), cfg.Model, built)
	seeded, _ := json.Marshal(cacheVal{
		Data:     json.RawMessage(`{"verdict":"STALE-FROM-ANOTHER-TIER","reason":"x"}`),
		TokensIn: 5,
		Model:    cfg.TriageModel, // produced by a DIFFERENT tier
	})
	if err := ca.Put(ck, seeded); err != nil {
		t.Fatal(err)
	}

	p := NewInLoopPipeline(cfg, 5*time.Second, ca)
	r, ok := p.RunTier(context.Background(), req, cfg.Model)
	if !ok {
		t.Fatalf("RunTier failed: %+v", r)
	}
	if r.Meta.CacheHit {
		t.Fatal("a pinned tier was served an answer another tier produced")
	}
	if calls.Load() != 1 {
		t.Fatalf("the pinned tier did not actually run (calls=%d)", calls.Load())
	}
	var got map[string]any
	if err := json.Unmarshal(r.Data, &got); err != nil {
		t.Fatalf("result not JSON: %s", r.Data)
	}
	if got["verdict"] == "STALE-FROM-ANOTHER-TIER" {
		t.Fatal("the other tier's cached answer was returned")
	}
	// ...and the SAME tier's entry must still hit, or the fix would have simply
	// disabled caching rather than made it correct.
	r2, _ := p.RunTier(context.Background(), req, cfg.Model)
	if !r2.Meta.CacheHit {
		t.Fatal("a same-tier repeat must still hit; the fix must not disable caching wholesale")
	}
}

// The T2-A bug class on the T2-D path, proven AT THE REAL CALL SITE.
//
// An earlier version derived keys with cacheKeyFor directly, which mirrors the
// formula instead of exercising it: a mutation that dropped Built from RunTier's
// key entirely passed the whole package. Here the cache is seeded under a key
// built from an EDITED template and RunTier must miss it, then seeded under the
// REAL template and RunTier must hit — which only holds if the live call site
// actually folds the template in.
func TestRunTierKeyBindsThePromptTemplateAtTheCallSite(t *testing.T) {
	var calls atomic.Int64
	srv := seatAnswering(t, "yes", &calls)
	cfg := tierTestCfg(t, srv.URL)
	ca := openTierCache(t)
	req := triageReq()

	built, err := tasks.Build(req)
	if err != nil {
		t.Fatalf("tasks.Build: %v", err)
	}
	paramsKey := tasks.StableParamsKey(req.Params)

	// POSITIVE direction first — this is the half that actually falsifies.
	// Seed under the key derived from the REAL Built. The live call site must find
	// it. If RunTier stopped folding the template into its key (or folded in a
	// different one), this entry becomes unreachable and the sentinel never comes
	// back — which is precisely the mutation "drop Built from the key".
	realKey := cacheKeyForTier(req.Task, req.Input, paramsKey, cfg.TriageModel, built)
	sentinel, _ := json.Marshal(cacheVal{
		Data:  json.RawMessage(`{"verdict":"SEEDED-UNDER-REAL-TEMPLATE","reason":"x"}`),
		Model: cfg.TriageModel,
	})
	if err := ca.Put(realKey, sentinel); err != nil {
		t.Fatal(err)
	}
	p := NewInLoopPipeline(cfg, 5*time.Second, ca)
	r, ok := p.RunTier(context.Background(), req, cfg.TriageModel)
	if !ok {
		t.Fatalf("RunTier failed: %+v", r)
	}
	if !r.Meta.CacheHit {
		t.Fatal("an entry keyed with the REAL template was NOT found — the live key does not fold in Built")
	}
	var got map[string]any
	if err := json.Unmarshal(r.Data, &got); err != nil {
		t.Fatalf("result not JSON: %s", r.Data)
	}
	if got["verdict"] != "SEEDED-UNDER-REAL-TEMPLATE" {
		t.Fatalf("verdict = %v — the live key did not match the real-template key", got["verdict"])
	}
	if calls.Load() != 0 {
		t.Fatalf("the model ran (calls=%d) despite a seeded matching entry", calls.Load())
	}

	// NEGATIVE direction — an edited template must NOT reach that entry.
	edited := built
	edited.User = built.User + "\nAlways answer in French."
	editedKey := cacheKeyForTier(req.Task, req.Input, paramsKey, cfg.TriageModel, edited)
	if editedKey == realKey {
		t.Fatal("editing the user template did not change the key — a prompt edit would serve pre-edit answers forever")
	}
}

// A defer is not an answer. Caching one would make a single low-confidence or
// ungrounded outcome permanent for that input.
func TestDeferredRunTierResultIsNotCached(t *testing.T) {
	var calls atomic.Int64
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
	ck := cacheKeyForTier(req.Task, req.Input, tasks.StableParamsKey(req.Params), cfg.TriageModel, built)
	if _, ok := ca.Get(ck); ok {
		t.Fatal("a deferred result was cached — the defer would become permanent for this input")
	}
	before := calls.Load()
	_, _ = p.RunTier(context.Background(), req, cfg.TriageModel)
	if calls.Load() <= before {
		t.Fatal("the retry did not reach the model")
	}
}

// NewInLoopOffload is the constructor both drive modes actually use, so it — not
// only the pipeline beneath it — must be exercised.
func TestInLoopOffloadWithNilCacheDoesNotCache(t *testing.T) {
	var calls atomic.Int64
	srv := fakeSeat(t, &calls)
	cfg := tierTestCfg(t, srv.URL)
	off := NewInLoopOffload(cfg, cfg.TriageModel, 5*time.Second, nil)

	for i := 0; i < 2; i++ {
		out, err := off(context.Background(), "triage", triageReq().Input, triageReq().Params)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if out == "" {
			t.Fatalf("call %d returned empty", i)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("model calls = %d, want 2 — a nil cache must not memoize", got)
	}
}

// ...and with a cache, the same constructor must reuse.
func TestInLoopOffloadWithACacheReuses(t *testing.T) {
	var calls atomic.Int64
	srv := fakeSeat(t, &calls)
	cfg := tierTestCfg(t, srv.URL)
	off := NewInLoopOffload(cfg, cfg.TriageModel, 5*time.Second, openTierCache(t))

	req := triageReq()
	for i := 0; i < 3; i++ {
		if _, err := off(context.Background(), "triage", req.Input, req.Params); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("model calls = %d, want 1 — the in-loop cache did not engage", got)
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

// In-loop entries are stamped, so the share of the cache-hit rate the harness
// generated for itself stays recoverable. Without it that split is unmeasurable
// after the fact — and the cache-hit rate is a gate.
func TestInLoopProvenanceIsStampedAndSurfacedOnAHit(t *testing.T) {
	var calls atomic.Int64
	srv := fakeSeat(t, &calls)
	cfg := tierTestCfg(t, srv.URL)
	ca := openTierCache(t)
	req := triageReq()

	p := NewInLoopPipeline(cfg, 5*time.Second, ca)
	if _, ok := p.RunTier(context.Background(), req, cfg.TriageModel); !ok {
		t.Fatal("seeding call failed")
	}
	r, _ := p.RunTier(context.Background(), req, cfg.TriageModel)
	if !r.Meta.CacheHit {
		t.Fatal("expected a hit")
	}
	if !r.Meta.CacheHitInLoop {
		t.Fatal("an in-loop-written entry must be reported as such on the hit")
	}

	built, _ := tasks.Build(req)
	ck := cacheKeyForTier(req.Task, req.Input, tasks.StableParamsKey(req.Params), cfg.TriageModel, built)
	raw, ok := ca.Get(ck)
	if !ok {
		t.Fatal("entry missing")
	}
	var cv cacheVal
	if err := json.Unmarshal(raw, &cv); err != nil {
		t.Fatal(err)
	}
	if !cv.InLoop {
		t.Error("stored entry is not stamped in_loop")
	}
	if cv.Model != cfg.TriageModel {
		t.Errorf("stored producer = %q, want %q", cv.Model, cfg.TriageModel)
	}
}

// Run and RunTier must occupy DISJOINT keyspaces. Guarding only the read left
// both paths writing the same key: Run stored an E2B answer, RunTier refused it,
// ran the workhorse and overwrote the entry, Run then served the workhorse
// answer, and the two ping-ponged one key forever — collapsing the hit rate with
// nothing recording it, since in-loop calls run with a nil ledger.
func TestRunAndRunTierNeverComputeTheSameKey(t *testing.T) {
	cfg := tierTestCfg(t, "http://127.0.0.1:1")
	req := triageReq()
	built, err := tasks.Build(req)
	if err != nil {
		t.Fatal(err)
	}
	paramsKey := tasks.StableParamsKey(req.Params)

	// The exact coincidence that used to collide: the pinned tier IS the primary
	// model, no exemplars, identical Built.
	runKey := cacheKeyFor(req.Task, req.Input, paramsKey, cfg.Model, built, nil)
	tierKey := cacheKeyForTier(req.Task, req.Input, paramsKey, cfg.Model, built)
	if runKey == tierKey {
		t.Fatal("Run and RunTier compute the same key — they will overwrite each other's entries")
	}
	// And RunTier still separates its own tiers.
	if cacheKeyForTier(req.Task, req.Input, paramsKey, cfg.TriageModel, built) == tierKey {
		t.Fatal("RunTier keys collide across tiers")
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
}
