// Package pipeline orchestrates one offload request end to end:
// trivial-check -> context-budget trim -> cache -> build -> generate(grammar)
// -> parse -> verify -> validate -> (retry|defer|accept) -> cache + ledger.
package pipeline

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/offload-harness/internal/audioio"
	"github.com/dmmdea/offload-harness/internal/breaker"
	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/confhead"
	"github.com/dmmdea/offload-harness/internal/confidence"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/contextbudget"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/exemplars"
	"github.com/dmmdea/offload-harness/internal/gbnf"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/gpulock"
	"github.com/dmmdea/offload-harness/internal/grounding"
	"github.com/dmmdea/offload-harness/internal/imagegen"
	"github.com/dmmdea/offload-harness/internal/imageio"
	"github.com/dmmdea/offload-harness/internal/judge"
	"github.com/dmmdea/offload-harness/internal/knn"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/parser"
	"github.com/dmmdea/offload-harness/internal/router"
	"github.com/dmmdea/offload-harness/internal/shadow"
	"github.com/dmmdea/offload-harness/internal/sttclient"
	"github.com/dmmdea/offload-harness/internal/svgkit"
	"github.com/dmmdea/offload-harness/internal/tasks"
	"github.com/dmmdea/offload-harness/internal/validator"
	"github.com/dmmdea/offload-harness/internal/verifier"
	"github.com/dmmdea/offload-harness/internal/videoio"
)

type tierOverrides struct {
	TierTimeoutsMs map[string]int `json:"tier_timeouts_ms"`
	Degraded       []string       `json:"degraded"`
}

type Pipeline struct {
	cfg        config.Config
	client     *llamaclient.Client
	stt        *sttclient.Client  // whisper-server transcribe client (audio never hits the text cascade)
	cache      *cache.Cache       // may be nil
	led        *ledger.Ledger     // may be nil
	thresholds map[string]float64 // per-task conformal margin thresholds (Phase 2); nil = config constant
	breakers   *breaker.Group     // per-tier circuit breakers (Phase 3)
	router     *router.Model      // entry-tier router (Phase 5); nil = static rule
	overrides  *tierOverrides     // health-driven per-tier timeouts/degraded (Phase 4); nil = none
	healMu     sync.Mutex         // Phase 7 autoheal rate-limit
	lastHeal   map[string]time.Time
	// Phase 2 Task 4: opt-in correctness head + per-task p(correct) thresholds.
	// Both nil/empty unless cfg.ConfHeadEnabled — the gate is inert otherwise.
	confhead       *confhead.Model    // nil = no head (gate off)
	confThresholds map[string]float64 // per-task p(correct) escalation thresholds
	// meta-router v2: zero-training kNN entry-tier pre-filter (bridge before the
	// LR router trains). Both nil unless cfg.KNNPreFilterEnabled.
	knn   *knn.Index                      // nil = disabled / no substrate
	embed func(string) ([]float64, error) // nil = disabled; set to judge.Embedder.Embed
	// A2 hot-reload: learnMu guards every self-learning field that the background
	// reloader can swap (thresholds, router, overrides, confhead, confThresholds).
	// The request path reads them ONLY through the *Snap accessors (uncontended
	// RLock, zero IO/parse). learnHashes records the content hash of each watched
	// file so a tick only re-loads on a real content change. knn/embed are NOT
	// poll-reloaded (append-file; see reload.go), so they need no hash entry.
	learnMu     sync.RWMutex
	learnHashes map[string]string
	// LO-9 cold-swap tracking: per-tier last-attempt timestamps so a timeout on
	// the FIRST call to an idle tier (llama-swap loading the model) is not
	// counted against that tier's circuit breaker. nowFn is an injectable clock
	// for tests (nil = time.Now).
	swapMu   sync.Mutex
	tierSeen map[string]time.Time
	nowFn    func() time.Time
	// LO-1 GPU-lock gate: vision calls check the render runners' single-slot GPU
	// lock (internal/gpulock) BEFORE hitting llama-swap — while a generation job
	// owns the GPU the VLM cannot (re)load, so calling anyway just burns an
	// http_5xx defer to the expensive cloud model. gpuLockPath is resolved once
	// in New (config override > GPU_LOCK env > tmpdir default, same as the .mjs
	// runners); visionGPUWait/visionGPUPoll bound the pre-call wait; and
	// visionRetryWait is the one-retry backoff on a vision http_5xx. The three
	// durations are fields (not consts) so tests can shrink them.
	gpuLockPath     string
	visionGPUWait   time.Duration
	visionGPUPoll   time.Duration
	visionRetryWait time.Duration
}

// Cfg exposes the loaded config so callers like the MCP server can build
// side-channel tools (e.g. the explicit NIM remote tool) from the same
// configuration without re-loading it. It returns a shallow copy — read-only
// use only (the slice/map fields share backing with the live config).
func (p *Pipeline) Cfg() config.Config { return p.cfg }

func New(cfg config.Config, c *llamaclient.Client, ca *cache.Cache, l *ledger.Ledger) *Pipeline {
	p := &Pipeline{cfg: cfg, client: c, cache: ca, led: l, lastHeal: map[string]time.Time{}, learnHashes: map[string]string{}}
	p.stt = sttclient.New(cfg.Endpoint, time.Duration(cfg.STTRequestTimeoutSec)*time.Second)
	// LO-1: resolve the shared GPU lock path ONCE, the same way the Node render
	// runners do, so the vision gate watches the exact lock the gen jobs hold.
	p.gpuLockPath = gpulock.Path(cfg.GPULockPath)
	p.visionGPUWait = time.Duration(cfg.VisionGPUWaitSec) * time.Second
	p.visionGPUPoll = 2 * time.Second
	p.visionRetryWait = 3 * time.Second
	p.thresholds = loadThresholds(cfg.ThresholdsPath)    // Phase 2
	p.router = router.Load(cfg.RouterWeightsPath)        // Phase 5
	p.overrides = loadOverrides(cfg.TierOverridesPath)   // Phase 4
	p.breakers = breaker.NewGroup(5, 10, 20*time.Second) // Phase 3: 5 infra-fails / 10-window, 20s cooldown
	// Phase 2 Task 4: opt-in correctness gate. Loading is graceful — a missing
	// weights/thresholds file leaves the head nil / map empty, so the gate is
	// inert. Off entirely unless cfg.ConfHeadEnabled.
	if cfg.ConfHeadEnabled {
		p.confhead = confhead.Load(cfg.ConfHeadPath)
		p.confThresholds = confhead.LoadThresholds(cfg.ConfHeadThresholdsPath)
	}
	// meta-router v2: kNN entry-tier pre-filter. Off unless enabled; a missing
	// substrate leaves p.knn nil (fail-open). The embedder uses a short timeout
	// so a slow/down embeddinggemma fails open fast on the request path.
	if cfg.KNNPreFilterEnabled {
		p.knn = knn.Load(cfg.KNNIndexPath)
		p.embed = judge.NewEmbedder(cfg.Endpoint, time.Duration(cfg.KNNEmbedTimeoutMs)*time.Millisecond).Embed
	}
	// Seed the reloader's content hashes from the files just loaded so the first
	// poll tick is a no-op for unchanged artifacts (and a transient bad initial
	// read self-heals: a file that failed to load now hashes to whatever is on
	// disk, so the NEXT good write differs and reloads). knn is intentionally
	// absent — it is never poll-reloaded.
	for _, path := range p.watchedPaths() {
		p.learnHashes[path] = fileContentHash(path)
	}
	return p
}

type cacheVal struct {
	Data     json.RawMessage `json:"data"`
	TokensIn int             `json:"tokens_in"`
}

