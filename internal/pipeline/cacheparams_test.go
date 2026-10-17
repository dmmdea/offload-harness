package pipeline

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/tasks"
)

// cacheParams is the params ingredient of a result-cache key, for the test mirrors of
// the production keys. The params these tests use are keyable; a failure here is a bug
// in the test, so it panics instead of returning an error nobody would read.
func cacheParams(p map[string]any) string {
	k, err := tasks.CacheParamsKey(p)
	if err != nil {
		panic("cacheParams: " + err.Error())
	}
	return k
}

// paramsFromJSON decodes params the way the MCP door does (numbers as float64, lists as
// []any, objects as map[string]any).
func paramsFromJSON(t *testing.T, doc string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("bad params document %q: %v", doc, err)
	}
	return m
}

// repliesWith is a fake chat endpoint that answers every call with content and counts
// the calls, so "did the model run?" is an exact assertion.
func repliesWith(t *testing.T, content string, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fakeChat{content: content, finishReason: "stop", promptTokens: 40}.marshal())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// THE RESULT CACHE MUST NOT BE FORGEABLE THROUGH THE PARAMS. Run's key carried the
// params as StableParamsKey rendered them, and that prints a nested value with %v: type
// erased and space-separated. Two extract requests whose schemas differ only where %v
// cannot see it (a "required" list that names "a b" instead of "a" and "b"; a "minimum"
// that is the string "1" instead of the number 1) had one key, so the second was served
// the first's cached answer before its own schema was ever checked. The pairs below are
// the release review's. Each is driven through Run itself: B stores, then A must MISS
// and the model must run again.
func TestRunKeepsTypeErasedParamsApart(t *testing.T) {
	const schemaTmpl = `{"schema":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":%s}}`
	const numTmpl = `{"schema":{"type":"object","properties":{"n":{"type":"number","minimum":%s}}}}`
	cases := []struct {
		name  string
		b, a  map[string]any // b runs and stores first; a must not be served b's entry
		reply string         // what the model says: valid for b's schema and grounded in the input
	}{
		{
			name:  "required [a, b] stored, required [a b] asks",
			b:     paramsFromJSON(t, strings.Replace(schemaTmpl, "%s", `["a","b"]`, 1)),
			a:     paramsFromJSON(t, strings.Replace(schemaTmpl, "%s", `["a b"]`, 1)),
			reply: `{"a":"Alice","b":"Bob"}`,
		},
		{
			name:  "minimum 1 stored, minimum \"1\" asks",
			b:     paramsFromJSON(t, strings.Replace(numTmpl, "%s", `1`, 1)),
			a:     paramsFromJSON(t, strings.Replace(numTmpl, "%s", `"1"`, 1)),
			reply: `{"n":5}`,
		},
	}
	const input = "Alice met Bob in Paris on Tuesday, and they agreed that the invoice for the delivery would be paid within 5 days."
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Guard on the guard: the pair had one key through StableParamsKey, and the
			// built prompt does not tell the two requests apart either, so nothing but the
			// params ingredient can.
			if tasks.StableParamsKey(tc.a) != tasks.StableParamsKey(tc.b) {
				t.Fatalf("the pair no longer collides under StableParamsKey; pick one that does")
			}

			var calls atomic.Int64
			srv := repliesWith(t, tc.reply, &calls)
			p, cleanup := newTestPipelineWithSideEffects(t, srv, t.TempDir())
			defer cleanup()
			ctx := context.Background()
			reqB := core.Request{Task: core.TaskExtract, Input: input, Params: tc.b}
			reqA := core.Request{Task: core.TaskExtract, Input: input, Params: tc.a}

			first := p.Run(ctx, reqB)
			if !first.OK || first.Meta.CacheHit {
				t.Fatalf("B's first call: ok=%v hit=%v reason=%q", first.OK, first.Meta.CacheHit, first.Reason)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("model calls after B = %d, want 1", got)
			}
			// Not vacuous: B's own repeat hits, so the entry A must not be served exists.
			if rep := p.Run(ctx, reqB); !rep.Meta.CacheHit || calls.Load() != 1 {
				t.Fatalf("B's own repeat did not hit the cache (hit=%v, calls=%d): the test would pass with caching off", rep.Meta.CacheHit, calls.Load())
			}

			before := calls.Load()
			res := p.Run(ctx, reqA)
			if res.Meta.CacheHit {
				t.Fatal("A was served B's cached answer: the params part of the result-cache key does not tell the two requests apart")
			}
			if calls.Load() == before {
				t.Fatal("A never reached the model, so its own schema was never applied to an answer")
			}
		})
	}
}

