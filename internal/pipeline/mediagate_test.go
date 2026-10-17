package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/mediahash"
	"github.com/dmmdea/offload-harness/internal/tasks"
	"github.com/dmmdea/offload-harness/internal/winexec"
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
// for video_describe the key is cache.Key(task, req.Input, extra, ...), and
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

// THE VISION KEY BOUNDARY. runVisionGen keys on the caller's req.Input AND the
// media identity (`extra`). Gluing them into one "input|extra" string let two
// different (input, extra) pairs spell the same bytes, so a request whose input
// carried a forged "|" served another request's cached description. Each field is
// its own key part now, and this drives runVisionGen itself so the assertion is on
// the behaviour a caller sees (the second request must run the model), not on a
// mirror of the key formula.
//
// The tokens are unique on purpose: templateCacheTag also hashes the prompt, and a
// token that reappeared in the question would make the second call miss for the
// wrong reason, passing this test against the glued key too.
func TestRunVisionGenKeepsInputAndIdentityApart(t *testing.T) {
	cfg := gateCfg(t)
	ca := gateCache(t)
	p := gatePipeline(t, cfg, ca)

	var calls atomic.Int64
	gen := func(context.Context) (llamaclient.GenResult, error) {
		return llamaclient.GenResult{Content: fmt.Sprintf("answer %d", calls.Add(1)), TokensIn: 7}, nil
	}
	meta := core.Meta{Model: cfg.VisionModel}
	params := map[string]any{"question": "what happens?"}
	reqA := core.Request{Task: core.TaskVideoDescribe, Input: "zxq1|zxq2", Params: params}
	reqB := core.Request{Task: core.TaskVideoDescribe, Input: "zxq1", Params: params}
	const extraA, extraB = "zxq3", "zxq2|zxq3" // "zxq1|zxq2" + "|" + "zxq3" == "zxq1" + "|" + "zxq2|zxq3"

	run := func(req core.Request, extra string) core.Result {
		t.Helper()
		built, err := tasks.Build(req)
		if err != nil {
			t.Fatalf("tasks.Build: %v", err)
		}
		res := p.runVisionGen(context.Background(), req, built, meta, time.Now(), extra, true, gen)
		if !res.OK {
			t.Fatalf("call deferred: %s", res.Reason)
		}
		return res
	}

	first := run(reqA, extraA)
	if first.Meta.CacheHit {
		t.Fatal("the first call cannot be a hit on an empty cache")
	}
	// The mirror the other tests in this file use must be the key the pipeline wrote,
	// or an assertion on its absence elsewhere would pass for the wrong reason.
	builtA, err := tasks.Build(reqA)
	if err != nil {
		t.Fatalf("tasks.Build: %v", err)
	}
	if !cacheHas(t, ca, visionKeyFor(reqA, builtA, meta, extraA)) {
		t.Fatal("the first call did not store under visionKeyFor — the mirror has drifted from runVisionGen")
	}
	second := run(reqB, extraB)
	if second.Meta.CacheHit {
		t.Fatal("a different (input, identity) pair was served the first pair's entry — the key still runs the two fields together")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("model calls = %d, want 2: the second request must run the model", got)
	}
	if string(first.Data) == string(second.Data) {
		t.Fatalf("both requests came back with %q: one request's answer was served to the other", first.Data)
	}
	if n := countEntries(t, ca); n != 2 {
		t.Fatalf("cache holds %d entries, want 2 (one per request)", n)
	}

	// Not vacuous: the cache still works for a genuine repeat of either request.
	if !run(reqA, extraA).Meta.CacheHit || !run(reqB, extraB).Meta.CacheHit {
		t.Fatal("a genuine repeat missed the cache: the fix disabled caching instead of separating the fields")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("model calls = %d after the repeats, want 2", got)
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

// THE TRANSCRIBE KEY, at its call site. TestTranscribeCacheKeyKeepsFieldsApart only
// calls transcribeCacheKey, so it cannot see which key runTranscribe really stores
// under: with the glued `cache.Key("transcribe", ident, ...)` put back at the call site
// every other test in this file stayed green. This drives runTranscribe and ties the
// entry it stores to transcribeCacheKey, the way the vision test ties its entry to
// visionKeyFor. The language is given (and the route, which decides the protocol and
// whether the language is part of the identity, is read from the same function the
// pipeline uses), so the expected key is fully determined. Both protocol legs run: the
// OAI leg (an HQ model on the OpenAI shape) keys with no language and proto "oai", so a
// call site that passed the requested language there would go red on that leg only.
func TestRunTranscribeStoresUnderTranscribeCacheKey(t *testing.T) {
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH")
	}
	for _, leg := range []struct {
		name   string
		hq     bool
		params map[string]any
	}{
		{"whisper", false, map[string]any{"language": "en"}},
		{"oai", true, map[string]any{"language": "en", "hq": true}},
	} {
		t.Run(leg.name, func(t *testing.T) {
			wav := makeSilentWav(t, ffmpeg)
			cfg := gateCfg(t)
			cfg.MediaDir = t.TempDir()
			cfg.FFmpegPath = ffmpeg
			cfg.Endpoint = fakeWhisper(t)
			cfg.STTModelHQ = "stt-hq"
			cfg.STTHQAPI = "openai"
			ca := gateCache(t)
			p := gatePipeline(t, cfg, ca)

			res := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: wav, Params: leg.params})
			if !res.OK {
				t.Fatalf("transcribe deferred: %s", res.Reason)
			}

			id, err := p.digestMedia(wav)
			if err != nil {
				t.Fatalf("digestMedia: %v", err)
			}
			model, useOAI := sttRoute(cfg, leg.hq)
			if useOAI != leg.hq {
				t.Fatalf("sttRoute(hq=%v) chose useOAI=%v: the leg does not run the protocol it names", leg.hq, useOAI)
			}
			lang, proto := "en", "whisper"
			if useOAI {
				lang, proto = "", "oai"
			}
			want := transcribeCacheKey(id.Digest, model, lang, proto, cacheParams(leg.params))
			if !cacheHas(t, ca, want) {
				t.Fatal("runTranscribe did not store under transcribeCacheKey(digest, model, lang, proto, params): its call site builds the key some other way")
			}
			if n := countEntries(t, ca); n != 1 {
				t.Fatalf("the cache holds %d entries, want exactly the one under transcribeCacheKey", n)
			}

			// Not vacuous: that entry is the one a repeat reads.
			if rep := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: wav, Params: leg.params}); !rep.Meta.CacheHit {
				t.Fatal("a repeat did not hit the entry stored under transcribeCacheKey")
			}
		})
	}
}