// Run executes req through the Gemma-4 family cascade and always returns a
// Result (success or structured defer). Fast tasks (triage/classify) enter at
// the small tier; on a quality failure the request climbs to the next-larger
// local model before ever deferring to Opus. Infra errors do not escalate.
func (p *Pipeline) Run(ctx context.Context, req core.Request) core.Result {
	start := time.Now()
	meta := core.Meta{Model: p.cfg.Model}

	if !req.Task.Valid() {
		return core.Deferf("unknown task "+string(req.Task), "", meta)
	}

	// extract_image is a COMPOSITE that builds its own sub-requests (an ocr task +
	// an extract task), so it dispatches BEFORE tasks.Build — there is no single
	// prompt/grammar to build here. It reuses the proven extract pipeline verbatim.
	if req.Task == core.TaskExtractImage {
		return p.runExtractImage(ctx, req, meta, start)
	}

	if req.Task == core.TaskVideoDescribe {
		built, err := tasks.Build(req)
		if err != nil {
			return core.Deferf("build error: "+err.Error(), "", meta)
		}
		return p.runVideoDescribe(ctx, req, built, meta, start)
	}

	// transcribe converts req.Audio to 16kHz WAV then calls whisper-server. Its
	// own branch (audio in, no prompt/grammar, never the text cascade).
	if req.Task == core.TaskTranscribe {
		return p.runTranscribe(ctx, req, meta, start)
	}

	// generate_image renders req.Input (the prompt) to a PNG on the local ComfyUI by
	// shelling out to comfy-generate.mjs (which holds the GPU lock + ComfyUI lifecycle).
	// Its own branch — no text cascade, no grammar, no vision call.
	if req.Task == core.TaskGenerateImage {
		return p.runGenerateImage(ctx, req, meta, start)
	}

	// generate_svg renders a brand-agnostic parametric SVG component (kind + spec in
	// params) via internal/svgkit. Its own branch — pure Go, no text cascade, no
	// grammar, no GPU lock.
	if req.Task == core.TaskGenerateSVG {
		return p.runGenerateSVG(ctx, req, meta, start)
	}

	// generate_video animates req.Image into a short clip on the local ComfyUI by
	// shelling out (via internal/gpugen) to comfy-video.mjs (shared GPU lock + ComfyUI
	// lifecycle + process-tree-kill). Its own branch — no text cascade, no grammar.
	if req.Task == core.TaskGenerateVideo {
		return p.runGenerateVideo(ctx, req, meta, start)
	}

	// generate_audio synthesizes audio on the local GPU: kind=voice (Chatterbox via
	// tts.mjs, no ComfyUI) or kind=music (ACE-Step via ComfyUI). Its own branch,
	// dispatching by kind to VoiceGenScript/MusicGenScript through internal/gpugen.
	if req.Task == core.TaskGenerateAudio {
		return p.runGenerateAudio(ctx, req, meta, start)
	}

	// Vision tasks (vqa) take a SEPARATE branch: the input is an image, not text,
	// so they skip the trivial-input gate, the context-budget trim, and the whole
	// text model cascade. The text path below stays byte-identical for non-vision
	// tasks. Build the prompt here so a bad request still defers cleanly.
	if isVisionTask(req.Task) {
		built, err := tasks.Build(req)
		if err != nil {
			return core.Deferf("build error: "+err.Error(), "", meta)
		}
		return p.runVision(ctx, req, built, meta, start)
	}

	if contextbudget.IsTrivial(req.Input) {
		return core.Deferf("input too small to offload", "", meta)
	}
	req.Input, _ = contextbudget.Trim(req.Input, p.cfg.MaxInputChars)
	meta.Feat = featurize(req.Task, req.Input) // cheap input features for the router

	built, err := tasks.Build(req)
	if err != nil {
		return core.Deferf("build error: "+err.Error(), "", meta)
	}
	// Phase 6: prepend retrieved few-shot exemplars to the local-model prompt
	// (off by default — ExemplarShots=0). Grammar/schema/cache key are unchanged.
	if p.cfg.ExemplarShots > 0 && p.cfg.ExemplarsDir != "" {
		if ex := exemplars.Retrieve(p.cfg.ExemplarsDir, string(req.Task), req.Input, p.cfg.ExemplarShots); len(ex) > 0 {
			built.User = injectExemplars(built.User, ex)
		}
	}

	// Cache key is stable on the PRIMARY model so a result produced by any tier
	// is reused on re-runs (the cascade is an internal detail of one logical call).
	ck := cache.Key(string(req.Task), req.Input, tasks.StableParamsKey(req.Params), p.cfg.Model, built.Grammar)
	if p.cache != nil {
		if raw, ok := p.cache.Get(ck); ok {
			var cv cacheVal
			if json.Unmarshal(raw, &cv) == nil && len(cv.Data) > 0 {
				meta.CacheHit = true
				meta.TokensIn = cv.TokensIn
				meta.LatencyMs = time.Since(start).Milliseconds()
				p.record(req.Task, meta, len(req.Input))
				return core.Result{OK: true, Data: cv.Data, Meta: meta}
			}
		}
	}

	// kNN entry-tier pre-filter is off unless configured (p.knn set only under
	// cfg.KNNPreFilterEnabled); skip the call entirely when off so the request path
	// is literally — not just behaviorally — unchanged.
	knnSkip := false
	if kn, _ := p.knnSnap(); kn != nil {
		knnSkip = p.knnPreferLargerEntry(req.Task, req.Input)
	}
	chain := p.modelChain(req.Task, meta.Feat, knnSkip)
	var last core.Result
	// Task 1.5: entry-tier (ci==0) snapshot + candidate, so a later agreeing tier
	// can record a cascade-agreement correctness-proxy label for classify/triage.
	var entrySnapshot *ledger.Entry // value copy — safe vs meta mutation across iterations
	var entryCandidate string       // entry-tier candidate JSON (its Partial)
	for ci, model := range chain {
		meta.Model = model
		meta.Escalations = ci
		likelyColdSwap := p.noteTierCall(model) // LO-9: before the attempt, so the window is per-call
		res, escalatable := p.attempt(ctx, req, built, ck, model, meta, start, true)
		// Phase 3/7: the breaker tracks INFRA health only (ErrClass set); a quality
		// defer means the tier physically worked. Autoheal fires on infra failure.
		// LO-9: a TIMEOUT on the first call to an idle tier is exempted from
		// breaker accounting (likely a llama-swap cold swap, not a sick tier —
		// see breakerFailure).
		if p.breakers != nil {
			infra := res.Meta.ErrClass != ""
			p.breakers.Record(model, !breakerFailure(res.Meta.ErrClass, likelyColdSwap))
			if infra && p.cfg.AutoHeal {
				p.maybeHeal(model)
			}
		}
		if res.OK {
			if ci > 0 && entrySnapshot != nil {
				p.labelAgreement(req.Task, *entrySnapshot, entryCandidate, res, len(req.Input))
			}
			return res
		}
		last = res
		if ci == 0 {
			// Snapshot from res.Meta, NOT the outer meta: the entry tier's real
			// Margin/Retries/Truncated are set on attempt's by-value copy and
			// returned only in res.Meta (the outer meta is still pre-call zeros for
			// those). res.Meta is a copy, so it's safe against later loop mutation.
			snap := entryFrom(req.Task, res.Meta, true, len(req.Input))
			entrySnapshot = &snap
			entryCandidate = res.Partial // candidate JSON string (gen.Content carried into Deferf)
		}
		if !escalatable || ci == len(chain)-1 {
			break
		}
	}
	// Terminal LOCAL reasoning tier (grammar tasks only): after the whole cascade defers, give
	// a thinking model one shot under a think-wrapped grammar to reclaim the deferral before
	// falling through to Opus. A failure here defers exactly as before (never calls cloud).
	if p.cfg.ReasoningModel != "" && built.Grammar != "" && !last.Meta.Truncated {
		rres, ok := p.attemptReasoning(ctx, req, built, ck, meta, start)
		if ok {
			return rres
		}
		last = rres
	}
	p.recordDefer(req.Task, last.Meta, len(req.Input), last.Reason)
	return last
}

// isVisionTask reports whether a task runs on the vision branch (single VLM tier,
// image input, no text cascade). Extensible as extract-image/assess land.
func isVisionTask(t core.TaskType) bool {
	return t == core.TaskVQA || t == core.TaskOCR || t == core.TaskAssessImage
}

// visionResultKey returns the JSON key under which a vision task's success output
// is wrapped: vqa answers a question ("answer"); ocr transcribes text ("text").
// vqa stays byte-identical to its original behavior.
func visionResultKey(t core.TaskType) string {
	if t == core.TaskOCR {
		return "text"
	}
	return "answer"
}

// runVision handles a single multimodal call on the VLM tier. It mirrors the
// text path's cache + ledger + defer machinery but uses GenerateVision and has
// NO grammar/grounding/confidence-margin gate — vqa is free-text, so it rides
// only empty-output, truncation, and infra defers. A bigger local tier is not
// available, so any defer goes straight to Opus (itself a strong VLM).
func (p *Pipeline) runVision(ctx context.Context, req core.Request, built tasks.Built, meta core.Meta, start time.Time) core.Result {
	// An empty VisionModel means "no vision route configured" (documented in
	// config.VisionModel). Guard FIRST: GenerateVision(ctx, "", ...) would fall back
	// to the TEXT model alias, misrouting an image request onto a text tier. Defer
	// to Opus (itself a strong VLM) instead — never call the model.
	if p.cfg.VisionModel == "" {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), "no vision model configured")
		return core.Deferf("no vision model configured", "", meta)
	}
	meta.Model = p.cfg.VisionModel
	dataURI, err := imageio.LoadImageB64(req.Image, p.cfg.VisionMaxImageBytes)
	if err != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), "image load: "+err.Error())
		return core.Deferf("image load: "+err.Error(), "", meta)
	}
	return p.runVisionGen(ctx, req, built, meta, start, "img:"+sha256hex(dataURI), func(gctx context.Context) (llamaclient.GenResult, error) {
		return p.client.GenerateVision(gctx, p.cfg.VisionModel, built.System, built.User, []string{dataURI}, built.Grammar, built.MaxTokens, p.cfg.Temperature, 0)
	})
}

