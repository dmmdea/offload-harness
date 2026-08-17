package pipeline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/mediahash"
	"github.com/dmmdea/offload-harness/internal/tasks"
)

// These drive the REAL pipeline gates and assert on the REAL cache.
//
// An earlier version of this coverage lived in a file named for the pipeline but
// only ever called mediahash directly — so deleting every `&& identifiable` and
// `&& cacheable` from pipeline.go left the whole suite green. That is assertion
// theatre, and it is the exact refactor the gates exist to survive.

func gateCache(t *testing.T) *cache.Cache {
	t.Helper()
	c, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func gateCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Endpoint = "http://127.0.0.1:1"
	cfg.VisionModel = "test-vlm"
	cfg.STTModel = "test-stt"
	cfg.ExemplarsDir = ""
	return cfg
}

func gatePipeline(t *testing.T, cfg config.Config, ca *cache.Cache) *Pipeline {
	t.Helper()
	oc := llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, time.Second)
	p := New(cfg, oc, ca, nil)
	// Never wait on a real GPU lock in a unit test.
	p.visionGPUWait = 0
	p.visionGPUPoll = time.Millisecond
	return p
}

// countingCacheLen reports how many entries the cache holds for the given key.
func cacheHas(t *testing.T, ca *cache.Cache, key string) bool {
	t.Helper()
	_, ok := ca.Get(key)
	return ok
}

// THE VISION GATE. runVisionGen must not read or write the cache when the caller
// could not establish the source file's content identity.
//
// The blast radius if this gate is removed is larger than the bug it replaced:
// for video_describe the key is cache.Key(task, req.Input+"|"+extra, ...), and
// `extra` is the ONLY carrier of file identity. When cacheable is false `extra`
// is "" — so an ungated write would key every unidentifiable video on the prompt
// alone, colliding all of them.
func TestRunVisionGenSkipsTheCacheWhenNotCacheable(t *testing.T) {
	cfg := gateCfg(t)
	ca := gateCache(t)
	p := gatePipeline(t, cfg, ca)

	req := core.Request{Task: core.TaskVideoDescribe, Input: "describe this clip", Params: map[string]any{"question": "what happens?"}}
	built, err := tasks.Build(req)
	if err != nil {
		t.Fatalf("tasks.Build: %v", err)
	}
	var calls atomic.Int64
	gen := func(context.Context) (llamaclient.GenResult, error) {
		calls.Add(1)
		return llamaclient.GenResult{Content: "a description", TokensIn: 7}, nil
	}
	meta := core.Meta{Model: cfg.VisionModel}

	// cacheable=false: the model runs, and NOTHING is stored — so a repeat runs
	// the model again.
	for i := 0; i < 2; i++ {
		res := p.runVisionGen(context.Background(), req, built, meta, time.Now(), "", false, gen)
		if !res.OK {
			t.Fatalf("call %d deferred: %s", i, res.Reason)
		}
		if res.Meta.CacheHit {
			t.Fatalf("call %d reported a cache hit for an unidentifiable input", i)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("model calls = %d, want 2 — an unidentifiable input was cached", got)
	}
	// And prove it by the store, not only by the call count: the key an ungated
	// write would have used must be absent.
	ungatedKey := visionKeyFor(req, built, meta, "")
	if cacheHas(t, ca, ungatedKey) {
		t.Fatal("an entry was written for an unidentifiable input — every such video would collide on this key")
	}
}

// ...and the same path MUST cache when identity is established, or the "fix"
// would just be a disabled feature.
func TestRunVisionGenCachesWhenCacheable(t *testing.T) {
	cfg := gateCfg(t)
	ca := gateCache(t)
	p := gatePipeline(t, cfg, ca)

	req := core.Request{Task: core.TaskVideoDescribe, Input: "describe this clip", Params: map[string]any{"question": "what happens?"}}
	built, err := tasks.Build(req)
	if err != nil {
		t.Fatalf("tasks.Build: %v", err)
	}
	var calls atomic.Int64
	gen := func(context.Context) (llamaclient.GenResult, error) {
		calls.Add(1)
		return llamaclient.GenResult{Content: "a description", TokensIn: 7}, nil
	}
	meta := core.Meta{Model: cfg.VisionModel}
	const extra = "vid:media:sha256:sz=10:deadbeef|fps=1|n=8|w=512|frames=8"

	first := p.runVisionGen(context.Background(), req, built, meta, time.Now(), extra, true, gen)
	if !first.OK {
		t.Fatalf("first call deferred: %s", first.Reason)
	}
	second := p.runVisionGen(context.Background(), req, built, meta, time.Now(), extra, true, gen)
	if !second.Meta.CacheHit {
		t.Fatal("an identifiable repeat did not hit the cache — the gate disabled caching instead of scoping it")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("model calls = %d, want 1", got)
	}
}

// THE TRANSCRIBE GATE, driven through runTranscribe itself. The identity seams
// let the two failure modes be injected deterministically; without them a missing
// file defers inside ffmpeg long before the gate is reached.
func TestRunTranscribeSkipsTheCacheWhenIdentityFails(t *testing.T) {
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH; this gate needs a real convert to reach the cache decision")
	}
	wav := makeSilentWav(t, ffmpeg)

	for _, tc := range []struct {
		name  string
		wire  func(*Pipeline)
		wants string
	}{
		{
			name: "digest fails",
			wire: func(p *Pipeline) {
				p.mediaDigest = func(string) (mediahash.Ident, error) { return mediahash.Ident{}, errors.New("injected read failure") }
			},
			wants: "injected read failure",
		},
		{
			name:  "file changed during the call",
			wire:  func(p *Pipeline) { p.mediaUnchanged = func(mediahash.Ident, string) bool { return false } },
			wants: "source changed during the call",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := gateCfg(t)
			cfg.MediaDir = t.TempDir()
			cfg.FFmpegPath = ffmpeg
			srv := fakeWhisper(t)
			cfg.Endpoint = srv
			ca := gateCache(t)
			p := gatePipeline(t, cfg, ca)
			tc.wire(p)

			req := core.Request{Task: core.TaskTranscribe, Audio: wav}
			res := p.Run(context.Background(), req)
			if !res.OK {
				t.Fatalf("transcribe deferred before reaching the gate: %s", res.Reason)
			}
			if res.Meta.CacheBypass == "" {
				t.Fatal("a bypassed cache was not recorded — it is indistinguishable from a cold miss")
			}
			if !containsStr(res.Meta.CacheBypass, tc.wants) {
				t.Errorf("CacheBypass = %q, want it to name %q", res.Meta.CacheBypass, tc.wants)
			}
			// The load-bearing assertion: nothing was stored.
			if n := countEntries(t, ca); n != 0 {
				t.Fatalf("%d cache entries written for an unidentifiable input, want 0", n)
			}
		})
	}
}