// FAIL CLOSED. A params value with no faithful typed form (NaN, a function) cannot be
// keyed. The call must then neither read nor write the result cache, record why, and run
// the model every time. Falling back to a %v rendering would give that call a key it can
// share with a different request.
func TestRunSkipsTheResultCacheWhenParamsCannotBeKeyed(t *testing.T) {
	bad := map[string]any{
		"NaN":      math.NaN(),
		"infinity": math.Inf(1),
		"function": func() {},
	}
	for name, v := range bad {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			srv := repliesWith(t, `{"label":"animal","confidence":0.91}`, &calls)
			p, cleanup := newTestPipelineWithSideEffects(t, srv, t.TempDir())
			defer cleanup()
			ctx := context.Background()
			params := map[string]any{"labels": []string{"animal", "finance"}, "note": v}
			req := core.Request{Task: core.TaskClassify, Input: "the cat sat on the mat and looked at the dog with curiosity", Params: params}

			var bypass []string
			for i := 1; i <= 2; i++ {
				res := p.Run(ctx, req)
				if !res.OK {
					t.Fatalf("call %d deferred: %s", i, res.Reason)
				}
				if res.Meta.CacheHit {
					t.Fatalf("call %d was a cache hit for params that cannot be keyed", i)
				}
				bypass = append(bypass, res.Meta.CacheBypass)
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("model calls = %d, want 2: the second identical call must run the model too", got)
			}
			if n, _ := p.cache.Count(); n != 0 {
				t.Fatalf("%d entries stored for params that cannot be keyed, want 0", n)
			}
			for i, b := range bypass {
				if !strings.Contains(b, "params") {
					t.Fatalf("call %d: CacheBypass = %q, want it to say the params could not be keyed", i+1, b)
				}
			}

			// Not vacuous: the same request without the bad value is cached and hit.
			good := core.Request{Task: core.TaskClassify, Input: req.Input, Params: map[string]any{"labels": []string{"animal", "finance"}}}
			p.Run(ctx, good)
			if rep := p.Run(ctx, good); !rep.Meta.CacheHit {
				t.Fatal("the same request with keyable params did not hit the cache: the test would pass with caching off")
			}
		})
	}
}

// ...on the RunTier path (the in-loop pipeline, the one that caches RunTier answers)...
func TestRunTierSkipsTheResultCacheWhenParamsCannotBeKeyed(t *testing.T) {
	var calls atomic.Int64
	srv := fakeSeat(t, &calls)
	cfg := tierTestCfg(t, srv.URL)
	ca := openTierCache(t)
	p := NewInLoopPipeline(cfg, 5*time.Second, ca)

	req := triageReq()
	params := map[string]any{"note": math.NaN()}
	for k, v := range req.Params {
		params[k] = v
	}
	req.Params = params

	var bypass []string
	for i := 1; i <= 2; i++ {
		res, ok := p.RunTier(context.Background(), req, cfg.TriageModel)
		if !ok {
			t.Fatalf("RunTier %d failed: %+v", i, res)
		}
		if res.Meta.CacheHit {
			t.Fatalf("RunTier %d was a cache hit for params that cannot be keyed", i)
		}
		bypass = append(bypass, res.Meta.CacheBypass)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("model calls = %d, want 2", got)
	}
	if n, _ := ca.Count(); n != 0 {
		t.Fatalf("%d entries stored for params that cannot be keyed, want 0", n)
	}
	for i, b := range bypass {
		if !strings.Contains(b, "params") {
			t.Fatalf("RunTier %d: CacheBypass = %q, want it to say the params could not be keyed", i+1, b)
		}
	}
}

// ...and on the vision path.
func TestRunVisionGenSkipsTheResultCacheWhenParamsCannotBeKeyed(t *testing.T) {
	cfg := gateCfg(t)
	ca := gateCache(t)
	p := gatePipeline(t, cfg, ca)

	var calls atomic.Int64
	gen := func(context.Context) (llamaclient.GenResult, error) {
		calls.Add(1)
		return llamaclient.GenResult{Content: "a description", TokensIn: 7}, nil
	}
	meta := core.Meta{Model: cfg.VisionModel}
	req := core.Request{Task: core.TaskVideoDescribe, Input: "describe this clip", Params: map[string]any{"question": "what happens?", "note": math.NaN()}}
	built, err := tasks.Build(req)
	if err != nil {
		t.Fatalf("tasks.Build: %v", err)
	}
	const extra = "vid:media:sha256:sz=10:deadbeef|fps=1|n=8|w=512|frames=8"

	var bypass []string
	for i := 1; i <= 2; i++ {
		res := p.runVisionGen(context.Background(), req, built, meta, time.Now(), extra, true, gen)
		if !res.OK {
			t.Fatalf("call %d deferred: %s", i, res.Reason)
		}
		if res.Meta.CacheHit {
			t.Fatalf("call %d was a cache hit for params that cannot be keyed", i)
		}
		bypass = append(bypass, res.Meta.CacheBypass)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("model calls = %d, want 2", got)
	}
	if n := countEntries(t, ca); n != 0 {
		t.Fatalf("%d entries stored for params that cannot be keyed, want 0", n)
	}
	for i, b := range bypass {
		if !strings.Contains(b, "params") {
			t.Fatalf("call %d: CacheBypass = %q, want it to say the params could not be keyed", i+1, b)
		}
	}
}

// ...and on the transcribe path.
func TestRunTranscribeSkipsTheResultCacheWhenParamsCannotBeKeyed(t *testing.T) {
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH")
	}
	wav := makeSilentWav(t, ffmpeg)
	cfg := gateCfg(t)
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = ffmpeg
	cfg.Endpoint = fakeWhisper(t)
	ca := gateCache(t)
	p := gatePipeline(t, cfg, ca)

	var bypass []string
	for i := 1; i <= 2; i++ {
		res := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: wav, Params: map[string]any{"note": math.NaN()}})
		if !res.OK {
			t.Fatalf("call %d deferred: %s", i, res.Reason)
		}
		if res.Meta.CacheHit {
			t.Fatalf("call %d was a cache hit for params that cannot be keyed", i)
		}
		bypass = append(bypass, res.Meta.CacheBypass)
	}
	if n := countEntries(t, ca); n != 0 {
		t.Fatalf("%d entries stored for params that cannot be keyed, want 0", n)
	}
	for i, b := range bypass {
		if !strings.Contains(b, "params") {
			t.Fatalf("call %d: CacheBypass = %q, want it to say the params could not be keyed", i+1, b)
		}
	}
}