// runVisionGen owns the cache + ledger + defer/wrap machinery shared by the
// single-image vision tasks and video_describe. `gen` is a closure that performs
// the actual multimodal call (1 image for vqa/ocr/assess; interleaved frames for
// video). cacheKeyExtra distinguishes inputs in the cache. No grammar/grounding/
// confidence gate for the free-text tasks; a grammar-constrained vision task
// (assess_image) surfaces its JSON verbatim. Any defer goes straight to Opus.
// LO-1: after the cache check it gates on the render runners' GPU lock (bounded
// wait, distinct "gpu busy" defer), retries once on http_5xx, and records the
// final infra outcome into the vision tier's circuit breaker.
func (p *Pipeline) runVisionGen(ctx context.Context, req core.Request, built tasks.Built, meta core.Meta, start time.Time, cacheKeyExtra string, gen func(context.Context) (llamaclient.GenResult, error)) core.Result {
	ck := cache.Key(string(req.Task), req.Input+"|"+cacheKeyExtra, tasks.StableParamsKey(req.Params), p.cfg.VisionModel, built.Grammar)
	if p.cache != nil {
		if raw, ok := p.cache.Get(ck); ok {
			var cv cacheVal
			if json.Unmarshal(raw, &cv) == nil && len(cv.Data) > 0 {
				meta.CacheHit = true
				meta.TokensIn = cv.TokensIn
				meta.LatencyMs = time.Since(start).Milliseconds()
				p.record(req.Task, meta, len(req.Input))
				return core.Result{OK: true, Data: cv.Data, Meta: meta}
			}
		}
	}
	// LO-1: the VLM shares the 8GB GPU with the generation runners. If a gen job
	// holds the single-slot lock, llama-swap CANNOT (re)load the vision model —
	// during the Jul-1 incident every vision call 5xx'd and deferred to the
	// expensive cloud model (295 of 337 all-time defers in ONE hour). Wait for
	// the slot (bounded, cheap dir-stat poll) instead of burning a doomed call;
	// if it never frees, defer with a distinct, actionable reason.
	if info := gpulock.WaitFree(ctx, p.gpuLockPath, p.visionGPUWait, p.visionGPUPoll); info.Held {
		meta.LatencyMs = time.Since(start).Milliseconds()
		meta.ErrClass = "gpu_busy"
		reason := fmt.Sprintf("gpu busy: generation job holds the lock (%ds)", int(info.Age/time.Second))
		p.recordDefer(req.Task, meta, len(req.Input), reason)
		return core.Deferf(reason, "", meta)
	}

	// LO-9 parity for the vision tier: stamp the call so a cold-swap timeout is
	// exempt from breaker accounting (http_5xx / warm timeouts still count).
	likelyColdSwap := p.noteTierCall(p.cfg.VisionModel)
	gres, gerr := gen(ctx)
	if gerr != nil && classifyErr(gerr) == "http_5xx" {
		// LO-1: retry ONCE after a short backoff — a vision 5xx is usually
		// llama-swap failing a (re)load under transient GPU pressure (e.g. a gen
		// job grabbed the lock between our gate check and the call), and the
		// second attempt lands after the pressure passes.
		select {
		case <-ctx.Done():
		case <-time.After(p.visionRetryWait):
			gres, gerr = gen(ctx)
		}
	}
	// LO-1: the vision tier now records into the per-tier breaker group exactly
	// like the text tiers — infra failures only (quality defers below mean the
	// tier physically worked), and only the FINAL outcome after the retry.
	if p.breakers != nil {
		ec := ""
		if gerr != nil {
			ec = classifyErr(gerr)
		}
		p.breakers.Record(p.cfg.VisionModel, !breakerFailure(ec, likelyColdSwap))
	}
	if gerr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		meta.ErrClass = classifyErr(gerr)
		p.recordDefer(req.Task, meta, len(req.Input), "vision model call failed: "+gerr.Error())
		return core.Deferf("vision model call failed: "+gerr.Error(), "", meta)
	}
	meta.TokensIn = gres.TokensIn
	meta.TokensOut = gres.TokensOut
	meta.TokPerSec = gres.TokPerSec
	meta.Truncated = gres.Truncated
	meta.LatencyMs = time.Since(start).Milliseconds()

	answer := strings.TrimSpace(gres.Content)
	if answer == "" {
		p.recordDefer(req.Task, meta, len(req.Input), "empty vision output")
		return core.Deferf("empty vision output", gres.Content, meta)
	}
	if gres.Truncated {
		p.recordDefer(req.Task, meta, len(req.Input), "vision output truncated")
		return core.Deferf("vision output truncated", gres.Content, meta)
	}
	var data json.RawMessage
	if built.Grammar != "" {
		if !json.Valid([]byte(answer)) {
			p.recordDefer(req.Task, meta, len(req.Input), "non-JSON output from grammar vision task")
			return core.Deferf("non-JSON output from grammar vision task", gres.Content, meta)
		}
		data = json.RawMessage(answer)
	} else {
		data, _ = json.Marshal(map[string]string{visionResultKey(req.Task): answer})
	}
	if p.cache != nil {
		if b, e := json.Marshal(cacheVal{Data: data, TokensIn: gres.TokensIn}); e == nil {
			_ = p.cache.Put(ck, b)
		}
	}
	p.record(req.Task, meta, len(req.Input))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// runVideoDescribe samples frames from req.Video, builds <T.T seconds> timestamp
// labels (from VideoFPS), and runs them interleaved through the vision tier. A
// sampling failure (ffmpeg missing/bad video) is an input/infra error: defer.
func (p *Pipeline) runVideoDescribe(ctx context.Context, req core.Request, built tasks.Built, meta core.Meta, start time.Time) core.Result {
	if p.cfg.VisionModel == "" {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), "no vision model configured")
		return core.Deferf("no vision model configured", "", meta)
	}
	meta.Model = p.cfg.VisionModel
	fps := p.cfg.VideoFPS
	if fps <= 0 {
		fps = 1
	}
	// Sample frames and describe them. If the VLM rejects the request for
	// exceeding its context window (a high-res / tall clip — e.g. a 4K vertical
	// reel — can blow the ctx with the default frame budget), HALVE the frame
	// RESOLUTION and retry: this keeps full temporal coverage (same frame count)
	// rather than dropping frames, so the answer still spans the whole clip.
	// Floor at 256px so we don't spiral into uselessly tiny frames.
	width := p.cfg.VideoFrameWidth
	if width <= 0 {
		width = 512
	}
	for {
		frames, err := videoio.SampleFrames(req.Video, p.cfg.FFmpegPath, p.cfg.VideoFPS, p.cfg.VideoMaxFrames, width, p.cfg.VisionMaxImageBytes)
		if err != nil {
			meta.LatencyMs = time.Since(start).Milliseconds()
			p.recordDefer(req.Task, meta, len(req.Input), "frame sampling: "+err.Error())
			return core.Deferf("frame sampling: "+err.Error(), "", meta)
		}
		labels := make([]string, len(frames))
		for i := range frames {
			labels[i] = fmt.Sprintf("<%.1f seconds>", float64(i)/fps)
		}
		extra := fmt.Sprintf("vid:%s|fps=%g|n=%d|w=%d|frames=%d", req.Video, p.cfg.VideoFPS, p.cfg.VideoMaxFrames, width, len(frames))
		res := p.runVisionGen(ctx, req, built, meta, start, extra, func(gctx context.Context) (llamaclient.GenResult, error) {
			return p.client.GenerateVisionInterleaved(gctx, p.cfg.VisionModel, built.System, labels, frames, built.User, built.Grammar, built.MaxTokens, p.cfg.Temperature, 0)
		})
		if res.OK || width <= 256 || !isContextOverflow(res.Reason) {
			return res
		}
		width /= 2 // halve resolution, keep the frame count, retry to fit the ctx
	}
}

// isContextOverflow reports whether a vision defer was caused by the request
// exceeding the model's context window (too many / too-large frames for the
// VLM's ctx). runVideoDescribe retries such cases at a lower frame resolution.
func isContextOverflow(reason string) bool {
	r := strings.ToLower(reason)
	return strings.Contains(r, "exceeds the available context") ||
		strings.Contains(r, "exceed_context_size") ||
		strings.Contains(r, "context size")
}