// A cached transcript names files on disk. A node that removes a finished upload job's transcript
// files (fleet_stt_transcript_ttl_min) leaves the cache entry pointing at nothing, and a second upload
// of the same recording would be answered with paths that do not exist: a hit whose files are gone is
// a miss, and the transcription is redone (and its files written again).
func TestTranscribeCacheHitWithMissingFilesIsAMiss(t *testing.T) {
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH")
	}
	wav := makeSilentWav(t, ffmpeg)
	cfg := gateCfg(t)
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = ffmpeg
	cfg.Endpoint = fakeWhisper(t)
	p := gatePipeline(t, cfg, gateCache(t))
	run := func() (core.Result, map[string]any) {
		res := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: wav})
		if !res.OK {
			t.Fatalf("transcribe deferred: %s", res.Reason)
		}
		var data map[string]any
		if err := json.Unmarshal(res.Data, &data); err != nil {
			t.Fatal(err)
		}
		return res, data
	}

	first, data := run()
	if first.Meta.CacheHit {
		t.Fatal("a cold call reported a cache hit")
	}
	paths := []string{data["srt_path"].(string), data["text_path"].(string), data["json_path"].(string)}
	for _, f := range paths {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("the first call left no %s: %v", f, err)
		}
	}

	hit, _ := run()
	if !hit.Meta.CacheHit {
		t.Fatal("the same recording with its files in place must be a cache hit")
	}

	for _, f := range paths {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	redo, data2 := run()
	if redo.Meta.CacheHit {
		t.Fatal("a cache hit whose transcript files are gone was served: its paths point at nothing")
	}
	for _, k := range []string{"srt_path", "text_path", "json_path"} {
		if _, err := os.Stat(data2[k].(string)); err != nil {
			t.Errorf("the redone call's %s is missing: %v", k, err)
		}
	}
}

// The fleet node's media route gates and sweeps an stt upload's transcripts by NAME
// (internal/fleetnode/media_gate.go: sttOutputRe, `^stt-[0-9]+-[0-9a-f]{8}\.(srt|txt|segments\.json)$`).
// The producer is this pipeline: the door's private file is os.CreateTemp(dir, "stt-*.<ext>") (stt-
// plus a decimal random number) and the outputs are mediaBase(...) plus the extension. If either
// side drifts the node's bearer gate fails OPEN and its sweep deletes nothing, with every test on
// the consumer's side still green, so the REAL producer is pinned here against the same literal.
var sttUploadOutputRe = regexp.MustCompile(`^stt-[0-9]+-[0-9a-f]{8}\.(srt|txt|segments\.json)$`)

func TestMediaBaseOfAnSTTUploadMatchesTheNodesGatedShape(t *testing.T) {
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, "stt-*.ogg") // exactly the door's call (fleetnode.buildSTTUpload)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	base := filepath.Base(mediaBase(t.TempDir(), f.Name(), "media:sha256:sz=1:"+strings.Repeat("a", 64)+"|model=m|lang=|proto=whisper"))
	for _, ext := range []string{".srt", ".txt", ".segments.json"} {
		if !sttUploadOutputRe.MatchString(base + ext) {
			t.Errorf("the transcript name %q does not match the node's gated shape %s", base+ext, sttUploadOutputRe)
		}
	}
}

// The same through the real transcribe path: the files an upload-named audio file's call writes.
func TestTranscribeOutputsOfAnSTTUploadMatchTheNodesGatedShape(t *testing.T) {
	ffmpeg := lookFFmpeg()
	if ffmpeg == "" {
		t.Skip("ffmpeg not on PATH")
	}
	wav := makeSilentWav(t, ffmpeg)
	raw, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "stt-*.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(raw); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cfg := gateCfg(t)
	cfg.MediaDir = t.TempDir()
	cfg.FFmpegPath = ffmpeg
	cfg.Endpoint = fakeWhisper(t)
	p := gatePipeline(t, cfg, gateCache(t))
	res := p.Run(context.Background(), core.Request{Task: core.TaskTranscribe, Audio: f.Name()})
	if !res.OK {
		t.Fatalf("transcribe deferred: %s", res.Reason)
	}
	var data map[string]any
	if err := json.Unmarshal(res.Data, &data); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"srt_path", "text_path", "json_path"} {
		name := filepath.Base(data[k].(string))
		if !sttUploadOutputRe.MatchString(name) {
			t.Errorf("%s = %q does not match the node's gated shape %s", k, name, sttUploadOutputRe)
		}
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
	return cache.Key(string(req.Task), req.Input, extra, cacheParams(req.Params), meta.Model, built.Grammar,
		templateCacheTag(built.System, built.Grammar, built.User, req.Input))
}

func makeSilentWav(t *testing.T, ffmpeg string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "silence.wav")
	cmd := winexec.Command(ffmpeg, "-nostdin", "-f", "lavfi", "-i", "anullsrc=r=16000:cl=mono",
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