// The positive control: with identity intact the same path DOES cache, so the
// test above is measuring the gate rather than a broken environment.
func TestRunTranscribeCachesWhenIdentifiable(t *testing.T) {
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

	res := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: wav})
	if !res.OK {
		t.Fatalf("transcribe deferred: %s", res.Reason)
	}
	if res.Meta.CacheBypass != "" {
		t.Fatalf("an identifiable input reported a bypass: %q", res.Meta.CacheBypass)
	}
	if n := countEntries(t, ca); n == 0 {
		t.Fatal("an identifiable input stored nothing — the gate disabled caching instead of scoping it")
	}
}

// ---- helpers ---------------------------------------------------------------

func lookFFmpeg() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

// fakeWhisper stands in for whisper-server so the gate decision is reached
// without a model. Returns the base URL.
func fakeWhisper(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"silence","segments":[{"start":0,"end":1,"text":"silence"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// countEntries reports how many keys the cache holds, so "nothing was stored" is
// asserted against the STORE rather than inferred from a call count.
func countEntries(t *testing.T, ca *cache.Cache) int {
	t.Helper()
	n, err := ca.Count()
	if err != nil {
		t.Fatalf("cache count: %v", err)
	}
	return n
}

// visionKeyFor mirrors runVisionGen's key so a test can assert the ABSENCE of the
// entry an ungated write would have produced.
func visionKeyFor(req core.Request, built tasks.Built, meta core.Meta, extra string) string {
	return cache.Key(string(req.Task), req.Input+"|"+extra, tasks.StableParamsKey(req.Params), meta.Model, built.Grammar,
		templateCacheTag(built.System, built.Grammar, built.User, req.Input))
}

func makeSilentWav(t *testing.T, ffmpeg string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "silence.wav")
	cmd := exec.Command(ffmpeg, "-nostdin", "-f", "lavfi", "-i", "anullsrc=r=16000:cl=mono",
		"-t", "1", "-c:a", "pcm_s16le", "-y", out)
	if err := cmd.Run(); err != nil {
		t.Skipf("could not synthesise a test wav with ffmpeg: %v", err)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Skip("ffmpeg produced no output")
	}
	return out
}

func containsStr(hay, needle string) bool {
	return len(needle) == 0 || (len(hay) >= len(needle) && indexOf(hay, needle) >= 0)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