// runTranscribe converts req.Audio to a 16kHz mono WAV (ffmpeg), transcribes it
// on the whisper upstream, writes .srt/.txt/.segments.json to MediaDir, and
// returns {gist, segments[](capped), language, duration_sec, num_segments,
// *_path}. Any failure (no model / convert / model call / empty) defers to Opus.
// It force-unloads the upstream after the call (zero-always-warm) unless
// disabled. params: language (string), hq (bool -> the large-v3 upstream).
func (p *Pipeline) runTranscribe(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	if p.cfg.STTModel == "" {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Audio), "no stt model configured")
		return core.Deferf("no stt model configured", "", meta)
	}
	model := p.cfg.STTModel
	if paramBool(req.Params, "hq") && p.cfg.STTModelHQ != "" {
		model = p.cfg.STTModelHQ
	}
	meta.Model = model

	lang := p.cfg.STTLanguage
	if l := paramStr(req.Params, "language"); l != "" {
		lang = l
	}
	if strings.EqualFold(lang, "auto") {
		lang = ""
	}

	// Convert first (cheap, deterministic). A bad/missing file defers here.
	wav, cleanup, cerr := audioio.ConvertToWav16k(req.Audio, p.cfg.FFmpegPath)
	if cerr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Audio), "audio convert: "+cerr.Error())
		return core.Deferf("audio convert: "+cerr.Error(), "", meta)
	}
	defer cleanup()

	// Identity = source file (path+size+mtime) + model + lang. Used for BOTH the
	// cache key AND the on-disk media filename so they agree and never collide
	// across distinct sources that share a basename (recording.m4a is common in
	// field audio) or across model/lang variants of the same source.
	ident := req.Audio + "|" + audioCacheExtra(req.Audio, model, lang)
	ck := cache.Key("transcribe", ident, tasks.StableParamsKey(req.Params), model, "")
	if p.cache != nil {
		if raw, ok := p.cache.Get(ck); ok {
			var cv cacheVal
			if json.Unmarshal(raw, &cv) == nil && len(cv.Data) > 0 {
				meta.CacheHit = true
				meta.LatencyMs = time.Since(start).Milliseconds()
				p.record(req.Task, meta, len(req.Audio))
				return core.Result{OK: true, Data: cv.Data, Meta: meta}
			}
		}
	}

	prm := sttclient.DefaultParams()
	prm.Language = lang
	if !p.cfg.STTVAD {
		prm.VAD = false
	}
	tr, terr := p.stt.Transcribe(ctx, model, wav, prm)
	// zero-always-warm: free the upstream's VRAM now (best-effort, short timeout).
	if p.cfg.STTUnloadAfter {
		uctx, ucancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = p.stt.Unload(uctx, model)
		ucancel()
	}
	if terr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		meta.ErrClass = classifyErr(terr)
		p.recordDefer(req.Task, meta, len(req.Audio), "transcribe call failed: "+terr.Error())
		return core.Deferf("transcribe call failed: "+terr.Error(), "", meta)
	}
	full := strings.TrimSpace(tr.Text)
	if full == "" && len(tr.Segments) == 0 {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Audio), "empty transcript")
		return core.Deferf("empty transcript", "", meta)
	}

	// Write the full payload to disk (the pointer pattern) — best-effort: a write
	// failure does not fail the result (the inline data still carries the answer).
	base := mediaBase(p.cfg.MediaDir, req.Audio, ident)
	srtPath, txtPath, jsonPath := base+".srt", base+".txt", base+".segments.json"
	_ = os.MkdirAll(filepath.Dir(base), 0o755)
	_ = os.WriteFile(srtPath, []byte(sttclient.SRT(tr.Segments)), 0o644)
	_ = os.WriteFile(txtPath, []byte(full), 0o644)
	if sj, e := json.MarshalIndent(tr.Segments, "", "  "); e == nil {
		_ = os.WriteFile(jsonPath, sj, 0o644)
	}

	// Inline a capped set of segments; the rest live in jsonPath.
	segs := tr.Segments
	truncated := false
	if p.cfg.STTMaxInlineSegments > 0 && len(segs) > p.cfg.STTMaxInlineSegments {
		segs = segs[:p.cfg.STTMaxInlineSegments]
		truncated = true
	}

	out := transcribeResult{
		Language:          tr.Language,
		DurationSec:       tr.Duration,
		NumSegments:       len(tr.Segments),
		Gist:              preview(full, 400),
		Segments:          segs,
		SegmentsTruncated: truncated,
		SRTPath:           srtPath,
		TextPath:          txtPath,
		JSONPath:          jsonPath,
	}
	data, _ := json.Marshal(out)
	if p.cache != nil {
		if b, e := json.Marshal(cacheVal{Data: data}); e == nil {
			_ = p.cache.Put(ck, b)
		}
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	p.record(req.Task, meta, len(req.Audio))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// transcribeResult is the offload_transcribe payload (the {gist, segments[]}
// citation pattern + on-disk pointers).
type transcribeResult struct {
	Language          string              `json:"language"`
	DurationSec       float64             `json:"duration_sec"`
	NumSegments       int                 `json:"num_segments"`
	Gist              string              `json:"gist"`
	Segments          []sttclient.Segment `json:"segments"`
	SegmentsTruncated bool                `json:"segments_truncated"`
	SRTPath           string              `json:"srt_path"`
	TextPath          string              `json:"text_path"`
	JSONPath          string              `json:"json_path"`
}

// mediaBase returns <MediaDir>/<sanitized-basename>-<8hex of ident> as the
// output stem. The ident hash disambiguates distinct sources that share a
// basename (e.g. two different recording.m4a) or model/lang variants of one
// source, so the returned .srt/.txt/.segments.json pointers never reference a
// different audio's transcript. ident is the SAME identity used for the cache
// key, so on-disk files and cache entries agree.
func mediaBase(mediaDir, audioPath, ident string) string {
	name := filepath.Base(audioPath)
	if ext := filepath.Ext(name); ext != "" {
		name = name[:len(name)-len(ext)]
	}
	name = sanitizeStem(name)
	if name == "" || name == "." {
		name = "transcript"
	}
	return filepath.Join(mediaDir, name+"-"+sha256hex(ident)[:8])
}

// runGenerateImage renders req.Input (the prompt) to a PNG on the LOCAL ComfyUI by shelling
// out to comfy-generate.mjs (which takes the shared GPU lock and starts/stops ComfyUI). Its
// own branch — no text models, no grammar, no vision call. Any failure (no route, empty
// prompt, ComfyUI down, render error, timeout) defers to Claude. params: negative (string),
// width/height/steps/seed (int).
func (p *Pipeline) runGenerateImage(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	if p.cfg.ImageGenScript == "" {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), "no image-gen route configured")
		return core.Deferf("no image-gen route configured", "", meta)
	}
	prompt := strings.TrimSpace(req.Input)
	if prompt == "" {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), "empty image prompt")
		return core.Deferf("empty image prompt", "", meta)
	}
	// LO-2: resolve a relative script path against the exe dir (an MCP host spawns
	// us with no meaningful cwd) and defer with a distinct reason when missing.
	script, serr := gpugen.ResolveScript(p.cfg.ImageGenScript)
	if serr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), serr.Error())
		return core.Deferf(serr.Error(), "", meta)
	}
	meta.Model = "comfyui-sdxl"

	// Pin a concrete seed BEFORE the render so the reported seed matches what ComfyUI actually
	// used: comfy-render picks a RANDOM seed when none is supplied, so without this the result
	// would report seed:0 — wrong, and defeating the documented reproducibility. Honor a
	// caller-supplied positive seed; otherwise mint one and thread it through req.Params.
	seed := paramIntOr(req.Params, "seed", 0)
	if seed <= 0 {
		seed = mintSeed()
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		req.Params["seed"] = seed
	}

	// Output path: caller's "out", else a stable name under MediaDir (identical prompt+params
	// reuse one file; a seed/size change varies the hash).
	out := paramStr(req.Params, "out")
	if out == "" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		out = filepath.Join(p.cfg.MediaDir, "render-"+sha256hex(prompt + tasks.StableParamsKey(req.Params))[:8]+".png")
	}

	timeout := time.Duration(p.cfg.ImageGenTimeoutSec) * time.Second
	outPath, gerr := imagegen.Generate(ctx, p.cfg.NodePath, script, p.cfg.ComfyDir, out, prompt, req.Params, timeout, p.lockEnv()...)
	if gerr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		meta.ErrClass = classifyErr(gerr)
		p.recordDefer(req.Task, meta, len(req.Input), "image generation failed: "+gerr.Error())
		return core.Deferf("image generation failed: "+gerr.Error(), "", meta)
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(map[string]any{
		"image_path": outPath,
		"width":      paramIntOr(req.Params, "width", 1024),
		"height":     paramIntOr(req.Params, "height", 1024),
		"seed":       seed,
	})
	p.record(req.Task, meta, len(prompt))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// runGenerateVideo animates req.Image (a still) into a short clip on the LOCAL ComfyUI by
// shelling out (via internal/gpugen) to comfy-video.mjs, which holds the shared GPU lock,
// runs the ComfyUI lifecycle, and is now process-tree-killed on timeout. Its own branch —
// no text models, no grammar. Any failure (no route, empty prompt, render error, timeout)
// defers to Claude. params: still (string image path), model (hunyuan|wan), frames/width/
// height/steps/seed (int), negative (string), reserve_vram (float, per-workflow override).
func (p *Pipeline) runGenerateVideo(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	if p.cfg.VideoGenScript == "" {
		return p.deferGen(req, meta, start, len(req.Input), "no video-gen route configured")
	}
	prompt := strings.TrimSpace(req.Input)
	if prompt == "" {
		return p.deferGen(req, meta, start, len(req.Input), "empty video prompt")
	}
	// LO-2: resolve a relative script path against the exe dir (an MCP host spawns
	// us with no meaningful cwd) and defer with a distinct reason when missing.
	script, serr := gpugen.ResolveScript(p.cfg.VideoGenScript)
	if serr != nil {
		return p.deferGen(req, meta, start, len(req.Input), serr.Error())
	}
	meta.Model = "comfyui-video"

	seed := paramIntOr(req.Params, "seed", 0)
	if seed <= 0 {
		seed = mintSeed()
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		req.Params["seed"] = seed
	}

	// still: explicit param wins; else req.Image (the I2V input). May be empty for a
	// text-driven graph — the runner validates and errors (→ defer) if it truly needs one.
	still := paramStr(req.Params, "still")
	if still == "" {
		still = req.Image
	}

	out := paramStr(req.Params, "out")
	if out == "" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		out = filepath.Join(p.cfg.MediaDir, "video-"+sha256hex(prompt+tasks.StableParamsKey(req.Params))[:8]+".mp4")
	}

	// comfy-video.mjs CLI: <out> <still> "<prompt>" [--model ..] [--frames N] ...
	args := []string{out}
	if still != "" {
		args = append(args, still)
	}
	args = append(args, prompt)
	if m := paramStr(req.Params, "model"); m != "" {
		args = append(args, "--model", m)
	}
	if n := paramStr(req.Params, "negative"); n != "" {
		args = append(args, "--negative", n)
	}
	for _, k := range []string{"frames", "width", "height", "steps", "seed"} {
		if v := paramIntOr(req.Params, k, 0); v > 0 {
			args = append(args, "--"+k, strconv.Itoa(v))
		}
	}
	// invariant 5: --reserve-vram stays per-workflow-overridable (default lives in the
	// runner; Wan 14B=2.0, ACE-Step differs). Pass it through ONLY when the caller set it.
	if rv := paramStr(req.Params, "reserve_vram"); rv != "" {
		args = append(args, "--reserve-vram", rv)
	}

	timeout := time.Duration(p.cfg.VideoGenTimeoutSec) * time.Second
	outPath, gerr := gpugen.Generate(ctx, gpugen.Spec{
		Exe:     p.cfg.NodePath,
		Script:  script,
		Args:    args,
		Env:     p.genEnv(p.cfg.VideoGenWaitMs),
		Out:     out,
		Timeout: timeout,
	})
	if gerr != nil {
		meta.ErrClass = gpugen.ClassifyErr(gerr)
		return p.deferGen(req, meta, start, len(req.Input), "video generation failed: "+gerr.Error())
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(map[string]any{"video_path": outPath, "seed": seed})
	p.record(req.Task, meta, len(prompt))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// runGenerateAudio synthesizes audio on the LOCAL GPU. It reads params["kind"]
// (voice|music, default voice) and dispatches to VoiceGenScript (Chatterbox TTS, no
// ComfyUI) or MusicGenScript (ACE-Step via ComfyUI). An empty target script — or an
// unknown kind — defers cleanly (music defaults empty until B3). Shells out via
// internal/gpugen so the python/ComfyUI worker is process-tree-killed on timeout
// (invariant 3). params: kind (voice|music), clone/lang (voice), seconds (music),
// out (string), seed (int), reserve_vram (float, music only).
func (p *Pipeline) runGenerateAudio(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	kind := paramStr(req.Params, "kind")
	if kind == "" {
		kind = "voice"
	}
	var script string
	switch kind {
	case "voice":
		script = p.cfg.VoiceGenScript
	case "music":
		script = p.cfg.MusicGenScript
	default:
		return p.deferGen(req, meta, start, len(req.Input), "unknown audio kind "+kind)
	}
	if script == "" {
		return p.deferGen(req, meta, start, len(req.Input), "no audio-gen route configured for kind "+kind)
	}
	text := strings.TrimSpace(req.Input)
	if text == "" {
		return p.deferGen(req, meta, start, len(req.Input), "empty audio prompt")
	}
	// LO-2: resolve a relative script path against the exe dir (an MCP host spawns
	// us with no meaningful cwd) and defer with a distinct reason when missing.
	script, serr := gpugen.ResolveScript(script)
	if serr != nil {
		return p.deferGen(req, meta, start, len(req.Input), serr.Error())
	}
	meta.Model = "chatterbox-tts"
	if kind == "music" {
		meta.Model = "comfyui-acestep"
	}

	seed := paramIntOr(req.Params, "seed", 0)
	if seed <= 0 {
		seed = mintSeed()
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		req.Params["seed"] = seed
	}

	ext := ".wav"
	if kind == "music" {
		ext = ".flac"
	}
	out := paramStr(req.Params, "out")
	if out == "" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		out = filepath.Join(p.cfg.MediaDir, kind+"-"+sha256hex(text+tasks.StableParamsKey(req.Params))[:8]+ext)
	}

	// CLI: tts.mjs <out> "<text>" [--clone ref] [--lang es]
	//      music worker <out> "<style>" --seed N [--seconds N] [--lyrics ..] [--reserve-vram ..]
	args := []string{out, text}
	if kind == "voice" {
		if ref := paramStr(req.Params, "clone"); ref != "" {
			args = append(args, "--clone", ref)
		}
		if lang := paramStr(req.Params, "lang"); lang != "" {
			args = append(args, "--lang", lang)
		}
		// voice path is unchanged: Chatterbox takes no seed, so no --seed flag.
	} else { // music
		// ACE-Step IS seed-reproducible, so pass the minted/echoed seed (fixes the B1
		// gap: the audio path minted a seed but never threaded it to the music worker).
		args = append(args, "--seed", strconv.Itoa(seed))
		if s := paramIntOr(req.Params, "seconds", 0); s > 0 {
			args = append(args, "--seconds", strconv.Itoa(s))
		}
		if l := paramStr(req.Params, "lyrics"); l != "" {
			args = append(args, "--lyrics", l)
		}
		if rv := paramStr(req.Params, "reserve_vram"); rv != "" {
			args = append(args, "--reserve-vram", rv)
		}
	}

	timeout := time.Duration(p.cfg.AudioGenTimeoutSec) * time.Second
	// voice never starts ComfyUI → skip the post-run ComfyUI /free (still tree-kills
	// the python worker on timeout). music drives ComfyUI → keep the /free.
	spec := gpugen.Spec{
		Exe:           p.cfg.NodePath,
		Script:        script,
		Args:          args,
		Env:           p.genEnv(p.cfg.AudioGenWaitMs),
		Out:           out,
		Timeout:       timeout,
		SkipFreeComfy: kind == "voice",
	}
	outPath, gerr := gpugen.Generate(ctx, spec)
	if gerr != nil {
		meta.ErrClass = gpugen.ClassifyErr(gerr)
		return p.deferGen(req, meta, start, len(req.Input), "audio generation failed: "+gerr.Error())
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(map[string]any{"audio_path": outPath, "kind": kind, "seed": seed})
	p.record(req.Task, meta, len(text))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// genEnv builds the extra env for a GPU-gen child: COMFY_DIR + the per-task GPU-lock
// wait window (GPU_LOCK_WAIT_MS, so a queued TTS isn't starved by a long video job)
// + MEMORY_STACK (invariant 1: the CPU-only models freeLlamaSwap must never unload,
// sourced from config not a buried const). waitMs<=0 omits the wait override (runner
// default applies).
func (p *Pipeline) genEnv(waitMs int) []string {
	env := []string{"COMFY_DIR=" + p.cfg.ComfyDir}
	if waitMs > 0 {
		env = append(env, "GPU_LOCK_WAIT_MS="+strconv.Itoa(waitMs))
	}
	if len(p.cfg.MemoryStack) > 0 {
		env = append(env, "MEMORY_STACK="+strings.Join(p.cfg.MemoryStack, ","))
	}
	return append(env, p.lockEnv()...)
}

// lockEnv threads a configured GPU-lock override to a render runner as the
// GPU_LOCK env, so the Go-side vision gate (LO-1) and the Node runners always
// contend on the SAME lock path. Empty when gpu_lock_path is unset — both
// sides then resolve the identical GPU_LOCK-env/tmpdir default on their own.
func (p *Pipeline) lockEnv() []string {
	if p.cfg.GPULockPath != "" {
		return []string{"GPU_LOCK=" + p.cfg.GPULockPath}
	}
	return nil
}

// deferGen records a deferred gen result with latency stamped, keeping the four gen
// runners' defer paths uniform (defer-not-crash, invariant 4).
func (p *Pipeline) deferGen(req core.Request, meta core.Meta, start time.Time, inputChars int, reason string) core.Result {
	meta.LatencyMs = time.Since(start).Milliseconds()
	p.recordDefer(req.Task, meta, inputChars, reason)
	return core.Deferf(reason, "", meta)
}

// runGenerateSVG renders a brand-agnostic parametric SVG component (kind + spec
// in params) via internal/svgkit and writes it to a .svg under cfg.SVGDir. Pure
// Go — no model, no grammar, no GPU lock, no cascade. Any bad kind/spec/write
// defers (Claude makes the asset another way). params: kind (string), spec
// (object/JSON), out (string). Returns {svg_path, width, height}.
func (p *Pipeline) runGenerateSVG(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	meta.Model = "svgkit"
	kind := paramStr(req.Params, "kind")
	if kind == "" {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), "generate_svg: missing kind")
		return core.Deferf("generate_svg: missing kind", "", meta)
	}
	var specRaw json.RawMessage
	if raw, ok := req.Params["spec"]; ok {
		b, mErr := json.Marshal(raw) // spec arrives as a decoded map/any; re-marshal to JSON for svgkit
		if mErr != nil {
			meta.LatencyMs = time.Since(start).Milliseconds()
			p.recordDefer(req.Task, meta, len(req.Input), "generate_svg: bad spec: "+mErr.Error())
			return core.Deferf("generate_svg: bad spec: "+mErr.Error(), "", meta)
		}
		specRaw = b
	} else {
		specRaw = json.RawMessage("{}")
	}
	svg, w, h, rErr := svgkit.Render(kind, specRaw)
	if rErr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), "generate_svg: "+rErr.Error())
		return core.Deferf("generate_svg: "+rErr.Error(), "", meta)
	}
	out := paramStr(req.Params, "out")
	if out == "" {
		_ = os.MkdirAll(p.cfg.SVGDir, 0o755)
		out = filepath.Join(p.cfg.SVGDir, kind+"-"+sha256hex(string(specRaw))[:8]+".svg")
	}
	if wErr := os.WriteFile(out, []byte(svg), 0o644); wErr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), "generate_svg: write: "+wErr.Error())
		return core.Deferf("generate_svg: write: "+wErr.Error(), "", meta)
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(map[string]any{"svg_path": out, "width": w, "height": h})
	p.record(req.Task, meta, len(specRaw))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// mintSeed returns a random positive seed (1..1e9) so an unspecified-seed render is still
// reproducible — the value is threaded into the render and reported back to the caller.
func mintSeed() int {
	n, err := crand.Int(crand.Reader, big.NewInt(1_000_000_000))
	if err != nil {
		return 1
	}
	return int(n.Int64()) + 1
}

// paramIntOr reads an int param (int / int64 / float64), or def if absent.
func paramIntOr(p map[string]any, k string, def int) int {
	switch n := p[k].(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return def
}

// sanitizeStem replaces path separators and Windows-illegal filename characters
// with '_' so a media file always writes cleanly regardless of the source name.
func sanitizeStem(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, s)
}

// audioCacheExtra folds the source file identity (path+size+modtime) + model +
// language into the cache key so a changed file or a different model/lang misses.
func audioCacheExtra(audioPath, model, lang string) string {
	var sz, mt int64
	if fi, err := os.Stat(audioPath); err == nil {
		sz = fi.Size()
		mt = fi.ModTime().UnixNano()
	}
	return fmt.Sprintf("sz=%d|mt=%d|model=%s|lang=%s", sz, mt, model, lang)
}

// preview returns roughly the first n bytes of s trimmed at a word boundary,
// with an ellipsis when truncated — a cheap, deterministic gist (no model call).
// It is rune-safe: n may land mid-rune (e.g. a Spanish á/ñ), so any trailing
// partial UTF-8 rune is trimmed before returning.
func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	for len(cut) > 0 && !utf8.ValidString(cut) { // drop a split multibyte rune
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut) + "…"
}

// paramBool reads a bool param (JSON decodes to bool; tolerate "true").
func paramBool(p map[string]any, k string) bool {
	switch v := p[k].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return false
}

// paramStr reads a string param.
func paramStr(p map[string]any, k string) string {
	if v, ok := p[k].(string); ok {
		return v
	}
	return ""
}

// sha256hex returns the hex-encoded SHA-256 of s (used to fold an image into the
// vision cache key without storing the whole data URI).
func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// runExtractImage is the COMPOSITE extract_image flow: it OCRs the image via the
// existing ocr task, then feeds the OCR text into the EXISTING text extract task.
// This reuses the proven extract path unchanged — GBNF object grammar, verbatim
// grounding (extracted values must appear in the OCR text), schema validation,
// and the escalation/defer ladder all come for free. There is no new extraction
// logic here; runExtractImage only composes ocr + extract.
//
// Telemetry: the two sub-calls each record their own ledger row (an `ocr` vision
// row + an `extract` text row). That is the correct, honest accounting, so
// runExtractImage adds NO recording of its own — meta/start are unused here.
func (p *Pipeline) runExtractImage(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	_ = meta
	_ = start
	// 1. OCR the image via the existing ocr task (reuses runVision + the vision
	//    tier). A propagated defer covers image-load, empty-output, and model-fail.
	ocrRes := p.Run(ctx, core.Request{Task: core.TaskOCR, Image: req.Image})
	if !ocrRes.OK {
		return ocrRes
	}
	// 2. Pull the OCR text out of ocrRes.Data ({"text": "..."}).
	var m map[string]string
	_ = json.Unmarshal(ocrRes.Data, &m)
	ocrText := m["text"]
	if strings.TrimSpace(ocrText) == "" {
		return core.Deferf("empty OCR text for extract_image", "", ocrRes.Meta)
	}
	// 3. Run the EXISTING extract on the OCR text — grammar + grounding (against
	//    ocrText) + schema validation, all reused. The caller's schema rides in
	//    req.Params exactly as offload_extract passes it.
	return p.Run(ctx, core.Request{Task: core.TaskExtract, Input: ocrText, Params: req.Params})
}

// attempt runs the grammar+retry loop for ONE model tier. It returns the result
// and whether a quality failure could plausibly be fixed by a larger tier
// (escalatable). Infra failures return escalatable=false (defer straight out).
// Success is cached + recorded here; a defer is NOT recorded (Run records the
// final one once, so escalation does not double-count).
//
// record gates ALL persistent side-effects on a successful result: the savings
// ledger, the cache write, the shadow-queue capture, and the exemplar harvest.
// Pass true for normal Run calls; pass false for shadow/counterfactual RunTier
// calls that must produce a gradeable result but write NO production side-effects.
func (p *Pipeline) attempt(ctx context.Context, req core.Request, built tasks.Built, ck, model string, meta core.Meta, start time.Time, record bool) (core.Result, bool) {
	attempts := p.cfg.MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	user := built.User
	var lastContent string

	// triage/classify carry a single decision token whose raw logprob margin is a
	// genuine-uncertainty signal; request top logprobs for those tasks only.
	topLP := 0
	if req.Task == core.TaskTriage || req.Task == core.TaskClassify {
		topLP = 10
	}

	// Phase 4: a health-derived per-tier timeout (P95×2), if one was learned.
	actx := ctx
	if ov := p.overridesSnap(); ov != nil {
		if ms, ok := ov.TierTimeoutsMs[model]; ok && ms > 0 {
			var cancel context.CancelFunc
			actx, cancel = context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
			defer cancel()
		}
	}

	for i := 0; i < attempts; i++ {
		meta.Retries = i
		gen, gerr := p.client.Generate(actx, model, built.System, user, built.Grammar, built.MaxTokens, p.cfg.Temperature, topLP)
		if gerr != nil {
			meta.LatencyMs = time.Since(start).Milliseconds()
			meta.ErrClass = classifyErr(gerr)
			return core.Deferf("model call failed: "+gerr.Error(), lastContent, meta), false
		}
		lastContent = gen.Content
		meta.TokensIn = gen.TokensIn
		meta.TokensOut = gen.TokensOut
		meta.TokPerSec = gen.TokPerSec
		meta.Truncated = gen.Truncated

		data, perr := parser.Extract(gen.Content)
		v := verifier.Check(gen.Content, gen.Truncated, perr)
		if v.OK {
			if verr := validator.Validate(data, built.Schema); verr != nil {
				v = verifier.Verdict{Retry: true, Reason: "schema: " + verr.Error()}
			} else if g, ok := grounding.Check(req.Task, req.Input, data); ok {
				// Phase 1 quality eval. Log grounded for the calibration label; act
				// (retry/escalate) ONLY on extract — extraction is verbatim, so a
				// value not in source is a real error. Summarize grounding is noisier
				// (paraphrase), so it's recorded but not actioned.
				meta.Grounded = &g
				if !g && req.Task == core.TaskExtract {
					reason := "ungrounded extract (values not in source)"
					if bad, okf := grounding.CheckFields(req.Task, req.Input, data); okf && len(bad) > 0 {
						reason = "ungrounded extract fields: " + strings.Join(bad, ", ")
					}
					v = verifier.Verdict{Retry: true, Reason: reason}
				}
			}
			if v.OK {
				reason, margin, low := p.confidenceGate(req, data, gen.Logprobs)
				meta.Margin = margin
				// Confhead correctness gate (opt-in, ADOPT tasks only): if the head
				// predicts a low p(correct) for this call, treat it as low-confidence
				// so Run escalates to a larger tier. Only fires when (a) enabled + head
				// loaded, (b) the task has a learned threshold, and (c) a larger tier
				// exists to escalate to (never on the escalation tier itself — the head
				// does not model it). Never touches grammar.
				// P1 no torn read: snapshot the head AND its thresholds together
				// under one RLock, then use ONLY these two locals for the gate. A
				// concurrent reload that swaps both can never yield a crossed
				// (old-head, new-thresholds) pair here.
				chHead, chThr := p.confheadSnap()
				if !low && chHead != nil && len(chThr) > 0 && p.cfg.EscalationModel != "" && model != p.cfg.EscalationModel {
					if tau, ok := chThr[string(req.Task)]; ok {
						e := entryFrom(req.Task, meta, false, len(req.Input))
						pc := chHead.Predict(string(req.Task), confhead.FeatureRow(e))
						if pc >= 0 && pc < tau {
							low = true
							reason = fmt.Sprintf("low confhead p(correct)=%.3f < threshold %.3f", pc, tau)
						}
					}
				}
				if low {
					meta.LatencyMs = time.Since(start).Milliseconds()
					// a larger, more decisive tier may clear the threshold
					return core.Deferf(reason, gen.Content, meta), true
				}
			}
		}

		if v.OK {
			meta.LatencyMs = time.Since(start).Milliseconds()
			// record gates ALL persistent side-effects: ledger, cache, shadow queue,
			// and exemplar harvest. Pass record=false for counterfactual RunTier calls
			// that must produce a gradeable result without any production side-effects.
			if record {
				if p.cache != nil {
					if b, e := json.Marshal(cacheVal{Data: data, TokensIn: gen.TokensIn}); e == nil {
						_ = p.cache.Put(ck, b)
					}
				}
				p.record(req.Task, meta, len(req.Input))
				// Phase A.3: sampled shadow-queue capture (non-escalated classify/triage/extract; config-gated, off by default).
				p.captureShadow(req, entryFrom(req.Task, meta, false, len(req.Input)), core.Result{OK: true, Data: data, Meta: meta})
				// Phase 6: harvest a verified-good (input, output) exemplar for the sidecar.
				if p.cfg.ExemplarsDir != "" && goodExemplar(meta) {
					_ = exemplars.Append(p.cfg.ExemplarsDir, string(req.Task), tasks.StableParamsKey(req.Params), req.Input, data, meta.Margin)
				}
			}
			return core.Result{OK: true, Data: data, Meta: meta}, false
		}

		if v.Retry && i < attempts-1 {
			user = built.User + "\n\nYour previous reply was rejected (" + v.Reason + "). Output ONLY a single valid JSON object with the exact required fields and nothing else."
			continue
		}
		meta.LatencyMs = time.Since(start).Milliseconds()
		// A terminal failure (e.g. truncation — input too large for ANY local
		// tier) defers straight to Opus; escalating would just burn the slow 26B.
		return core.Deferf(v.Reason, gen.Content, meta), !v.Terminal
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	return core.Deferf("exhausted retries", lastContent, meta), true
}

// reasoningThinkBudget is extra generation budget granted to the reasoning tier on top of a
// task's native budget, so the grammar-forced <think> span has room before the JSON answer.
const reasoningThinkBudget = 512

// attemptReasoning is the terminal LOCAL tier for grammar tasks: a thinking model reasons
// under a think-wrapped grammar (gbnf.WrapThinking), the <think> span is stripped, then the
// SAME verify + validate + grounding gates as attempt() run. It is deliberately simpler than
// attempt — no retries and no confidence-escalation gate (there is no larger local tier to
// escalate to; a valid answer here reclaims a cloud deferral, an invalid one falls through to
// the normal defer-to-Opus). Returns (result, ok). On ok the result is recorded + cached; a
// defer is NOT recorded (Run records the final one once).
func (p *Pipeline) attemptReasoning(ctx context.Context, req core.Request, built tasks.Built, ck string, meta core.Meta, start time.Time) (core.Result, bool) {
	meta.Model = p.cfg.ReasoningModel
	meta.Reasoning = true // tag every reasoning-tier outcome so a reclaim is distinguishable from an escalation answer (same model)
	wrapped := gbnf.WrapThinking(built.Grammar)
	// The wrapped grammar emits a <think> span BEFORE the JSON, so the task's native token
	// budget (classify=64, assess=128) would truncate the reasoning before any answer. Give the
	// think span headroom on top of the original budget.
	gen, gerr := p.client.Generate(ctx, p.cfg.ReasoningModel, built.System, built.User, wrapped, built.MaxTokens+reasoningThinkBudget, p.cfg.Temperature, 0)
	if gerr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		meta.ErrClass = classifyErr(gerr)
		return core.Deferf("reasoning model call failed: "+gerr.Error(), "", meta), false
	}
	content := parser.StripThink(gen.Content)
	meta.TokensIn = gen.TokensIn
	meta.TokensOut = gen.TokensOut
	meta.TokPerSec = gen.TokPerSec
	meta.Truncated = gen.Truncated

	data, perr := parser.Extract(content)
	v := verifier.Check(content, gen.Truncated, perr)
	if v.OK {
		if verr := validator.Validate(data, built.Schema); verr != nil {
			v = verifier.Verdict{Reason: "schema: " + verr.Error()}
		} else if g, ok := grounding.Check(req.Task, req.Input, data); ok {
			meta.Grounded = &g
			if !g && req.Task == core.TaskExtract {
				v = verifier.Verdict{Reason: "ungrounded extract (values not in source)"}
			}
		}
	}
	// Classify self-confidence: honor the same accept/defer gate the cascade uses, so a
	// model-flagged-unsure classify answer defers (to Opus) rather than being accepted here.
	if v.OK && req.Task == core.TaskClassify {
		if conf, low := lowConfidence(data, p.cfg.ClassifyMinConfidence); low {
			v = verifier.Verdict{Reason: fmt.Sprintf("low classify confidence %.2f < %.2f", conf, p.cfg.ClassifyMinConfidence)}
		}
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	if !v.OK {
		return core.Deferf("reasoning tier: "+v.Reason, gen.Content, meta), false
	}
	if p.cache != nil {
		if b, e := json.Marshal(cacheVal{Data: data, TokensIn: gen.TokensIn}); e == nil {
			_ = p.cache.Put(ck, b)
		}
	}
	p.record(req.Task, meta, len(req.Input))
	return core.Result{OK: true, Data: data, Meta: meta}, true
}

// modelChain returns the ascending-capability tiers for a task. Fast tasks enter
// at the small tier — UNLESS the learned router predicts it will fail on this
// input (Phase 5) or health marked it degraded (Phase 4), in which case the
// entry is bumped to E4B. Tiers whose circuit breaker is OPEN (Phase 3) are
// skipped (routed around). Duplicates collapse; order preserved.
func (p *Pipeline) modelChain(task core.TaskType, feat map[string]float64, knnSkip bool) []string {
	var tiers []string
	add := func(m string) {
		if m == "" {
			return
		}
		for _, x := range tiers {
			if x == m {
				return
			}
		}
		if p.breakers != nil && p.breakers.State(m) == "open" {
			return // breaker tripped: route around this tier
		}
		tiers = append(tiers, m)
	}
	if task == core.TaskTriage || task == core.TaskClassify {
		if entry := p.cfg.TriageModel; entry != "" && !p.skipSmallEntry(task, entry, feat, knnSkip) {
			add(entry)
		}
	}
	add(p.cfg.Model)
	add(p.cfg.EscalationModel)
	if len(tiers) == 0 { // breakers pruned everything — fall back to the workhorse
		tiers = []string{p.cfg.Model}
	}
	return tiers
}

// skipSmallEntry decides whether to bypass the small (E2B) entry tier: the
// learned router predicts it won't handle this input, or health flagged it.
func (p *Pipeline) skipSmallEntry(task core.TaskType, entry string, feat map[string]float64, knnSkip bool) bool {
	if p.routerSnap().PreferLargerEntry(string(task), feat) { // nil-safe receiver; trained router wins
		return true
	}
	if knnSkip { // zero-training kNN bridge (only set when the router isn't trained)
		return true
	}
	if ov := p.overridesSnap(); ov != nil {
		for _, d := range ov.Degraded {
			if d == entry {
				return true
			}
		}
	}
	return false
}

// knnPreferLargerEntry consults the zero-training kNN entry-tier pre-filter:
// true => skip the E2B tier and enter larger. It is a BRIDGE before the LR
// router has data — once the router is trained for this task, the router owns the
// decision and the kNN is skipped (no request-path embedding cost). Off unless
// KNNPreFilterEnabled loaded a substrate + embedder. Fail-open: any miss => false.
func (p *Pipeline) knnPreferLargerEntry(task core.TaskType, input string) bool {
	kn, embed := p.knnSnap()
	if kn == nil || embed == nil {
		return false
	}
	if task != core.TaskClassify && task != core.TaskTriage {
		return false
	}
	if p.routerSnap().HasTask(string(task)) { // nil-safe: false when no router yet
		return false // the trained router decides; don't pay the embed
	}
	vec, err := embed(input)
	if err != nil {
		return false
	}
	skip, ok := kn.PreferLargerEntry(string(task), vec, p.cfg.KNNPreFilterK, p.cfg.KNNMinNeighbors, p.cfg.KNNPreFilterThreshold)
	if !ok {
		return false
	}
	return skip
}

// entryFrom builds a ledger entry from per-call meta + the enriched signals.
func entryFrom(task core.TaskType, meta core.Meta, deferred bool, inputChars int) ledger.Entry {
	return ledger.Entry{
		Task: string(task), TokensIn: meta.TokensIn, TokensOut: meta.TokensOut,
		LatencyMs: meta.LatencyMs, TokPerSec: meta.TokPerSec, CacheHit: meta.CacheHit,
		Deferred: deferred,
		Margin:   meta.Margin, ModelTier: meta.Model, Escalations: meta.Escalations,
		Reasoning: meta.Reasoning,
		Retries:   meta.Retries, Truncated: meta.Truncated, Grounded: meta.Grounded,
		EscalatedAgreed: meta.EscalatedAgreed, ErrClass: meta.ErrClass,
		InputChars: inputChars, Feat: meta.Feat,
	}
}

func (p *Pipeline) record(task core.TaskType, meta core.Meta, inputChars int) {
	if p.led == nil {
		return
	}
	_ = p.led.Record(entryFrom(task, meta, false, inputChars))
}

// recordDefer logs a single deferred ledger entry for the final cascade
// outcome, carrying the human-readable defer reason (LO-8: err_class alone
// made incidents invisible — the Jul-1 GPU-contention defers all read as bare
// timeouts with no way to see WHY from the ledger).
func (p *Pipeline) recordDefer(task core.TaskType, meta core.Meta, inputChars int, reason string) {
	if p.led == nil {
		return
	}
	e := entryFrom(task, meta, true, inputChars)
	e.Reason = reason
	_ = p.led.Record(e)
}

// confidenceGate decides whether a validated triage/classify result is too shaky
// to accept and should escalate to a larger tier. It combines the model's
// self-reported confidence (classify) with the logprob-derived decision margin
// (both tasks). It ALWAYS returns the computed margin (0 if N/A) so the ledger
// can record it on success — that margin stream is what Phase 2 calibrates on.
// The threshold is per-task (data-derived via `calibrate`) with the config
// constant as fallback. Returns (reason, margin, escalate?).
func (p *Pipeline) confidenceGate(req core.Request, data []byte, lps []llamaclient.TokenLogprob) (string, float64, bool) {
	var margin float64
	switch req.Task {
	case core.TaskClassify:
		if labels := labelClasses(req.Params); len(labels) >= 2 {
			if m, ok := confidence.Margin(lps, "label", labels); ok {
				margin = m
			}
		}
		if conf, low := lowConfidence(data, p.cfg.ClassifyMinConfidence); low {
			return fmt.Sprintf("low confidence %.2f", conf), margin, true
		}
		if t := p.marginThreshold(req.Task); t > 0 && margin > 0 && margin < t {
			return fmt.Sprintf("low decision margin %.2f<%.2f", margin, t), margin, true
		}
	case core.TaskTriage:
		if m, ok := confidence.Margin(lps, "decision", []string{"yes", "no", "unsure"}); ok {
			margin = m
		}
		if t := p.marginThreshold(req.Task); t > 0 && margin > 0 && margin < t {
			return fmt.Sprintf("low decision margin %.2f<%.2f", margin, t), margin, true
		}
	}
	return "", margin, false
}

// marginThreshold returns the per-task escalation threshold: a data-derived
// conformal threshold (Phase 2, loaded from thresholds.json into p.thresholds)
// when present, else the config constant.
func (p *Pipeline) marginThreshold(task core.TaskType) float64 {
	if thr := p.thresholdsSnap(); thr != nil {
		if t, ok := thr[string(task)]; ok {
			return t
		}
	}
	return p.cfg.ConfidenceMarginThreshold
}

// labelClasses extracts the classify label set from request params, accepting
// either []string or []any (JSON-decoded).
func labelClasses(params map[string]any) []string {
	v, ok := params["labels"]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func lowConfidence(data []byte, threshold float64) (float64, bool) {
	var c struct {
		Confidence float64 `json:"confidence"`
	}
	if json.Unmarshal(data, &c) != nil {
		return 0, false
	}
	return c.Confidence, c.Confidence < threshold
}

var (
	reNumber = regexp.MustCompile(`\d[\d.,]*`)
	reCaps   = regexp.MustCompile(`\b[A-Z][A-Za-z0-9]+`)
)

// featurize extracts cheap, deterministic input features for the entry-tier
// router (Phase 5) — all len/regex ops, sub-millisecond, no inference.
func featurize(task core.TaskType, input string) map[string]float64 {
	bf := func(c bool) float64 {
		if c {
			return 1
		}
		return 0
	}
	return map[string]float64{
		"len_chars": float64(len(input)),
		"n_words":   float64(len(strings.Fields(input))),
		"n_numbers": float64(len(reNumber.FindAllString(input, -1))),
		"n_caps":    float64(len(reCaps.FindAllString(input, -1))),
		"has_code":  bf(strings.Contains(input, "```") || strings.Contains(input, "func ") || strings.Contains(input, "def ")),
		"has_url":   bf(strings.Contains(input, "http://") || strings.Contains(input, "https://")),
	}
}

// coldSwapIdle is the idle window after which a tier is assumed cold: on the
// swap-exclusive 8GB llama-swap, an unused alias is evicted whenever another
// model loads, and its next call blocks for the whole (re)load. 10 minutes is
// deliberately conservative — a false "cold" only exempts one timeout from
// breaker accounting; a false "warm" just counts a real swap timeout, and the
// 5-fails/10-window threshold absorbs occasional miscounts.
const coldSwapIdle = 10 * time.Minute

// noteTierCall stamps an attempt on model and reports whether that call was
// LIKELY to hit a llama-swap cold swap: the first call to the tier in this
// process, or the first after coldSwapIdle of tier inactivity.
func (p *Pipeline) noteTierCall(model string) bool {
	p.swapMu.Lock()
	defer p.swapMu.Unlock()
	now := time.Now
	if p.nowFn != nil {
		now = p.nowFn
	}
	t := now()
	if p.tierSeen == nil {
		p.tierSeen = map[string]time.Time{}
	}
	last, seen := p.tierSeen[model]
	p.tierSeen[model] = t
	return !seen || t.Sub(last) > coldSwapIdle
}

// breakerFailure reports whether an attempt outcome counts as an infra failure
// for the circuit breaker.
//
// Design note (LO-9, option b — exclude swap-window timeouts from breaker
// accounting): llama-swap QUEUES incoming requests while it loads a model, so
// the only failure shape a cold swap produces on this client is a plain
// whole-request timeout on the FIRST call to an idle tier (there is no
// "model loading" status to detect). Those timeouts mean "the model was still
// loading under GPU contention", not "the tier is sick" — counting them
// tripped the per-tier breakers during the Jul-1 GPU-contention incident and
// routed around healthy tiers for 20s at a time. We therefore exclude exactly
// (likely-cold-swap AND err_class=="timeout"); conn_refused / http_5xx / oom
// still count, and a WARM tier's timeout still counts. This was chosen over
// extending the first call's budget because it never holds a caller hostage
// beyond RequestTimeoutSec and is deterministic to unit-test.
func breakerFailure(errClass string, likelyColdSwap bool) bool {
	return errClass != "" && !(likelyColdSwap && errClass == "timeout")
}

// classifyErr buckets an infra error for the ledger + circuit breaker (Phase 3).
func classifyErr(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "out of memory") || strings.Contains(s, "cudamalloc") || strings.Contains(s, "oom"):
		return "oom"
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline") || strings.Contains(s, "context canceled"):
		return "timeout"
	case strings.Contains(s, "connection refused") || strings.Contains(s, "econnrefused") || strings.Contains(s, "no such host"):
		return "conn_refused"
	case strings.Contains(s, "llama-server 5"): // "llama-server 5xx: ..."
		return "http_5xx"
	default:
		return "other"
	}
}

// loadThresholds reads per-task conformal margin thresholds written by
// `local-offload calibrate`. Missing/unparseable => nil (use the config constant).
func loadThresholds(path string) map[string]float64 {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]float64
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// loadOverrides reads health-derived per-tier timeouts + degraded list (Phase 4).
func loadOverrides(path string) *tierOverrides {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var o tierOverrides
	if json.Unmarshal(b, &o) != nil {
		return nil
	}
	return &o
}

// maybeHeal (Phase 7) fires a single warmup request to force llama-swap to
// reload a tier whose breaker just tripped. Rate-limited per tier, opt-in
// (cfg.AutoHeal), off the request path (goroutine). A consequential-but-bounded
// recovery: one ping, ≤ once/60s/tier.
func (p *Pipeline) maybeHeal(tier string) {
	p.healMu.Lock()
	if time.Since(p.lastHeal[tier]) < 60*time.Second {
		p.healMu.Unlock()
		return
	}
	p.lastHeal[tier] = time.Now()
	p.healMu.Unlock()
	go func() {
		hctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = p.client.Generate(hctx, tier, "", "ok", "", 1, 0, 0) // tiny warmup
	}()
}

// goodExemplar gates which successful calls are harvested as few-shot examples:
// grounded (or N/A) and a confident margin (or N/A).
func goodExemplar(meta core.Meta) bool {
	if meta.Grounded != nil && !*meta.Grounded {
		return false
	}
	if meta.Margin > 0 && meta.Margin < 0.6 {
		return false
	}
	return true
}

// injectExemplars prepends a few-shot block (local-model tokens only) to the
// user prompt. Inputs are capped so the demonstrations stay small.
func injectExemplars(user string, ex []exemplars.Pair) string {
	var b strings.Builder
	b.WriteString("Examples of correct output for similar inputs:\n")
	for _, e := range ex {
		b.WriteString("INPUT: ")
		b.WriteString(truncateStr(e.Input, 400))
		b.WriteString("\nOUTPUT: ")
		b.WriteString(e.Output)
		b.WriteString("\n\n")
	}
	b.WriteString("Now do the same for the input below.\n\n")
	b.WriteString(user)
	return b.String()
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// --- Task 1.5: cascade-agreement correctness-proxy labels for classify/triage ---
//
// classify/triage get no grounding label (grounding doesn't apply). But when one
// of those calls escalates (the entry tier was low-confidence) and a larger tier
// then answers, AGREEMENT between the entry tier's candidate and the larger
// tier's answer is a strong proxy that the entry tier was correct. We record
// those labeled rows to a SEPARATE sidecar (never the main ledger, which feeds
// the router/calibration/savings and must stay pristine); only the confhead
// reads it. Labels accrue as escalation traffic occurs.

// answersAgree reports whether the entry-tier candidate and the final answer pick
// the same class. ok=false when the task isn't class-pinned or either side is
// unparseable / missing the class field.
func answersAgree(task core.TaskType, candidate string, finalData []byte) (agreed bool, ok bool) {
	var field string
	switch task {
	case core.TaskClassify:
		field = "label"
	case core.TaskTriage:
		field = "decision"
	default:
		return false, false
	}
	// Parser-extract the candidate first so it's cleaned the SAME way final.Data
	// was (final.Data is already parser-extracted). The entry candidate is raw
	// gen.Content — fenced/prose-wrapped/comma-trailing output would fail the
	// strict Unmarshal in jsonStringField and silently drop a valid agreement.
	cand, perr := parser.Extract(candidate)
	if perr != nil {
		return false, false
	}
	a := jsonStringField(cand, field) // cand is json.RawMessage ([]byte)
	b := jsonStringField(finalData, field)
	if a == "" || b == "" {
		return false, false
	}
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)), true
}

// AnswersAgree is a thin exported wrapper around answersAgree for use by the
// shadow-labeling flywheel (which lives in a separate package and cannot call the
// unexported function directly). task is a task-type string (e.g. "classify").
// Behavior is identical to answersAgree.
func AnswersAgree(task string, candidate string, finalData []byte) (agreed bool, ok bool) {
	return answersAgree(core.TaskType(task), candidate, finalData)
}

// jsonStringField returns the string value of `field` in a JSON object, or "" if
// the JSON is unparseable, the field is absent, or its value is not a string.
func jsonStringField(raw []byte, field string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	if v, ok := m[field]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// labelAgreement records a cascade-agreement correctness-proxy label for the
// entry tier to the confhead sidecar (best-effort telemetry; never fails the
// request). entry is the entry-tier (ci==0) feature snapshot; final is the
// larger tier's accepted result. No-op when the sidecar is unconfigured or the
// task isn't class-pinned / either answer is unparseable.
func (p *Pipeline) labelAgreement(task core.TaskType, entry ledger.Entry, candidate string, final core.Result, inputChars int) {
	if p.cfg.ConfHeadLabelsPath == "" {
		return
	}
	agreed, ok := answersAgree(task, candidate, final.Data)
	if !ok {
		return
	}
	entry.Grounded = nil
	entry.EscalatedAgreed = &agreed
	_ = ledger.AppendLabel(p.cfg.ConfHeadLabelsPath, entry)
}

// shadowCaptureTasks are the tasks whose non-escalated rows are captured into
// the shadow queue for nightly counterfactual labeling. Phase A judges
// classify/triage/extract with the existing in-process judges (answersAgree /
// grounding.Check); Phase B adds summarize (judged by the B2 summarize judge).
var shadowCaptureTasks = map[string]bool{"classify": true, "triage": true, "extract": true, "summarize": true}

// captureShadow appends a sampled, non-escalated entry-tier row to the shadow
// queue for nightly counterfactual labeling. Cheap (one append, no inference);
// best-effort (a queue error never affects the request). Capture is off by
// default (ShadowEnabled=false) and never touches the grammar/generation path.
func (p *Pipeline) captureShadow(req core.Request, e ledger.Entry, res core.Result) {
	if !p.cfg.ShadowEnabled || p.cfg.ShadowQueuePath == "" {
		return
	}
	if e.Escalations != 0 || !shadowCaptureTasks[strings.ToLower(e.Task)] {
		return
	}
	if rand.Float64() >= p.cfg.ShadowRate {
		return
	}
	_ = shadow.Enqueue(p.cfg.ShadowQueuePath, shadow.Item{
		TS:          e.TS,
		Task:        e.Task,
		Input:       req.Input,
		Params:      req.Params,
		EntryTier:   e.ModelTier,
		EntryOutput: string(res.Data),
		Feat:        e.Feat,
	})
}

// RunTier runs req through exactly the named tier (bypassing modelChain), with
// the full quality gate (grammar/verify/validate/ground/confidence) that attempt
// applies. It records NOTHING to the savings ledger — used by the offline
// shadow-labeling flywheel to evaluate a counterfactual tier without polluting
// the savings stats. Returns the tier's result and whether it was accepted.
func (p *Pipeline) RunTier(ctx context.Context, req core.Request, model string) (core.Result, bool) {
	start := time.Now()

	built, err := tasks.Build(req)
	if err != nil {
		return core.Result{}, false
	}
	feat := featurize(req.Task, req.Input)
	ck := cache.Key(string(req.Task), req.Input, tasks.StableParamsKey(req.Params), p.cfg.Model, built.Grammar)
	meta := core.Meta{Model: model, Feat: feat}
	res, _ := p.attempt(ctx, req, built, ck, model, meta, start, false /* record=false: no persistent side-effects */)
	// escalatable ignored: RunTier never escalates
	return res, res.OK
}
