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
	"errors"
	"fmt"
	"log"
	"math/big"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/dmmdea/offload-harness/internal/embedmemo"
	"github.com/dmmdea/offload-harness/internal/exemplars"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gbnf"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpulock"
	"github.com/dmmdea/offload-harness/internal/grounding"
	"github.com/dmmdea/offload-harness/internal/imagegen"
	"github.com/dmmdea/offload-harness/internal/imageio"
	"github.com/dmmdea/offload-harness/internal/judge"
	"github.com/dmmdea/offload-harness/internal/knn"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/mediahash"
	"github.com/dmmdea/offload-harness/internal/parser"
	"github.com/dmmdea/offload-harness/internal/router"
	"github.com/dmmdea/offload-harness/internal/rungraph"
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
	// T2-C embed memo backing p.embed. nil when disabled or unopenable — every
	// Memo method tolerates a nil receiver, so nothing branches on it except the
	// stats surface, which reports "no memo" rather than a fabricated zero.
	// embedMemoReason says WHICH of the several "no memo" causes applies; without
	// it the status surface can only publish an unfalsifiable three-way guess.
	embedMemo       *embedmemo.Memo
	embedMemoReason string
	// Label-judge coverage losses, split by cause because they support different
	// conclusions -- see labelAgreement. Only the unparseable count is a bias term, and only
	// it is persisted (loupe is a separate process). atomic: --serve shares one Pipeline
	// across concurrent handlers.
	labelDropsUnparseable     atomic.Int64
	labelDropsUnjudgeableTask atomic.Int64
	// T2-D: does RunTier read/write the result cache on THIS pipeline? Set only
	// by NewInLoopPipeline. False everywhere else — critically on the main
	// pipeline, whose RunTier the shadow-labelling flywheel drives to evaluate
	// counterfactual tiers, where a cache hit would replace the measurement.
	tierCache bool
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
	// owns the GPU the VLM cannot (re)load, so calling anyway just burns a doomed
	// HTTP call and returns an http_5xx defer — which hands the work back to the
	// calling session rather than calling any cloud model. gpuLockPath is resolved once
	// in New (config override > GPU_LOCK env > tmpdir default, same as the .mjs
	// runners); visionGPUWait/visionGPUPoll bound the pre-call wait; and
	// visionRetryWait is the one-retry backoff on a vision http_5xx. The three
	// durations are fields (not consts) so tests can shrink them.
	gpuLockPath     string
	visionGPUWait   time.Duration
	visionGPUPoll   time.Duration
	visionRetryWait time.Duration
	// Passive fleet footprint recording (docs/FLEET-NODE.md): every GPU render
	// carries a gpugen sampling hook keyed by this machine's bindings, so
	// measured VRAM peaks accumulate during NORMAL harness use, not just fleet
	// jobs. footOnce lazily opens the shared store (a footprints.json sibling
	// of the ledger/cache files); fleetSample overrides the composed sampler in
	// tests (nil = select per cfg.FleetSampler).
	footOnce    sync.Once
	foot        *fleetnode.Footprints
	fleetSample func(childPid int) (float64, error)
	// Opt-in image-prompt refiner seam (refiner.go): overrides the refiner's
	// chat call in tests (nil = p.client.Generate). Only reached when
	// cfg.ImageGenRefinerModel is set, so a client-less test Pipeline stays safe.
	refineGen func(ctx context.Context, model, system, user string, maxTokens int, temperature float64) (llamaclient.GenResult, error)
	// Process-level refiner fallback counters (refiner.go): total fallbacks and
	// the current consecutive run (reset on any success). They feed the
	// escalating server-log line; surfacing them in health/offload_status is a
	// recorded follow-up, not wired yet.
	refineFallbacks   atomic.Int64
	refineConsecutive atomic.Int64
	// TO-3 tier-aware repacking state (tierpack.go): per-model /props probes +
	// tokenizer clients for the escalation-boundary repack. Zero value ready.
	tierPack tierPackState
	// T2-A2 test seams (nil = the real thing), following the same pattern as
	// refineGen/fleetSample/nowFn. They exist because the media identity gates are
	// otherwise untestable at the pipeline level: a missing file defers inside
	// ffmpeg long before the gate, and a mid-call rotation cannot be injected
	// deterministically from outside. Without them the gates had NO regression
	// guard — deleting every `&& identifiable` / `&& cacheable` left the suite
	// green, which is exactly the refactor they exist to catch.
	mediaDigest    func(path string) (mediahash.Ident, error)
	mediaUnchanged func(id mediahash.Ident, path string) bool
}

// digestMedia resolves a media file's content identity (test seam aware).
func (p *Pipeline) digestMedia(path string) (mediahash.Ident, error) {
	if p.mediaDigest != nil {
		return p.mediaDigest(path)
	}
	return mediahash.Digest(path, p.cfg.MediaHashMaxFullBytes)
}

// mediaStillMatches re-checks, AFTER the consuming read, that the file is still
// the one that was hashed (test seam aware).
func (p *Pipeline) mediaStillMatches(id mediahash.Ident, path string) bool {
	if p.mediaUnchanged != nil {
		return p.mediaUnchanged(id, path)
	}
	return id.Unchanged(path)
}

// Cfg exposes the loaded config so callers like the MCP server can build
// side-channel tools (e.g. the explicit NIM remote tool) from the same
// configuration without re-loading it. It returns a shallow copy — read-only
// use only (the slice/map fields share backing with the live config).
func (p *Pipeline) Cfg() config.Config { return p.cfg }

// Cache exposes this pipeline's result-cache handle so a SECOND pipeline built in
// the same process (the in-loop offload — see recordless.go) can share the one
// open bbolt file rather than trying to open it again and losing the lock race
// against itself. May be nil (caching opted out, or the file is held elsewhere).
func (p *Pipeline) Cache() *cache.Cache { return p.cache }

// LabelDrops reports coverage losses for the confhead label corpus, split by cause.
//
// unparseable is the BIAS term: those candidates were judgeable in principle and are
// disproportionately extreme disagreements, so losing them pulls the published agreement
// rate upward. unjudgeableTask is a structural exclusion (not classify/triage) and carries
// no bias implication -- it is returned so a caller can explain the denominator, never to
// be added to the bias figure.
//
// These are this PROCESS's counts. The durable figure loupe reads is the persisted
// unparseable counter beside the sidecar; see appendDropCounter.
func (p *Pipeline) LabelDrops() (unparseable, unjudgeableTask int64) {
	return p.labelDropsUnparseable.Load(), p.labelDropsUnjudgeableTask.Load()
}

// EmbedMemoStats reports the embed memo's counters, or the REASON there are
// none. A caller must surface the reason rather than printing a zero hit rate,
// which would report a measured failure where no measurement exists.
//
// The reason is non-empty in exactly two cases: there is no memo (disabled, the
// pre-filter is off, the store could not be opened), or the memo exists but its
// counters could not be READ — which is a fault, not an empty store, and must
// never be published as "0 vectors, never consulted".
func (p *Pipeline) EmbedMemoStats() (embedmemo.Stats, string) {
	if p.embedMemo == nil {
		reason := p.embedMemoReason
		if reason == "" {
			// Unreachable via New, which sets a reason on both branches. Made
			// self-reporting rather than generic: a future constructor that builds
			// a Pipeline literal directly would otherwise silently reintroduce the
			// exact unfalsifiable "no memo" string this change set out to remove.
			reason = "BUG: pipeline constructed without New — embed-memo reason was never set"
		}
		return embedmemo.Stats{}, reason
	}
	st, err := p.embedMemo.Stats()
	if err != nil {
		return st, "store opened but unreadable: " + err.Error()
	}
	return st, ""
}

func New(cfg config.Config, c *llamaclient.Client, ca *cache.Cache, l *ledger.Ledger) *Pipeline {
	p := &Pipeline{cfg: cfg, client: c, cache: ca, led: l, lastHeal: map[string]time.Time{}, learnHashes: map[string]string{}}
	p.stt = sttclient.New(cfg.Endpoint, time.Duration(cfg.STTRequestTimeoutSec)*time.Second)
	// LO-1: resolve the shared GPU lock path ONCE, the same way the Node render
	// runners do, so the vision gate watches the exact lock the gen jobs hold.
	p.gpuLockPath = gpulock.Path(cfg.GPULockPath, cfg.StateDir)
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
		// T2-C: the embedder is memoized by exact input bytes. This is where the
		// memo earns most of its keep — the pre-filter runs on the REQUEST path,
		// so a repeat input skips not just the embedding compute but the ~1-2 s
		// cold-load the ttl=300 embedder pays after any idle gap. A disabled or
		// unopenable memo yields the plain live embedder, so the short timeout's
		// fail-open behaviour below is unchanged either way.
		mp, me, mx := cfg.EmbedMemoSettings()
		mz := judge.NewMemoizedEmbedder(cfg.Endpoint, cfg.EmbedModel(), time.Duration(cfg.KNNEmbedTimeoutMs)*time.Millisecond,
			judge.MemoOptions{Path: mp, Epoch: me, MaxEntries: mx})
		p.embed, p.embedMemo, p.embedMemoReason = mz.Embed, mz.Memo, mz.Reason
	} else {
		// The pre-filter is off (the default), so this pipeline embeds nothing and
		// deliberately does not open a store. Say so, rather than letting the
		// status surface guess between five different reasons for "no memo".
		p.embedMemoReason = "the kNN pre-filter is off (knn_prefilter_enabled=false), so this pipeline embeds nothing"
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
	// Model is the tier that actually PRODUCED this answer.
	//
	// Run and RunTier want different things from an entry: Run's key is
	// deliberately stable on the PRIMARY model so an answer produced anywhere in
	// the cascade is reused on a re-run, while RunTier pins ONE named tier and
	// must get that tier's output. With ExemplarShots at its default of 0 every
	// other ingredient coincided, so a triage-tier answer cached by Run was served
	// to an in-loop RunTier call pinned to the workhorse, with meta.Model
	// reporting the workhorse that never ran.
	//
	// That is now expressed in the KEY — see tierKeyspaceTag — because guarding
	// only the read left both paths writing the same entry and ping-ponging it.
	// The field is still WRITTEN on every RunTier cache Put; what is unreachable
	// from production is a MISMATCH, since the keyspace already separates the two
	// paths. It survives as defence in depth against a hand-crafted or externally
	// written entry.
	Model string `json:"model,omitempty"`
	// InLoop records that this entry was produced by the agent loop's in-loop
	// offload (T2-D), whose generation is deliberately never costed in the
	// savings ledger. Absent on every pre-T2-D entry, which reads as false —
	// correct, since nothing but the in-loop path can set it.
	InLoop bool `json:"in_loop,omitempty"`
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
	if req.Task == core.TaskVideoWatch {
		built, err := tasks.Build(req)
		if err != nil {
			return core.Deferf("build error: "+err.Error(), "", meta)
		}
		return p.runVideoWatch(ctx, req, built, meta, start)
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

	// inpaint_image re-renders ONLY the masked region of params.image on the local
	// ComfyUI by shelling out to comfy-inpaint.mjs (shared GPU lock + ComfyUI
	// lifecycle). Its own branch — no text cascade, no grammar, no vision call.
	if req.Task == core.TaskEditImageGenerative {
		return p.runEditImageGenerative(ctx, req, meta, start)
	}
	if req.Task == core.TaskInpaintImage {
		return p.runInpaintImage(ctx, req, meta, start)
	}
	// upscale_image enlarges params.image with an ESRGAN-family model on the local
	// ComfyUI by shelling out to comfy-upscale.mjs (shared GPU lock + ComfyUI
	// lifecycle). Its own branch — no text cascade, no grammar, no vision call.
	if req.Task == core.TaskUpscaleImage {
		return p.runUpscaleImage(ctx, req, meta, start)
	}

	// run_graph executes an arbitrary ComfyUI API-format graph + satisfies its node
	// manifest on the local ComfyUI by shelling out to comfy-run-graph.mjs (shared GPU
	// lock + ComfyUI lifecycle). Its own branch — no text cascade, no grammar, generic.
	if req.Task == core.TaskRunGraph {
		return p.runRunGraph(ctx, req, meta, start)
	}

	// pipeline-job runs an externally-provided pipeline CLI this node is configured
	// for (internal/config Config.Pipelines, Task 4) as a fleet task — 100%
	// config-driven, unlike every hardcoded route above. Its own branch — no text
	// cascade, no grammar, no machine-wide GPU lease (only the in-process
	// mediaSlot; see runPipelineJob's doc comment).
	if req.Task == core.TaskPipelineJob {
		return p.runPipelineJob(ctx, req, meta, start)
	}

	// agent executes a fleet-delegated coding-agent contract (agenttask.go) on
	// this node's agent seat. Its own branch — no text cascade, no grammar
	// build here (the structured re-pack builds its own), and its terminal
	// defers are encoded INSIDE the wire result as job-level successes, never
	// as pipeline defers (a defer is a SUCCESS shape at the job level).
	if req.Task == core.TaskAgentRun {
		return p.runAgentTask(ctx, req, meta, start)
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

	// edit_image / media are deterministic CPU ops (PIL/GIMP/ffmpeg via
	// internal/mediaops) — own branches, NO GPU lock, run in parallel with renders.
	if req.Task == core.TaskEditImage {
		return p.runEditImage(ctx, req, meta, start)
	}
	if req.Task == core.TaskMedia {
		return p.runMedia(ctx, req, meta, start)
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
		// Per-machine OCR output cap: a strong VLM can transcribe a dense page that
		// exceeds the built-in 1024 default (which otherwise truncates → defers the
		// whole OCR to cloud). Covers extract_image too — it re-enters via TaskOCR.
		if req.Task == core.TaskOCR && p.cfg.OCRMaxTokens > 0 {
			built.MaxTokens = p.cfg.OCRMaxTokens
		}
		return p.runVision(ctx, req, built, meta, start)
	}

	if contextbudget.IsTrivial(req.Input) {
		return core.Deferf("input too small to offload", "", meta)
	}
	// TO-3: retain the ORIGINAL source before any lossy packing. The entry
	// tier keeps today's char-budget packing exactly (hot path — no probes,
	// no tokenize round-trips); tiers the request CLIMBS to re-read from orig
	// against their own served windows (packForTier), instead of inheriting
	// the entry tier's cut — forwarding a small model's lossy view up the
	// ladder was a correctness bug class, not a tuning knob.
	orig := req.Input
	req.Input = compactForBudget(req.Input, p.cfg.MaxInputChars, p.cfg.GCFCompact)
	req.Input, _ = contextbudget.Trim(req.Input, p.cfg.MaxInputChars)
	entryPacked := req.Input
	meta.Feat = featurize(req.Task, req.Input) // cheap input features for the router (ENTRY view, deliberately — the router routes the entry tier)

	built, err := tasks.Build(req)
	if err != nil {
		return core.Deferf("build error: "+err.Error(), "", meta)
	}
	// Phase 6: prepend retrieved few-shot exemplars to the local-model prompt
	// (off by default — ExemplarShots=0). Grammar/schema/cache key are unchanged.
	// TO-3 round-1 findings: shots are retrieved ONCE, keyed on the ENTRY view,
	// and the SAME shots decorate every tier's rebuild — re-retrieving per tier
	// could silently hand a climbed tier different (or zero) shots, and an
	// undecorated repack measurement would let the injection outgrow the budget.
	shots := p.retrieveExemplars(req)
	decorate := func(b tasks.Built) tasks.Built {
		if len(shots) > 0 {
			b.User = injectExemplars(b.User, shots)
		}
		return b
	}
	built = decorate(built)
	// entryBuilt is retained so a failed per-tier rebuild restores the ENTRY
	// prompt outright — leaving a PREDECESSOR tier's packing in place would
	// make the "entry-inherited (rebuild: ...)" label lie about what the tier
	// actually saw (round-1 review finding).
	entryBuilt := built
	entryLen := len(entryPacked)

	// Cache key is stable on the PRIMARY model so a result produced by any tier
	// is reused on re-runs (the cascade is an internal detail of one logical call).
	// TO-3: keyed on the ORIGINAL input — the logical request's identity —
	// not the entry packing. For inputs under the char cap (the overwhelming
	// majority) the two are byte-identical, so cache continuity holds; an
	// oversized input re-keys once, and the old key COLLIDED two different
	// originals sharing a trim — the new key is strictly more correct.
	// Call identity (Phase 0.1). Computed once here, where BOTH the original input
	// and the fully-decorated prompt exist, then carried on meta so every ledger
	// row — success, defer, or cache hit — records the same fingerprints.
	meta.InputSHA256 = inputFingerprint(orig)
	meta.PromptPrefixSHA256 = promptPrefixFingerprint(built.System, userPreambleOf(built.User, orig))
	meta.ContextHash = p.contextHash()
	meta.ExemplarIDs = exemplarFingerprints(shots)

	// T2-A CORRECTNESS FIX: the key now also covers the prompt prefix (system +
	// grammar) and the injected-exemplar set.
	//
	// The bug this closes: the old key was (task, input, params, model, grammar)
	// and did NOT cover the system prompt or the exemplars. So editing a task's
	// system prompt — or regenerating exemplars/selected.json — left every prior
	// answer keyed identically, and the cache kept serving PRE-EDIT results
	// forever, with no signal anywhere. A prompt improvement silently did nothing.
	//
	// This can only ever SPLIT keys that were previously shared by mistake, so it
	// is a correctness change, not a hit-rate optimisation: old entries are not
	// wiped, they simply stop being reachable from a prompt that no longer matches
	// the template which produced them — which is exactly the intent.
	ck := cacheKeyFor(req.Task, orig, tasks.StableParamsKey(req.Params), p.cfg.Model, built, shots)
	if p.cache != nil {
		if raw, ok := p.cache.Get(ck); ok {
			var cv cacheVal
			if json.Unmarshal(raw, &cv) == nil && len(cv.Data) > 0 {
				meta.CacheHit = true
				// Carry the entry's provenance onto the row. The saving is real
				// either way, but "how much of the cache-hit rate did the harness
				// generate for itself?" is only answerable if this is recorded at
				// the moment of the hit.
				meta.CacheHitInLoop = cv.InLoop
				meta.TokensIn = cv.TokensIn
				meta.LatencyMs = time.Since(start).Milliseconds()
				p.record(req.Task, meta, entryLen)
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
		// TO-3: a climbed-to tier re-reads the ORIGINAL source against its own
		// served window instead of inheriting the entry cut. Fail-open to the
		// entry packing — byte-identical to the pre-TO-3 behavior — with the
		// disposition recorded on the row (tier_pack). The entry tier (ci==0)
		// is untouched: no probes, no tokenize round-trips on the hot path.
		if ci > 0 {
			tierInput, packPath := p.packForTier(ctx, model, orig, entryPacked, req, built.MaxTokens, decorate)
			meta.TierPack = packPath
			if tierInput != req.Input {
				treq := req
				treq.Input = tierInput
				if b2, berr := tasks.Build(treq); berr == nil {
					req.Input = tierInput
					built = decorate(b2)
				} else {
					meta.TierPack = "entry-inherited (rebuild: " + berr.Error() + ")"
					req.Input = entryPacked
					built = entryBuilt
				}
			}
		}
		likelyColdSwap := p.noteTierCall(model) // LO-9: before the attempt, so the window is per-call
		res, escalatable := p.attempt(ctx, req, built, ck, model, meta, start, true, entryLen)
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
				p.labelAgreement(req.Task, *entrySnapshot, entryCandidate, res, entryLen)
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
		// Carry WHY this call is climbing into the next tier's meta, so the
		// entry that finally SUCCEEDS attributes the climb. Without this the
		// source dies with the deferred attempt and the ledger stays silent on
		// exactly the rows that matter — a successful escalation.
		// Only-if-unset, so the value means "the gate that first sent this call
		// up", not "the last gate it happened to trip".
		if meta.EscSource == core.EscNone {
			meta.EscSource = res.Meta.EscSource
		}
		if !escalatable || ci == len(chain)-1 {
			break
		}
	}
	// Terminal LOCAL reasoning tier (grammar tasks only): after the whole cascade defers, give
	// a thinking model one shot under a think-wrapped grammar to reclaim the deferral before
	// falling through to Opus. A failure here defers exactly as before (never calls cloud).
	if p.cfg.ReasoningModel != "" && built.Grammar != "" && !last.Meta.Truncated {
		// TO-3: the terminal reasoning tier is a callee too — re-pack from the
		// original against ITS served window (same fail-open contract). Its
		// REAL completion request is MaxTokens+reasoningThinkBudget (the
		// think-wrapped grammar emits a <think> span before the JSON) — the
		// round-1 CRITICAL finding: budgeting against bare MaxTokens overshot
		// the served window by ~384 tokens on exactly the large inputs this
		// feature exists for.
		rInput, rPath := p.packForTier(ctx, p.cfg.ReasoningModel, orig, entryPacked, req, built.MaxTokens+reasoningThinkBudget, decorate)
		meta.TierPack = rPath
		if rInput != req.Input {
			treq := req
			treq.Input = rInput
			if b2, berr := tasks.Build(treq); berr == nil {
				req.Input = rInput
				built = decorate(b2)
			} else {
				meta.TierPack = "entry-inherited (rebuild: " + berr.Error() + ")"
				req.Input = entryPacked
				built = entryBuilt
			}
		}
		rres, ok := p.attemptReasoning(ctx, req, built, ck, meta, start, entryLen)
		if ok {
			return rres
		}
		last = rres
	}
	// input_chars keeps its historical ENTRY-view semantics on every row —
	// req.Input may hold a repacked tier view here, and the column is a
	// trained confhead feature (loginput) whose label stream is entry-scale
	// (round-1 review finding: silently shifting it desynchronized the same
	// row's len_chars and loginput and skewed train vs serve).
	p.recordDefer(req.Task, last.Meta, entryLen, last.Reason)
	return last
}

// retrieveExemplars fetches the Phase 6 few-shot pairs for req, keyed on the
// ENTRY view of the input. One retrieval per request — every tier decorates
// with the same shots.
func (p *Pipeline) retrieveExemplars(req core.Request) []exemplars.Pair {
	if p.cfg.ExemplarShots <= 0 || p.cfg.ExemplarsDir == "" {
		return nil
	}
	return exemplars.Retrieve(p.cfg.ExemplarsDir, string(req.Task), req.Input, p.cfg.ExemplarShots)
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

// visionModelFor picks the alias a vision task runs on. OCR gets its own binding
// when the machine has one: a purpose-built OCR model (GLM-OCR and friends) beats
// a general VLM on dense text, but is text-recognition ONLY — it cannot answer a
// vqa question or judge an image, so it must never become the vision tier itself.
// Empty ocr_model = the general vision model, exactly as before.
func (p *Pipeline) visionModelFor(t core.TaskType) string {
	if t == core.TaskOCR && p.cfg.OCRModel != "" {
		return p.cfg.OCRModel
	}
	return p.cfg.VisionModel
}

// runVision handles a single multimodal call on the VLM tier. It mirrors the
// text path's cache + ledger + defer machinery but uses GenerateVision and has
// NO grammar/grounding/confidence-margin gate — vqa is free-text, so it rides
// only empty-output, truncation, and infra defers. A bigger local tier is not
// available, so any defer goes straight to Opus (itself a strong VLM).
func (p *Pipeline) runVision(ctx context.Context, req core.Request, built tasks.Built, meta core.Meta, start time.Time) core.Result {
	// Resolve the alias ONCE: OCR may ride a dedicated ocr_model, and the call, the
	// cache key, the breaker and the ledger all have to agree on which model ran.
	model := p.visionModelFor(req.Task)
	// An empty model means "no vision route configured" (documented in
	// config.VisionModel). Guard FIRST: GenerateVision(ctx, "", ...) would fall back
	// to the TEXT model alias, misrouting an image request onto a text tier. Defer
	// to Opus (itself a strong VLM) instead — never call the model.
	if model == "" {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), "no vision model configured")
		return core.Deferf("no vision model configured", "", meta)
	}
	meta.Model = model
	dataURI, err := imageio.LoadImageB64(req.Image, p.cfg.VisionMaxImageBytes)
	if err != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), "image load: "+err.Error())
		return core.Deferf("image load: "+err.Error(), "", meta)
	}
	// WithoutThinking (offload-harness#168): a THINKING vision template spends
	// the whole budget in reasoning_content and returns empty content; every
	// vision task here is a describe/read/judge, never a reasoning step.
	return p.runVisionGen(ctx, req, built, meta, start, "img:"+sha256hex(dataURI), true, func(gctx context.Context) (llamaclient.GenResult, error) {
		return p.client.GenerateVision(gctx, model, built.System, built.User, []string{dataURI}, built.Grammar, built.MaxTokens, p.cfg.Temperature, 0, llamaclient.WithoutThinking())
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
func (p *Pipeline) runVisionGen(ctx context.Context, req core.Request, built tasks.Built, meta core.Meta, start time.Time, cacheKeyExtra string, cacheable bool, gen func(context.Context) (llamaclient.GenResult, error)) core.Result {
	// Same correctness fix as the text path (review finding: the first draft fixed
	// only the text cascade and left this one with the bug it was written to
	// eliminate). Vision system prompts are long, instruction-dense and exactly the
	// kind of thing that gets tuned — the OCR reading-order rules, assess_image's
	// field semantics — so editing one used to leave this cache serving pre-edit
	// transcriptions forever, silently. No exemplars are injected on the vision
	// path, so the exemplar tag is not an ingredient here.
	ck := cache.Key(string(req.Task), req.Input+"|"+cacheKeyExtra, tasks.StableParamsKey(req.Params), meta.Model, built.Grammar,
		templateCacheTag(built.System, built.Grammar, built.User, req.Input))
	// cacheable is false when the caller could not establish the source file's
	// CONTENT identity (T2-A2). Keying on anything else — a path, or a synthetic
	// error token — makes a transient read failure write a durable entry that a
	// DIFFERENT file at that path later hits. No identity, no cache: the work is
	// done and returned, nothing is stored.
	if p.cache != nil && cacheable {
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
	// during the Jul-1 incident every vision call 5xx'd and DEFERRED, handing the
	// work back to the calling session (295 of 337 all-time defers in ONE hour;
	// the harness itself never calls a cloud model). Wait for
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
	likelyColdSwap := p.noteTierCall(meta.Model)
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
		p.breakers.Record(meta.Model, !breakerFailure(ec, likelyColdSwap))
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
	if p.cache != nil && cacheable {
		if b, e := json.Marshal(cacheVal{Data: data, TokensIn: gres.TokensIn, Model: meta.Model, InLoop: p.tierCache}); e == nil {
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
	// IDENTIFY BEFORE CONSUMING, and ONCE — hoisted above both the retry loop and
	// the frame sampling. Two reasons: it is loop-invariant, so computing it
	// inside meant a full cold re-read of a multi-GB clip per retry (up to 4 at a
	// 2048 frame width); and a file rotated mid-loop would give the retry a
	// different identity than the attempt that looked it up, storing the result
	// under a digest for bytes the model never saw.
	vidID, vdErr := p.digestMedia(req.Video)
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
		// T2-A2: keyed on the video's CONTENT, not its path. The previous key was
		// the path STRING plus sampling params and was never hashed at all — so a
		// file replaced at the same path with the same sampling settings produced a
		// false HIT, serving the old video's description for the new one. The
		// sampling params stay in the key because they change what the model saw.
		//
		// An empty extra means "no identity": runVisionGen then bypasses the cache
		// entirely rather than keying on the path, which is what turned a transient
		// read failure into a durable wrong answer.
		// Verified AFTER the frames were sampled, not merely hoisted before it.
		// Hoisting alone bought the I/O win but widened the correctness gap: the
		// window became digest-at-t0 versus the FINAL successful SampleFrames,
		// which on a 4K vertical reel is the 4th iteration — minutes, after three
		// full ffmpeg passes. Worse, `frames=` below comes from that last sampling,
		// so a mid-loop rotation produced a key that was a hybrid of two file
		// states. One stat per iteration closes that.
		cacheable := vdErr == nil && p.mediaStillMatches(vidID, req.Video)
		extra := ""
		if cacheable {
			extra = fmt.Sprintf("vid:%s|fps=%g|n=%d|w=%d|frames=%d",
				vidID.Digest, p.cfg.VideoFPS, p.cfg.VideoMaxFrames, width, len(frames))
		} else if meta.CacheBypass == "" && p.cache != nil {
			why := "source changed during the call"
			if vdErr != nil {
				why = vdErr.Error()
			}
			meta.CacheBypass = "media identity: " + why
			log.Printf("video_describe: no stable content identity for %q (%s) — cache bypassed; result computed but not stored", req.Video, why)
		}
		res := p.runVisionGen(ctx, req, built, meta, start, extra, cacheable, func(gctx context.Context) (llamaclient.GenResult, error) {
			return p.client.GenerateVisionInterleaved(gctx, p.cfg.VisionModel, built.System, labels, frames, built.User, built.Grammar, built.MaxTokens, p.cfg.Temperature, 0, llamaclient.WithoutThinking())
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
	model, useOAI := sttRoute(p.cfg, paramBool(req.Params, "hq"))
	meta.Model = model

	lang := p.cfg.STTLanguage
	if l := paramStr(req.Params, "language"); l != "" {
		lang = l
	}
	if strings.EqualFold(lang, "auto") {
		lang = ""
	}

	// IDENTIFY BEFORE CONSUMING. The digest is taken FIRST, ahead of the ffmpeg
	// convert, so the identity belongs to (as nearly as possible) the same bytes
	// the conversion then reads. Hashing afterwards opened a TOCTOU window that
	// content-addressing makes WORSE rather than better: a producer rotating a
	// file between the convert and the hash would cache take A's transcript under
	// sha256(take B) — and that poisoned entry is then reachable from ANY path
	// holding B's bytes, not just the original one.
	audioID, adErr := p.digestMedia(req.Audio)

	// Convert (cheap, deterministic). A bad/missing file defers here.
	wav, cleanup, cerr := audioio.ConvertToWav16k(req.Audio, p.cfg.FFmpegPath)
	if cerr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Audio), "audio convert: "+cerr.Error())
		return core.Deferf("audio convert: "+cerr.Error(), "", meta)
	}
	defer cleanup()

	// Identity = source file CONTENT (sha256 of its bytes) + model + lang. Used for BOTH the
	// cache key AND the on-disk media filename so they agree and never collide
	// across distinct sources that share a basename (recording.m4a is common in
	// field audio) or across model/lang variants of the same source.
	// The protocol is part of the result's identity: the same alias re-bound across
	// protocols (whisper <-> mtmd) produces differently-shaped results (timestamped
	// segments vs one full-span segment), so a protocol flip must never serve the other
	// protocol's cached entry or on-disk media files. On the OAI path the language knob
	// does not apply, so it is excluded — otherwise each distinct language value would
	// re-transcribe and re-cache identical output (review findings, 2026-07-22).
	identLang := lang
	proto := "whisper"
	if useOAI {
		identLang = ""
		proto = "oai"
	}
	// Keyed on CONTENT, not on the path: an identical file at a second path must
	// hit, and a different file at the same path must miss.
	//
	// NO IDENTITY, NO CACHE. When the digest failed we do not know which bytes
	// this result describes, so the work is done and returned but nothing is
	// stored or looked up. Synthesising a key from the path + error text (an
	// earlier design) made a transient read failure write a durable PATH-keyed
	// entry — reintroducing, on the error path, precisely the false hit this
	// change exists to remove. `identifiable` also gates the on-disk media stem,
	// so two recordings that hit the same transient error cannot overwrite each
	// other's .srt/.txt.
	// VERIFY AFTER CONSUMING. Hashing before the convert did not close the TOCTOU
	// window, it transposed it — hash-then-read stores the NEW bytes' transcript
	// under the OLD digest instead of the reverse. One stat afterwards detects
	// both directions, and also the case no re-ordering can touch: a file still
	// being appended to, where the digest covers a prefix and ffmpeg read more.
	identifiable := adErr == nil && p.mediaStillMatches(audioID, req.Audio)
	// Only report a BYPASS where there was a cache to bypass. On a node with no
	// cache configured nothing was ever cacheable, so a cache_bypass row there is
	// noise — and core.Meta.CacheBypass's own doc excludes that case.
	if !identifiable && p.cache != nil {
		// Must be OBSERVABLE. Without this the call is byte-identical in telemetry
		// to an ordinary cold miss, so an input on a flaky mount re-runs whisper at
		// full cost on every call forever while the ledger shows a healthy run of
		// misses.
		why := "source changed during the call"
		if adErr != nil {
			why = adErr.Error()
		}
		meta.CacheBypass = "media identity: " + why
		log.Printf("transcribe: no stable content identity for %q (%s) — cache bypassed; result computed but not stored", req.Audio, why)
	}
	ident := fmt.Sprintf("%s|model=%s|lang=%s|proto=%s", audioID.Digest, model, identLang, proto)
	ck := cache.Key("transcribe", ident, tasks.StableParamsKey(req.Params), model, "")
	if p.cache != nil && identifiable {
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

	var tr sttclient.Result
	var terr error
	if useOAI {
		// The OAI path takes no whisper decode knobs (language is model-detected and
		// returned in the transcript prefix; no VAD/beam controls exist there).
		tr, terr = p.stt.TranscribeOAI(ctx, model, wav)
	} else {
		prm := sttclient.DefaultParams()
		prm.Language = lang
		if !p.cfg.STTVAD {
			prm.VAD = false
		}
		tr, terr = p.stt.Transcribe(ctx, model, wav, prm)
	}
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
	// When the input is unidentifiable, salt the on-disk stem with the start time
	// so two recordings that hit the same transient error at the same path cannot
	// overwrite each other's .srt/.txt — the returned srt_path would otherwise
	// point at a different recording's transcript.
	stemIdent := ident
	if !identifiable {
		// A random nonce, NOT a timestamp. Transcribe is reachable concurrently
		// (the fleet node serves each request in its own goroutine), so two
		// parallel calls on different unidentifiable files whose start lands in the
		// same wall-clock tick would produce the same stem — and, sharing a
		// basename as field audio routinely does, overwrite each other's
		// .srt/.txt while each returned a path to the other's transcript. That is
		// exactly the collision the salt exists to prevent, reproduced through the
		// salt itself.
		// crypto/rand.Read cannot fail on this toolchain — since Go 1.24 it always
		// fills the buffer and crashes the process irrecoverably if the OS source
		// errors. An earlier version wrapped it in an err check with a timestamp
		// fallback and a warning log; that branch was unreachable, so the commit
		// advertised a mitigation the code did not have. Ignoring the return is the
		// honest expression of the actual guarantee.
		var nonce [8]byte
		_, _ = crand.Read(nonce[:])
		stemIdent = fmt.Sprintf("%s|unidentified=%x", ident, nonce)
	}
	base := mediaBase(p.cfg.MediaDir, req.Audio, stemIdent)
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
	// Storing under a non-content key is what turns a transient read failure into
	// a permanent wrong answer — so an unidentifiable input is computed, returned,
	// and deliberately not persisted.
	if p.cache != nil && identifiable {
		if b, e := json.Marshal(cacheVal{Data: data, Model: model, InLoop: p.tierCache}); e == nil {
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
	// J2: per-machine media-engine seam. "sdcpp" routes to the stable-diffusion.cpp
	// runner (single Vulkan binary, no ComfyUI) — its own function so the ComfyUI
	// path below stays byte-for-byte unchanged. ""/"comfy" = the standing default.
	if p.cfg.ImageGenEngine == "sdcpp" {
		return p.runGenerateImageSdcpp(ctx, req, meta, start)
	}
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
	// Report the checkpoint this machine actually renders with, not a hardcoded model
	// family: the ledger would otherwise claim "sdxl" on a box running a DiT (HiDream).
	// UNBOUND keeps the historical "comfyui-sdxl" label on purpose: with no binding,
	// comfy-render.mjs really does default to SDXL, so the old label stays accurate —
	// and an existing machine's ledger/health tiers (health.groupByTier keys on this
	// string) must not fragment into a second tier just because it pulled this code.
	meta.Model = "comfyui-sdxl"
	if p.cfg.ImageGenCkpt != "" {
		meta.Model = "comfyui:" + p.cfg.ImageGenCkpt
	}

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
	// reuse one file; a seed/size change varies the hash). The "refine" knob is stripped
	// from the hash: it selects preprocessing, not render identity — with it included, the
	// same render forked into a second file even on refiner-less boxes.
	out := paramStr(req.Params, "out")
	if out == "" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		out = filepath.Join(p.cfg.MediaDir, "render-"+sha256hex(prompt + tasks.StableParamsKey(stripRefineParam(req.Params)))[:8]+".png")
	}

	// Opt-in prompt refiner (refiner.go): expand the raw prompt on the local text
	// tier BEFORE the render — and before the media lease, so the text call never
	// contends with our own render. Fail-safe: any refiner problem falls back to
	// the raw prompt. `out` above deliberately derives from the RAW prompt, so an
	// identical request keeps reusing one file across runs (the refined text is
	// sampled at temperature and would fragment the output path).
	renderPrompt, refined, refineNote, _ := p.maybeRefinePrompt(ctx, prompt, refineExplicitlyOff(req.Params))

	timeout := time.Duration(p.cfg.ImageGenTimeoutSec) * time.Second
	// This machine's image-model binding (per-machine config; never hardcoded here —
	// an 8GB box runs SDXL, a 16GB box may run an all-in-one DiT). All fields are
	// optional: a zero Model passes no flags and the renderer keeps its own defaults.
	model := imageModelFromConfig(p.cfg)
	// Passive fleet footprint: key this render by the machine's image binding
	// (family + the O1 bf16 quant) so measured peaks accumulate during normal use.
	imgFamily, imgQuant := imageFootprintKey(p.cfg)
	leaseEnv, releaseLease, lerr := p.acquireMediaLease("image-gen", timeout, p.gpuWait())
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer releaseLease()
	outPath, gerr := imagegen.Generate(ctx, p.cfg.NodePath, script, p.cfg.ComfyDir, out, renderPrompt, req.Params, model, timeout,
		p.footprintSampling(imgFamily, imgQuant, "image-gen"), leaseEnv...)
	if gerr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		meta.ErrClass = classifyErr(gerr)
		p.recordDefer(req.Task, meta, len(req.Input), "image generation failed: "+gerr.Error())
		return core.Deferf("image generation failed: "+gerr.Error(), "", meta)
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	payload := map[string]any{
		"image_path": outPath,
		"width":      paramIntOr(req.Params, "width", 1024),
		"height":     paramIntOr(req.Params, "height", 1024),
		"seed":       seed,
	}
	addRefineData(payload, p.cfg.ImageGenRefinerModel, refined, renderPrompt, refineNote)
	data, _ := json.Marshal(payload)
	p.record(req.Task, meta, len(prompt))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// runGenerateImageSdcpp renders req.Input via stable-diffusion.cpp (J2): a single
// native binary spawned per job under the same GPU lock — zero-warm by construction,
// no ComfyUI anywhere on the path (no COMFY_DIR in the env, no post-run /free). The
// AMD/Vulkan tier's engine; any failure defers exactly like the ComfyUI path.
func (p *Pipeline) runGenerateImageSdcpp(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	deferf := func(reason string) core.Result {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), reason)
		return core.Deferf(reason, "", meta)
	}
	if p.cfg.SdcppBin == "" {
		return deferf("imagegen_engine is sdcpp but sdcpp_bin is not configured")
	}
	if p.cfg.SdcppModel == "" {
		return deferf("imagegen_engine is sdcpp but sdcpp_model is not configured")
	}
	prompt := strings.TrimSpace(req.Input)
	if prompt == "" {
		return deferf("empty image prompt")
	}
	scriptCfg := p.cfg.SdcppScript
	if scriptCfg == "" {
		scriptCfg = "render/sdcpp-generate.mjs"
	}
	script, serr := gpugen.ResolveScript(scriptCfg)
	if serr != nil {
		return deferf(serr.Error())
	}
	// Ledger/health tier: the sdcpp engine is its own tier keyed by the bound model
	// file — never the ComfyUI labels (health.groupByTier must not merge engines).
	meta.Model = "sdcpp:" + filepath.Base(p.cfg.SdcppModel)
	// Same seed-pinning contract as the ComfyUI path: the reported seed must be the
	// seed actually rendered, so mint one before the run when the caller sent none.
	seed := paramIntOr(req.Params, "seed", 0)
	if seed <= 0 {
		seed = mintSeed()
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		req.Params["seed"] = seed
	}
	// Default the size EXPLICITLY (review finding): sd.cpp's own default is 512x512,
	// so without this the result metadata would claim 1024 while the file was 512 -
	// and 1024 is Z-Image's native resolution (quality-first default).
	for _, k := range []string{"width", "height"} {
		if paramIntOr(req.Params, k, 0) <= 0 {
			req.Params[k] = 1024
		}
	}
	out := paramStr(req.Params, "out")
	if out == "" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		out = filepath.Join(p.cfg.MediaDir, "render-"+sha256hex(prompt + tasks.StableParamsKey(stripRefineParam(req.Params)))[:8]+".png")
	}
	// Opt-in prompt refiner — same shared decision point as the ComfyUI path
	// (refiner.go), same raw-prompt-derived, refine-knob-stripped `out` rationale.
	renderPrompt, refined, refineNote, _ := p.maybeRefinePrompt(ctx, prompt, refineExplicitlyOff(req.Params))
	timeout := time.Duration(p.cfg.ImageGenTimeoutSec) * time.Second
	m := imagegen.SdcppModel{
		Bin:       p.cfg.SdcppBin,
		Model:     p.cfg.SdcppModel,
		ModelKind: p.cfg.SdcppModelKind,
		VAE:       p.cfg.SdcppVAE,
		ClipL:     p.cfg.SdcppClipL,
		ClipG:     p.cfg.SdcppClipG,
		T5:        p.cfg.SdcppT5,
		LLM:       p.cfg.SdcppLLM,
		Steps:     p.cfg.ImageGenSteps,
		CFG:       p.cfg.ImageGenCFG,
		Sampler:   p.cfg.ImageGenSampler,
		ExtraArgs: p.cfg.SdcppExtraArgs,
	}
	imgFamily, imgQuant := imageFootprintKey(p.cfg)
	leaseEnv, releaseLease, lerr := p.acquireMediaLease("image-gen (sdcpp)", timeout, p.gpuWait())
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer releaseLease()
	outPath, gerr := imagegen.GenerateSdcpp(ctx, p.cfg.NodePath, script, out, renderPrompt, req.Params, m, timeout,
		p.footprintSampling(imgFamily, imgQuant, "image-gen"), leaseEnv...)
	if gerr != nil {
		meta.ErrClass = classifyErr(gerr)
		return deferf("image generation failed: " + gerr.Error())
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	payload := map[string]any{
		"image_path": outPath,
		"width":      paramIntOr(req.Params, "width", 1024),
		"height":     paramIntOr(req.Params, "height", 1024),
		"seed":       seed,
	}
	addRefineData(payload, p.cfg.ImageGenRefinerModel, refined, renderPrompt, refineNote)
	data, _ := json.Marshal(payload)
	p.record(req.Task, meta, len(prompt))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// runInpaintImage re-renders ONLY the masked region of params.image on the LOCAL
// ComfyUI (generative inpainting). SDXL-family binding required (inpaint_*): a
// pixel-space DiT (HiDream) cannot drive VAEEncodeForInpaint. Any failure defers.
func (p *Pipeline) runInpaintImage(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	defer1 := func(reason string) core.Result {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), reason)
		return core.Deferf(reason, "", meta)
	}
	if p.cfg.InpaintScript == "" || p.cfg.InpaintCkpt == "" {
		return defer1("no inpaint route configured")
	}
	prompt := strings.TrimSpace(req.Input)
	if prompt == "" {
		return defer1("empty inpaint prompt")
	}
	image := paramStr(req.Params, "image")
	mask := paramStr(req.Params, "mask")
	if image == "" {
		return defer1("inpaint requires params.image")
	}
	// EXPERIMENTAL auto_text (CLI --auto-text): no mask given — chain the vision
	// text-box detector into a mask_boxes mask (inpaint_autotext.go). Any doubt in
	// that chain errors here and defers, naming the manual mask_boxes workflow.
	//
	// Grounding eval PASSED 2026-07-17 (plan Task 9 gate, previously an always-defer):
	// 3/3 text-stamped renders grounded correctly (qwen3vl on this stack) — text
	// found, boxed, erased; zero wrong-region repaints. An oversized image defers
	// cleanly on the vqa load limit. Evidence:
	// docs/superpowers/evidence/2026-07-17-nightshift-run-graph.md.
	if mask == "" && paramBool(req.Params, "auto_text") {
		am, aerr := p.autoTextMask(ctx, image)
		if aerr != nil {
			return defer1("auto text localization failed: " + aerr.Error() + " — build a mask with edit-image mask_boxes instead")
		}
		mask = am
	}
	if mask == "" {
		return defer1("inpaint requires params.mask")
	}
	script, serr := gpugen.ResolveScript(p.cfg.InpaintScript)
	if serr != nil {
		return defer1(serr.Error())
	}
	meta.Model = "comfyui-inpaint:" + p.cfg.InpaintCkpt
	// Pin a concrete seed BEFORE the render (same reproducibility rule as
	// runGenerateImage: the runner would otherwise pick a random seed and the
	// result would report a wrong one).
	seed := paramIntOr(req.Params, "seed", 0)
	if seed <= 0 {
		seed = mintSeed()
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		req.Params["seed"] = seed
	}
	out := paramStr(req.Params, "out")
	if out == "" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		out = filepath.Join(p.cfg.MediaDir, "inpaint-"+sha256hex(image + prompt + tasks.StableParamsKey(req.Params))[:8]+".png")
	}
	m := imagegen.InpaintModel{
		Ckpt: p.cfg.InpaintCkpt, VAE: p.cfg.InpaintVAE, Steps: p.cfg.InpaintSteps,
		CFG: p.cfg.InpaintCFG, Sampler: p.cfg.InpaintSampler, Scheduler: p.cfg.InpaintScheduler,
	}
	timeout := time.Duration(p.cfg.InpaintTimeoutSec) * time.Second
	leaseEnv, releaseLease, lerr := p.acquireMediaLease("inpaint", timeout, p.gpuWait())
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer releaseLease()
	outPath, gerr := imagegen.Inpaint(ctx, p.cfg.NodePath, script, p.cfg.ComfyDir, out, image, mask, prompt, req.Params, m, timeout, leaseEnv...)
	if gerr != nil {
		meta.ErrClass = classifyErr(gerr)
		return defer1("inpaint failed: " + gerr.Error())
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(map[string]any{"image_path": outPath, "seed": seed})
	p.record(req.Task, meta, len(prompt))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// runUpscaleImage enlarges params.image with this machine's ESRGAN-family model on
// the LOCAL ComfyUI. No prompt, no seed: the model is deterministic for a given
// input, so the out-path hash is input + params only. Any failure defers.
func (p *Pipeline) runUpscaleImage(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	defer1 := func(reason string) core.Result {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), reason)
		return core.Deferf(reason, "", meta)
	}
	if p.cfg.UpscaleScript == "" {
		return defer1("no upscale route configured (upscale_script unset)")
	}
	model := p.cfg.EffectiveUpscaleModel()
	if s, ok := req.Params["model"].(string); ok && s != "" {
		// A ComfyUI model NAME relative to upscale_models/ (subfolders are fine — the
		// loader lists them as "ESRGAN/4x.pth"). An absolute or parent-escaping path
		// can only be a typo or a different models root, and ComfyUI would reject it
		// after the cold start — refuse it here.
		if filepath.IsAbs(s) || hasDriveLetter(s) || strings.HasPrefix(s, "/") || strings.HasPrefix(s, `\`) || hasParentSegment(s) {
			return defer1("upscale model must be a name relative to ComfyUI's upscale_models/ (subfolders allowed), got " + s)
		}
		model = s
	}
	if model == "" {
		return defer1("no upscale route configured (upscale_model / videogen_upscale_model unset)")
	}
	meta.Model = "comfyui-upscale:" + model
	image := paramStr(req.Params, "image")
	if image == "" {
		return defer1("upscale requires params.image")
	}
	if fi, err := os.Stat(image); err != nil || fi.IsDir() {
		return defer1("upscale input not found: " + image)
	}
	w, h := paramIntOr(req.Params, "width", 0), paramIntOr(req.Params, "height", 0)
	if w < 0 || h < 0 {
		return defer1("upscale width and height must be positive integers")
	}
	if (w > 0) != (h > 0) {
		return defer1("upscale width and height must be given together")
	}
	if w > upscaleMaxResolution || h > upscaleMaxResolution {
		return defer1(fmt.Sprintf("upscale width and height must be <= %d (ComfyUI limit), got %dx%d", upscaleMaxResolution, w, h))
	}
	scale := 0.0
	if _, present := req.Params["scale"]; present {
		scale = paramFloat(req.Params, "scale")
		if scale <= 0 {
			return defer1("upscale scale must be > 0")
		}
		req.Params["scale"] = scale // normalized: upscaleArgs forwards float64 only
	}
	if m := paramStr(req.Params, "method"); m != "" && !upscaleMethods[m] {
		return defer1("upscale method must be one of lanczos|bicubic|bilinear|area|nearest-exact, got " + m)
	}
	// The size the request fixes, if any: a pinned width/height exactly; a scale from the
	// measured source (PNG/JPEG here — an unmeasurable source leaves the check to the
	// runner, which pins the size itself when it can read the header). The written file
	// is compared against it after the render: a silent wrong size never reports OK.
	srcW, srcH := imagegen.OutputSize(image)
	srcFormat := imagegen.SourceFormat(image)
	expW, expH := 0, 0
	switch {
	case w > 0:
		expW, expH = w, h
	case scale > 0 && srcW > 0:
		expW, expH = int(float64(srcW)*scale+0.5), int(float64(srcH)*scale+0.5)
		if expW > upscaleMaxResolution || expH > upscaleMaxResolution {
			return defer1(fmt.Sprintf("upscale scale %g on a %dx%d source needs %dx%d, above ComfyUI's %d limit", scale, srcW, srcH, expW, expH, upscaleMaxResolution))
		}
	}
	script, serr := gpugen.ResolveScript(p.cfg.UpscaleScript)
	if serr != nil {
		return defer1(serr.Error())
	}
	out := paramStr(req.Params, "out")
	if out == "" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		out = filepath.Join(p.cfg.MediaDir, "upscale-"+sha256hex(image + tasks.StableParamsKey(req.Params))[:8]+".png")
	}
	timeout := time.Duration(p.cfg.UpscaleTimeoutSec) * time.Second
	leaseEnv, releaseLease, lerr := p.acquireMediaLease("upscale", timeout, p.gpuWait())
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer releaseLease()
	outPath, gerr := imagegen.Upscale(ctx, p.cfg.NodePath, script, p.cfg.ComfyDir, out, image, req.Params, imagegen.UpscaleModel{Model: model}, timeout, leaseEnv...)
	if gerr != nil {
		meta.ErrClass = classifyErr(gerr)
		return defer1("upscale failed: " + gerr.Error())
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	// Size is read from the file the runner wrote, never predicted: gpugen proved the
	// file exists and is non-empty, so a header that does not decode means the bytes
	// are not the PNG this route promises — a defer, not a size-less success.
	ow, oh := imagegen.OutputSize(outPath)
	if ow <= 0 || oh <= 0 {
		return defer1("upscale wrote an undecodable file at " + outPath)
	}
	if expW > 0 && (absInt(ow-expW) > 2 || absInt(oh-expH) > 2) {
		return defer1(fmt.Sprintf("upscale produced %dx%d, expected %dx%d for %s — the written file is at %s", ow, oh, expW, expH, upscaleSizeRequest(w, h, scale, srcW, srcH, srcFormat), outPath))
	}
	result := map[string]any{"image_path": outPath, "model": model, "width": ow, "height": oh}
	// factor is the measured output/source ratio; a pinned non-uniform size has two.
	// "Uniform" is decided in pixels, not ratios: a uniform scale on a small odd-sized
	// source rounds each axis independently (3x5 at 2.33 → 7x12, ratios 2.33 vs 2.4),
	// so the height is uniform when it is within 1 px of what the width's factor predicts.
	if srcW > 0 && srcH > 0 {
		fx, fy := round2(float64(ow)/float64(srcW)), round2(float64(oh)/float64(srcH))
		uniformH := int(float64(ow)*float64(srcH)/float64(srcW) + 0.5)
		if absInt(oh-uniformH) <= 1 {
			result["factor"] = fx
		} else {
			result["factor_x"], result["factor_y"] = fx, fy
		}
	}
	data, _ := json.Marshal(result)
	p.record(req.Task, meta, 0)
	return core.Result{OK: true, Data: data, Meta: meta}
}

// upscaleMaxResolution is ComfyUI's core MAX_RESOLUTION for ImageScale width/height;
// a larger value is rejected server-side AFTER the GPU slot and cold start.
const upscaleMaxResolution = 16384

// upscaleMethods are ImageScale/ImageScaleBy's resamplers (ComfyUI core), the same
// five the graph builder accepts.
var upscaleMethods = map[string]bool{"lanczos": true, "bicubic": true, "bilinear": true, "area": true, "nearest-exact": true}

// hasParentSegment reports whether a relative model name climbs out of its root: a
// ".." PATH SEGMENT on either separator. A ".." inside a filename ("4x..pth") is odd
// but legal and must not trip it.
func hasParentSegment(s string) bool {
	for _, seg := range strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

// hasDriveLetter is the platform-independent "C:..." test: filepath.VolumeName only
// knows drive letters on Windows, and this gate must behave the same on a Linux fleet
// node (a drive-relative name is a typo everywhere).
func hasDriveLetter(s string) bool {
	return len(s) >= 2 && s[1] == ':' && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z'))
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func round2(v float64) float64 { return float64(int(v*100+0.5)) / 100 }

// runnerMeasures reports whether render/image-size.mjs can read this source format —
// the three it implements. For those a size mismatch means the renderer did not honor
// the pinned size; for anything else (gif: Go reads it, the runner cannot) the runner
// fell back to the model's filename factor, and the fix is different.
func runnerMeasures(format string) bool {
	return format == "png" || format == "jpeg" || format == "webp"
}

// upscaleSizeRequest names what fixed the expected size, for the mismatch defer, and
// why it can have been missed.
func upscaleSizeRequest(w, h int, scale float64, srcW, srcH int, srcFormat string) string {
	if w > 0 {
		return fmt.Sprintf("the pinned %dx%d (the renderer did not honor the requested size)", w, h)
	}
	if runnerMeasures(srcFormat) {
		return fmt.Sprintf("scale %g on a %dx%d %s source (the runner pinned that size and the renderer did not honor it)", scale, srcW, srcH, srcFormat)
	}
	return fmt.Sprintf("scale %g on a %dx%d %s source (the runner cannot measure this format and used the model's filename factor instead — pin width+height, or use a PNG/JPEG/WebP source)", scale, srcW, srcH, srcFormat)
}

// runEditImageGenerative rewrites the WHOLE of params.image from a text instruction
// on the LOCAL ComfyUI — no mask (Qwen-Image-Edit class). Distinct from
// runInpaintImage, which needs a mask and an SDXL-class latent binding. Any failure
// defers.
func (p *Pipeline) runEditImageGenerative(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	defer1 := func(reason string) core.Result {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), reason)
		return core.Deferf(reason, "", meta)
	}
	if p.cfg.GenEditScript == "" || p.cfg.GenEditUnet == "" {
		return defer1("no generative edit route configured")
	}
	prompt := strings.TrimSpace(req.Input)
	if prompt == "" {
		return defer1("empty edit instruction")
	}
	image := paramStr(req.Params, "image")
	if image == "" {
		return defer1("generative edit requires params.image")
	}
	script, serr := gpugen.ResolveScript(p.cfg.GenEditScript)
	if serr != nil {
		return defer1(serr.Error())
	}
	meta.Model = "comfyui-edit:" + p.cfg.GenEditUnet
	// Pin a concrete seed BEFORE the render, same reproducibility rule as
	// runGenerateImage/runInpaintImage: otherwise the runner mints its own and the
	// reported seed would not reproduce the image.
	seed := paramIntOr(req.Params, "seed", 0)
	if seed <= 0 {
		seed = mintSeed()
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		req.Params["seed"] = seed
	}
	out := paramStr(req.Params, "out")
	if out == "" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		out = filepath.Join(p.cfg.MediaDir, "edit-"+sha256hex(image + prompt + tasks.StableParamsKey(req.Params))[:8]+".png")
	}
	m := imagegen.EditModel{
		Unet: p.cfg.GenEditUnet, Preset: p.cfg.GenEditPreset, LoRA: p.cfg.GenEditLoRA,
		LoRAStrength: p.cfg.GenEditLoRAStrength, CLIP: p.cfg.GenEditCLIP, VAE: p.cfg.GenEditVAE,
		Steps: p.cfg.GenEditSteps, CFG: p.cfg.GenEditCFG,
		Sampler: p.cfg.GenEditSampler, Scheduler: p.cfg.GenEditScheduler,
		Megapixels: p.cfg.GenEditMegapixels,
	}
	timeout := time.Duration(p.cfg.GenEditTimeoutSec) * time.Second
	leaseEnv, releaseLease, lerr := p.acquireMediaLease("edit", timeout, p.gpuWait())
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer releaseLease()
	outPath, gerr := imagegen.Edit(ctx, p.cfg.NodePath, script, p.cfg.ComfyDir, out, image, prompt, req.Params, m, timeout, leaseEnv...)
	if gerr != nil {
		meta.ErrClass = classifyErr(gerr)
		return defer1("generative edit failed: " + gerr.Error())
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(map[string]any{"image_path": outPath, "seed": seed})
	p.record(req.Task, meta, len(prompt))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// ImageBatchJob is one line of a generate-image --batch JSONL: a prompt plus the
// per-job overridable request params. Out/Seed are filled by normalizeImageBatch
// when absent (same invariants as the single-render path).
type ImageBatchJob struct {
	Prompt   string `json:"prompt"`
	Negative string `json:"negative,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Steps    int    `json:"steps,omitempty"`
	Seed     int    `json:"seed,omitempty"`
	Out      string `json:"out,omitempty"`
	// Refine is the per-job opt-out mirroring the single path's "refine" param:
	// an EXPLICIT false skips the configured prompt refiner for this job; nil or
	// true means the configured default applies. Pointer so absent != false.
	Refine *bool `json:"refine,omitempty"`
}

// ImageBatchItem is the per-job outcome of a batch, in job order.
type ImageBatchItem struct {
	Out   string `json:"out"`
	Seed  int    `json:"seed"`
	OK    bool   `json:"ok"`
	Ms    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
	// Refiner outcome (refiner.go), mirroring the single path's result keys.
	// Refined is a *bool so the SHAPE matches the single path: with a refiner
	// configured every item carries "refined" (true or false); with none
	// configured the field is nil/omitted and the item is byte-identical to the
	// pre-refiner harness.
	Refined        *bool  `json:"refined,omitempty"`
	RefinedPrompt  string `json:"refined_prompt,omitempty"`
	RefineFallback string `json:"refine_fallback,omitempty"`
}

// normalizeImageBatch fills the per-job invariants the single-render path enforces
// (a concrete seed BEFORE the render so the report is reproducible; a stable output
// path under mediaDir) and renders the jobs file the render script consumes. Pure.
func normalizeImageBatch(jobs []ImageBatchJob, mediaDir string) ([]ImageBatchJob, string) {
	norm := make([]ImageBatchJob, len(jobs))
	for i, j := range jobs {
		if j.Seed <= 0 {
			j.Seed = mintSeed()
		}
		if j.Out == "" {
			// Same dedup key as the single path (which hashes req.Params INCLUDING
			// negative): two jobs differing only in negative must not share an output
			// path, or the second silently overwrites the first.
			params := map[string]any{"seed": j.Seed, "width": j.Width, "height": j.Height, "steps": j.Steps, "negative": j.Negative}
			j.Out = filepath.Join(mediaDir, "render-"+sha256hex(j.Prompt + tasks.StableParamsKey(params))[:8]+".png")
		}
		norm[i] = j
	}
	return norm, jobsJSONL(norm)
}

// jobsJSONL renders the jobs file the batch render script consumes — one JSON
// line per job. Shared by normalizeImageBatch and the refiner pass (which
// rewrites prompts after normalization and must re-render the same shape).
func jobsJSONL(jobs []ImageBatchJob) string {
	var b strings.Builder
	for _, j := range jobs {
		line, _ := json.Marshal(j)
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// parseBatchResults maps the script's results JSONL back onto the job list by index.
// A job with no recorded line (script died mid-batch) gets an explicit failed item so
// callers never silently lose a job. Pure.
func parseBatchResults(raw []byte, norm []ImageBatchJob) []ImageBatchItem {
	byIdx := map[int]ImageBatchItem{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r struct {
			I     int    `json:"i"`
			Out   string `json:"out"`
			Seed  int    `json:"seed"`
			OK    bool   `json:"ok"`
			Ms    int64  `json:"ms"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		byIdx[r.I] = ImageBatchItem{Out: r.Out, Seed: r.Seed, OK: r.OK, Ms: r.Ms, Error: r.Error}
	}
	items := make([]ImageBatchItem, len(norm))
	for i, j := range norm {
		if it, ok := byIdx[i]; ok {
			if it.Out == "" {
				it.Out = j.Out
			}
			if it.Seed == 0 {
				it.Seed = j.Seed
			}
			items[i] = it
		} else {
			items[i] = ImageBatchItem{Out: j.Out, Seed: j.Seed, OK: false, Error: "no result recorded (batch aborted?)"}
		}
	}
	return items
}

// batchErrClass classifies a failed batch item for the ledger's ErrClass, mirroring
// the single path's classifyErr. A job with no recorded result line died with the
// batch itself, so the batch-level error (gerr) is its true cause; a job with its own
// error line is classified from that. Pure.
func batchErrClass(itemErr string, gerr error) string {
	if gerr != nil && strings.Contains(itemErr, "no result recorded") {
		return classifyErr(gerr)
	}
	return classifyErr(fmt.Errorf("%s", itemErr))
}

// RunImageBatch renders N prompts through ONE warm ComfyUI session (the checkpoint
// loads once) while preserving zero-always-warm AT THE BATCH BOUNDARY: the render
// script's single teardown + gpugen's deferred /free restore a clean GPU when the
// batch ends, however it ends. Ledger: one entry per job, same model label as the
// single-render path (health tiers must not fragment).
func (p *Pipeline) RunImageBatch(ctx context.Context, jobs []ImageBatchJob) ([]ImageBatchItem, error) {
	if p.cfg.ImageGenScript == "" {
		return nil, fmt.Errorf("no image-gen route configured")
	}
	if len(jobs) == 0 {
		return nil, fmt.Errorf("empty batch")
	}
	for i, j := range jobs {
		if strings.TrimSpace(j.Prompt) == "" {
			return nil, fmt.Errorf("job %d: empty prompt", i)
		}
	}
	script, serr := gpugen.ResolveScript(p.cfg.ImageGenScript)
	if serr != nil {
		return nil, serr
	}
	_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
	norm, jsonl := normalizeImageBatch(jobs, p.cfg.MediaDir)
	// The batch file stamp derives from the RAW jobs, BEFORE any refinement:
	// refined text is temperature-sampled, so hashing it would mint new
	// jobs/results filenames for every run of an identical batch.
	stamp := sha256hex(jsonl)[:8]
	// Opt-in prompt refiner (refiner.go) — the batch parity of the single path's
	// pre-render refinement, via the SAME decision point. It runs AFTER
	// normalization, so each job's Out still derives from its RAW prompt (stable
	// across re-runs), and BEFORE the media lease, so the text-tier calls never
	// contend with our own renders. Per-job "refine": false opts a job out;
	// every fallback leaves that job's raw prompt in place.
	//
	// CIRCUIT BREAKER: a HUNG refiner would otherwise stall the whole batch —
	// timeout × N jobs, all before the first render, invisible as GPU activity
	// (a 220-job batch at the 30s default is ~110 min of nothing). After
	// refinerBreakerLimit CONSECUTIVE transport/timeout-class failures the
	// remaining jobs skip the refiner and are marked accordingly; guard-class
	// rejections (the model answered, the output failed a guard) never trip it.
	var refinedFlags []bool
	var refineNotes []string
	if p.cfg.ImageGenRefinerModel != "" {
		refinedFlags = make([]bool, len(norm))
		refineNotes = make([]string, len(norm))
		changed := false
		consecutive := 0
		disabledNote := ""
		for i := range norm {
			if norm[i].Refine != nil && !*norm[i].Refine {
				continue // per-job opt-out: no call, refined:false, no fallback note
			}
			if disabledNote != "" {
				refineNotes[i] = disabledNote
				p.refineFallbacks.Add(1)
				continue
			}
			rp, ok, note, transient := p.maybeRefinePrompt(ctx, norm[i].Prompt, false)
			refinedFlags[i], refineNotes[i] = ok, note
			if ok {
				norm[i].Prompt = rp
				changed = true
				consecutive = 0
				continue
			}
			if !transient {
				consecutive = 0
				continue
			}
			if consecutive++; consecutive >= refinerBreakerLimit {
				disabledNote = fmt.Sprintf("refiner disabled after %d consecutive failures", consecutive)
				log.Printf("imagegen prompt refiner: %s — skipping refinement for the remaining %d batch jobs", disabledNote, len(norm)-i-1)
			}
		}
		if changed {
			jsonl = jobsJSONL(norm)
		}
	}
	jobsPath := filepath.Join(p.cfg.MediaDir, "batch-"+stamp+".jobs.jsonl")
	resultsPath := filepath.Join(p.cfg.MediaDir, "batch-"+stamp+".results.jsonl")
	if err := os.WriteFile(jobsPath, []byte(jsonl), 0o644); err != nil {
		return nil, err
	}

	// Same labeling rule as runGenerateImage: report the checkpoint this machine
	// actually renders with; UNBOUND keeps the historical "comfyui-sdxl" label so
	// health tiers don't fragment.
	modelLabel := "comfyui-sdxl"
	if p.cfg.ImageGenCkpt != "" {
		modelLabel = "comfyui:" + p.cfg.ImageGenCkpt
	}
	model := imageModelFromConfig(p.cfg)
	// The whole batch shares one timeout: per-image budget × N (the first job also
	// absorbs the ComfyUI cold start, which the per-image budget already covers today).
	timeout := time.Duration(p.cfg.ImageGenTimeoutSec) * time.Second * time.Duration(len(norm))
	// ONE lease for the WHOLE batch. That is the point of batching: the tier is torn
	// down once for N renders instead of once each, which is the arithmetic behind the
	// 3,356 unloads in the server log.
	// This helper returns items+error rather than a core.Result, so a busy card surfaces
	// as an error for the caller to classify — no items were produced.
	leaseEnv, releaseLease, lerr := p.acquireMediaLease("image-gen batch", timeout, p.gpuWait())
	if lerr != nil {
		return nil, lerr
	}
	defer releaseLease()
	gerr := imagegen.GenerateBatch(ctx, p.cfg.NodePath, script, p.cfg.ComfyDir, jobsPath, resultsPath, model, timeout, leaseEnv...)

	raw, _ := os.ReadFile(resultsPath) // best-effort even on gerr: partial results are real work
	items := parseBatchResults(raw, norm)
	for i, it := range items {
		// Refiner outcome onto the item (single-path result-key parity: with a
		// refiner configured every item says refined true/false). Set BEFORE the
		// ledger loop reads `items` so callers and records agree.
		if refinedFlags != nil {
			v := refinedFlags[i]
			items[i].Refined = &v
			if v {
				items[i].RefinedPrompt = norm[i].Prompt
			} else {
				items[i].RefineFallback = refineNotes[i]
			}
		}
		// Ledger input-chars parity with the single path: the RAW prompt is the
		// caller's input (jobs[i]; norm[i].Prompt may hold the refined text).
		meta := core.Meta{Model: modelLabel, LatencyMs: it.Ms}
		if it.OK {
			p.record(core.TaskGenerateImage, meta, len(jobs[i].Prompt))
		} else {
			// Ledger parity with the single path (which sets ErrClass=classifyErr):
			// health analytics must distinguish oom/timeout/busy for batch jobs too.
			meta.ErrClass = batchErrClass(it.Error, gerr)
			p.recordDefer(core.TaskGenerateImage, meta, len(jobs[i].Prompt), "batch job failed: "+it.Error)
		}
	}
	if gerr != nil {
		return items, fmt.Errorf("image batch failed: %w", gerr)
	}
	return items, nil
}

// buildRunGraphParams maps request params → rungraph.Params. A missing graph path is a
// hard error (mapped to a clean defer upstream), never a silent empty run.
func buildRunGraphParams(req core.Request) (rungraph.Params, error) {
	gp := paramStr(req.Params, "graph_path")
	if gp == "" {
		return rungraph.Params{}, fmt.Errorf("run_graph: graph_path required")
	}
	return rungraph.Params{
		GraphPath:    gp,
		ManifestPath: paramStr(req.Params, "manifest_path"),
		OutDir:       paramStr(req.Params, "out_dir"),
		ResultPath:   paramStr(req.Params, "result_path"),
		ReserveVram:  paramStr(req.Params, "reserve_vram"),
	}, nil
}

// resolveOutDir picks the run-graph output directory — the caller's out_dir if given,
// else the media dir — and ensures it exists. Creating a caller-supplied directory here
// (not only the defaulted media dir) is what stops a not-yet-existing out_dir from
// ENOENT-ing at first output write and surfacing as an opaque RUN_ERROR.
func resolveOutDir(mediaDir, callerOutDir string) (string, error) {
	dir := callerOutDir
	if dir == "" {
		dir = mediaDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// runRunGraph executes an arbitrary ComfyUI API-format graph + satisfies its node manifest
// on the LOCAL ComfyUI by shelling out to comfy-run-graph.mjs (shared GPU lock + ComfyUI
// lifecycle via internal/rungraph → gpugen). Its own branch — no text models, no grammar,
// generic. Any failure (no route, missing graph, satisfier/preflight DEFER, render error,
// timeout) defers to Claude. params: graph_path (required), manifest_path, out_dir,
// result_path, reserve_vram. Returns the node-addressed envelope JSON.
func (p *Pipeline) runRunGraph(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	if p.cfg.RunGraphScript == "" {
		return p.deferGen(req, meta, start, len(req.Input), "no run-graph route configured")
	}
	params, err := buildRunGraphParams(req)
	if err != nil {
		return p.deferGen(req, meta, start, len(req.Input), err.Error())
	}
	// Default the result envelope + output dir under the media dir so an inline caller need
	// not pick paths; a stable name lets a re-run reuse one file.
	if params.ResultPath == "" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		params.ResultPath = filepath.Join(p.cfg.MediaDir, "run-graph-"+sha256hex(params.GraphPath + params.ManifestPath)[:8]+".json")
	}
	outDir, oerr := resolveOutDir(p.cfg.MediaDir, params.OutDir)
	if oerr != nil {
		return p.deferGen(req, meta, start, len(req.Input), "cannot create out_dir: "+oerr.Error())
	}
	params.OutDir = outDir
	// LO-2: resolve a relative script path against the exe dir (an MCP host spawns us with
	// no meaningful cwd) and defer with a distinct reason when missing.
	script, serr := gpugen.ResolveScript(p.cfg.RunGraphScript)
	if serr != nil {
		return p.deferGen(req, meta, start, len(req.Input), serr.Error())
	}
	meta.Model = "comfyui-run-graph"

	timeout := time.Duration(p.cfg.ImageGenTimeoutSec) * time.Second
	// Passive fleet footprint: family from a payload-declared model_family (the
	// fleet dispatch path threads it) else the generic comfy-graph bucket.
	leaseEnv, releaseLease, lerr := p.acquireMediaLease("run-graph", timeout, p.gpuWait())
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer releaseLease()
	env, gerr := rungraph.Run(ctx, p.cfg.NodePath, script, p.cfg.ComfyDir, params, timeout,
		p.footprintSampling(runGraphFootprintFamily(req.Params), "", "run-graph"), leaseEnv...)
	if gerr != nil {
		meta.ErrClass = gpugen.ClassifyErr(gerr)
		return p.deferGen(req, meta, start, len(req.Input), "run-graph failed: "+gerr.Error())
	}
	// fix #4: a handled failure inside the mjs now arrives as a typed DEFER in the envelope
	// (the mjs exits 0 so gpugen succeeds). Surface the typed code to the caller.
	if env.Deferred {
		return p.deferGen(req, meta, start, len(req.Input), env.Code+": "+env.Detail)
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(env)
	p.record(req.Task, meta, len(req.Input))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// pipelineJobResult is the pipeline-job task family's wire result
// (shared-contracts.md, verbatim). FinalPath MUST stay the FIRST field: Go
// struct fields marshal in declaration order, and the web viewmodel harvests
// artifacts in insertion order (artifacts[0] == final_path).
type pipelineJobResult struct {
	FinalPath    string  `json:"final_path"`
	QaReportPath string  `json:"qa_report_path,omitempty"`
	JobID        string  `json:"job_id"`
	Tier         string  `json:"tier"`
	DurationSec  float64 `json:"duration_sec"`
}

// sceneSwapFailPrefix is the CMP CLI contract's stderr marker on a handled
// failure: "SCENE-SWAP-FAIL stage=<last completed stage or none>: <message>".
const sceneSwapFailPrefix = "SCENE-SWAP-FAIL"

// runPipelineJob runs an externally-provided pipeline CLI this node is
// configured for (cfg.Pipelines[taskType], Task 4's PipelineSpec — e.g.
// "scene-swap") by shelling out to spec.Script via gpugen, mirroring
// runRunGraph's shape but config-driven end to end: taskType (which
// cfg.Pipelines entry to use), job_id/job_path/out_root/tier all arrive
// through req.Params, exactly as internal/fleetnode's buildPipelineJob (ack
// time) materialized them — every payload validation error already happened
// there; this function never re-validates the payload.
//
// GPU arbitration is DELIBERATELY narrower than every other GPU-gen route:
// only the in-process mediaSlot is acquired (takeMediaSlot/releaseMediaSlot),
// NEVER the machine-wide media lease (acquireMediaLease is not called). The
// CMP CLI's own nested per-stage calls take the machine-wide lease themselves
// for each stage (mirroring how a manual scene-swap run already works today —
// see the brief); acquiring it AGAIN here would self-deadlock the same
// process waiting on its own nested acquisition. A second concurrent
// pipeline-job in this process still can't race the first: it queues on
// mediaSlot for up to gpuWait() and then defers as gpu_busy, exactly like
// every other route's busy-card behavior.
//
// On a CHILD RENDER failure (non-zero exit) this returns a PLAIN failure —
// core.Result{OK:false, Reason: ...} — NOT a Deferred result: unlike the
// Claude-facing routes, a fleet job has no interactive caller to hand work
// back to, so a real render failure is a real failure. The last stderr line
// starting with SCENE-SWAP-FAIL is surfaced as Reason when present (gpugen's
// error already embeds up to the last 400 bytes of combined output); anything
// else (including a timeout-kill, which prints no such line) falls back to
// the generic exec error. Every OTHER defer path here (no route configured,
// missing script, gpu_busy) stays a normal Deferf — those are dispatch/
// contention conditions, not a broken render.
func (p *Pipeline) runPipelineJob(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	taskType := paramStr(req.Params, "pipeline_task")
	spec, ok := p.cfg.Pipelines[taskType]
	if !ok {
		return p.deferGen(req, meta, start, len(req.Input), "no pipeline route configured for "+taskType)
	}
	// Hoisted ABOVE takeMediaSlot (SP3 polish): validatePipelines already
	// rejects an empty artifacts list at config Load() time, so this only
	// fires for a Config assembled directly in-process (bypassing Load — see
	// PipelineSpec.valid()'s doc comment). Such a job can never publish a
	// result no matter how long it waits for the GPU slot, so refuse it
	// before burning any queue wait on a config that can never run.
	if len(spec.Artifacts) == 0 {
		return p.deferGen(req, meta, start, len(req.Input), "pipeline "+taskType+": no artifacts configured")
	}
	jobID := paramStr(req.Params, "job_id")
	jobPath := paramStr(req.Params, "job_path")
	outRoot := paramStr(req.Params, "out_root")
	tier := paramStr(req.Params, "tier")

	meta.Model = "pipeline:" + taskType

	script, serr := gpugen.ResolveScript(spec.Script)
	if serr != nil {
		return p.deferGen(req, meta, start, len(req.Input), serr.Error())
	}

	wait := p.gpuWait()
	if !takeMediaSlot(wait) {
		err := &errGPUBusy{detail: fmt.Sprintf("another generation job in this process still holds the card after %s", wait)}
		return p.deferForLease(err, req.Task, meta, len(req.Input), start)
	}
	defer releaseMediaSlot()

	timeout := time.Duration(spec.TimeoutSec) * time.Second
	gspec := gpugen.Spec{
		Exe:     p.cfg.NodePath,
		Script:  script,
		Args:    []string{"--job", jobPath, "--tier", tier, "--out", outRoot},
		Dir:     spec.Workdir,
		Out:     filepath.Join(outRoot, jobID, spec.Artifacts[0]),
		Timeout: timeout,
	}
	// Passive fleet footprint (mirrors image-gen's plumbing): no family/quant
	// claim — a pipeline job's sizing rides on its own task-scoped footprint
	// entry, Record("", "", taskType, peak), not an advertised model family.
	p.footprintSampling("", "", taskType).ApplyTo(&gspec)

	_, gerr := gpugen.Generate(ctx, gspec)
	if gerr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		meta.ErrClass = gpugen.ClassifyErr(gerr)
		reason := sceneSwapFailReason(gerr.Error())
		if reason == "" {
			reason = "pipeline " + taskType + " failed: " + gerr.Error()
		}
		return core.Result{OK: false, Reason: reason, Meta: meta}
	}

	published, perr := publishPipelineArtifacts(outRoot, jobID, spec.Artifacts, p.cfg.MediaDir)
	if perr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		return core.Result{OK: false, Reason: perr.Error(), Meta: meta}
	}

	meta.LatencyMs = time.Since(start).Milliseconds()
	result := pipelineJobResult{
		FinalPath:   published[0], // published is index-aligned with spec.Artifacts; [0] is required, never "".
		JobID:       jobID,
		Tier:        tier,
		DurationSec: time.Since(start).Seconds(),
	}
	// QaReportPath binds to Artifacts[1] SPECIFICALLY (never a later index that
	// happened to publish) — present iff Artifacts[1] was actually published.
	if len(published) > 1 && published[1] != "" {
		result.QaReportPath = published[1]
	}
	data, _ := json.Marshal(result)
	p.record(req.Task, meta, len(req.Input))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// sceneSwapFailReason extracts the last SCENE-SWAP-FAIL line out of a gpugen
// error message (which embeds up to the last 400 bytes of the child's
// combined stdout+stderr, wrapped as "... (<tail>)"). Returns "" when absent
// (a timeout-kill, or any failure before the CLI could print one), so the
// caller falls back to the generic exec error.
func sceneSwapFailReason(errMsg string) string {
	idx := strings.LastIndex(errMsg, sceneSwapFailPrefix)
	if idx < 0 {
		return ""
	}
	rest := errMsg[idx:]
	if nl := strings.IndexAny(rest, "\r\n"); nl >= 0 {
		rest = rest[:nl]
	} else {
		// No trailing newline in the child's output: gpugen's own format
		// string wraps the tail in one closing paren right after it.
		rest = strings.TrimSuffix(rest, ")")
	}
	return strings.TrimSpace(rest)
}

// publishPipelineArtifacts copies each configured artifact from the CLI's
// out/<id>/ dir to cfg.MediaDir as "<id>-<artifact>" (flat, bare names — the
// fleet media route rejects any separator).
//
// The returned slice is INDEX-ALIGNED with artifacts (same length): published[i]
// is the destination path for artifacts[i], or "" when that artifact was
// missing. This matters because the caller binds specific RESULT fields to
// specific indices (FinalPath <- [0], QaReportPath <- [1]) — a compacted
// slice would skew those bindings whenever a middle artifact is missing (a
// later, present artifact would silently slide into an earlier field's
// slot). The PRIMARY artifact (index 0) is required — gpugen's Out-stat gate
// already proved it exists, but a missing/unreadable file here is still
// surfaced as an error defensively; any OTHER (optional) artifact that is
// missing leaves its slot "" — never an error. Every configured artifact,
// including ones beyond index 1, is still published to MediaDir when
// present; only index 0 and 1 are ever NAMED in the JSON result (see
// docs/FLEET-NODE.md's "Pipeline-job task families" section) — a 3rd+
// artifact's caller must know its published name out of band.
func publishPipelineArtifacts(outRoot, jobID string, artifacts []string, mediaDir string) ([]string, error) {
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return nil, fmt.Errorf("pipeline publish: creating media dir: %w", err)
	}
	srcDir := filepath.Join(outRoot, jobID)
	published := make([]string, len(artifacts))
	for i, name := range artifacts {
		src := filepath.Join(srcDir, name)
		data, rerr := os.ReadFile(src)
		if rerr != nil {
			if i == 0 {
				return nil, fmt.Errorf("pipeline publish: primary artifact %q missing: %w", name, rerr)
			}
			// Two distinct optional-artifact outcomes used to look identical
			// (both a silent "continue"): the CLI simply never wrote this
			// artifact (a legitimate, expected shape — e.g. no QA report
			// configured for this tier) versus the CLI DID write it but this
			// node failed to read it back (permissions, a half-flushed file,
			// disk pressure) — the latter is an operational problem worth a
			// log line, the former is not.
			if os.IsNotExist(rerr) {
				log.Printf("pipeline publish: optional artifact %q not produced for job %s (skipping)", name, jobID)
			} else {
				log.Printf("pipeline publish: optional artifact %q was produced but could not be read for job %s: %v", name, jobID, rerr)
			}
			continue // optional artifact missing/unreadable: leave published[i] == "", never an error
		}
		dest := filepath.Join(mediaDir, jobID+"-"+name)
		if werr := os.WriteFile(dest, data, 0o644); werr != nil {
			if i == 0 {
				return nil, fmt.Errorf("pipeline publish: writing primary artifact %q: %w", name, werr)
			}
			// Read succeeded, so the artifact WAS produced — the failure is
			// purely in the copy-to-mediaDir step.
			log.Printf("pipeline publish: optional artifact %q was produced but copy to media dir failed for job %s: %v", name, jobID, werr)
			continue
		}
		published[i] = dest
	}
	if published[0] == "" {
		return nil, fmt.Errorf("pipeline publish: primary artifact %q missing", artifacts[0])
	}
	return published, nil
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
	// Resolve the family ONCE, here, and derive every downstream use from it.
	// 0.73.0 computed the ledger label from config alone while the runner arg was
	// resolved 30 lines below with the OPPOSITE precedence (an explicit per-request
	// `model` wins), so any caller using the documented override got a ledger row
	// naming this box's configured seat for a render that used another family —
	// a false provenance value, which is worse than the vague label it replaced.
	// Two call sites re-deriving the same precedence is what caused that, so they
	// are now one call.
	argModel, renderFamily := resolveVideoFamily(p.cfg, paramStr(req.Params, "model"))
	meta.Model = videoModelLabel(renderFamily)

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
		out = filepath.Join(p.cfg.MediaDir, "video-"+sha256hex(prompt + tasks.StableParamsKey(req.Params))[:8]+".mp4")
	}

	// comfy-video.mjs CLI: <out> <still> "<prompt>" [--model ..] [--frames N] ...
	args := []string{out}
	if still != "" {
		args = append(args, still)
	}
	args = append(args, prompt)
	// Family routing: resolved ONCE above by resolveVideoFamily (an explicit
	// per-request model wins; else the machine's configured videogen_family;
	// else the runner's own wan default, expressed by passing no --model at all).
	// argModel matches what this block computed before 0.73.1 for every
	// non-whitespace input. It is NOT byte-identical in general, and an earlier
	// revision of this comment wrongly claimed it was: resolveVideoFamily now
	// TrimSpaces both the request and the config family, where the old block
	// trimmed neither. So `model:" "` on a bound box used to send a single space
	// (which the runner's exact-match dispatch rejected, silently rendering Wan)
	// and now sends the configured family; a config family with stray whitespace
	// behaves the same way. Both changes are CORRECTIVE — the old behavior was to
	// silently ignore the binding — but they are behavior changes, so they are
	// stated rather than papered over.
	if argModel != "" {
		args = append(args, "--model", argModel)
	}
	if n := paramStr(req.Params, "negative"); n != "" {
		args = append(args, "--negative", n)
	}
	// Per-machine resolution/frame defaults (this box may run 720p; the 8GB laptop
	// stays at the builder's 480p default). A per-request value always wins; a 0
	// config default means "use the builder default". steps/seed have no machine
	// default (map lookup -> 0 -> unaffected).
	machineDefault := map[string]int{"width": p.cfg.VideoGenWidth, "height": p.cfg.VideoGenHeight, "frames": p.cfg.VideoGenFrames}
	for _, k := range []string{"frames", "width", "height", "steps", "seed"} {
		v := paramIntOr(req.Params, k, 0)
		if v <= 0 {
			v = machineDefault[k]
		}
		if v > 0 {
			args = append(args, "--"+k, strconv.Itoa(v))
		}
	}
	// invariant 5: --reserve-vram stays per-workflow-overridable (default lives in the
	// runner; Wan 14B=2.0, ACE-Step differs). Pass it through ONLY when the caller set it.
	if rv := paramStr(req.Params, "reserve_vram"); rv != "" {
		args = append(args, "--reserve-vram", rv)
	}
	// Per-machine Wan weight binding (quality-first): this box's configured expert weights
	// + text encoder, by filename. Unset = the render script's defaults (unchanged).
	if p.cfg.VideoGenUnetHigh != "" {
		args = append(args, "--high-unet", p.cfg.VideoGenUnetHigh)
	}
	if p.cfg.VideoGenUnetLow != "" {
		args = append(args, "--low-unet", p.cfg.VideoGenUnetLow)
	}
	if p.cfg.VideoGenTextEncoder != "" {
		args = append(args, "--text-encoder", p.cfg.VideoGenTextEncoder)
	}
	// LTX-2.5 family bindings (quality-first weight binding, same pattern as the
	// Wan flags above): filenames + fps + the pooled-DiT placement from config.
	if p.cfg.VideoGenTransformer != "" {
		args = append(args, "--transformer", p.cfg.VideoGenTransformer)
	}
	if p.cfg.VideoGenVideoVAE != "" {
		args = append(args, "--video-vae", p.cfg.VideoGenVideoVAE)
	}
	if p.cfg.VideoGenAudioVAE != "" {
		args = append(args, "--audio-vae", p.cfg.VideoGenAudioVAE)
	}
	if p.cfg.VideoGenLatentUpscaler != "" {
		args = append(args, "--latent-upscaler", p.cfg.VideoGenLatentUpscaler)
	}
	if p.cfg.VideoGenFPS > 0 {
		args = append(args, "--fps", strconv.Itoa(p.cfg.VideoGenFPS))
	}
	if p.cfg.VideoGenPoolVvramGB > 0 {
		args = append(args, "--pool-vvram-gb", strconv.FormatFloat(p.cfg.VideoGenPoolVvramGB, 'f', -1, 64))
		if p.cfg.VideoGenPoolCompute != "" {
			args = append(args, "--pool-compute", p.cfg.VideoGenPoolCompute)
		}
		if p.cfg.VideoGenPoolDonor != "" {
			args = append(args, "--pool-donor", p.cfg.VideoGenPoolDonor)
		}
	}
	// hero: native no-LoRA quality pass (per-request). upscale: use THIS machine's configured
	// upscale model + target size (per-machine config; a machine with none just skips it).
	// Both universal -- no model name baked into shared code.
	if paramBool(req.Params, "hero") {
		args = append(args, "--hero") // backward compat: native IS the default now
	}
	// Quality-first: the distilled lightx2v speed path is an explicit OPT-IN.
	if paramBool(req.Params, "fast") {
		args = append(args, "--fast")
	}
	if paramBool(req.Params, "upscale") && p.cfg.VideoGenUpscaleModel != "" {
		args = append(args, "--upscale-model", p.cfg.VideoGenUpscaleModel)
		if p.cfg.VideoGenUpscaleWidth > 0 && p.cfg.VideoGenUpscaleHeight > 0 {
			args = append(args, "--upscale-width", strconv.Itoa(p.cfg.VideoGenUpscaleWidth), "--upscale-height", strconv.Itoa(p.cfg.VideoGenUpscaleHeight))
		}
	}

	timeout := time.Duration(p.cfg.VideoGenTimeoutSec) * time.Second
	leaseEnv, releaseLease, lerr := p.acquireMediaLease("video-gen", timeout, p.gpuWait())
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer releaseLease()
	// COMFY_WAIT_SEC aligns the render script's poll budget with the harness timeout
	// (quality-first: the native recipe at 720p legitimately exceeds the script's old
	// hardcoded ceiling; the Go timeout stays the hard stop).
	env := append(p.genEnv(), leaseEnv...)
	if timeout > 0 {
		env = append(env, "COMFY_WAIT_SEC="+strconv.Itoa(int(timeout/time.Second)))
	}
	spec := gpugen.Spec{
		Exe:     p.cfg.NodePath,
		Script:  script,
		Args:    args,
		Env:     env,
		Out:     out,
		Timeout: timeout,
	}
	// Passive fleet footprint, keyed on the family that ACTUALLY renders.
	// Until 0.73.1 this hardcoded "wan2.2" and derived the quant from the Wan
	// GGUF filenames, which stay bound on this box as the fallback family — so on
	// an ltx25 box every LTX render was recorded as `wan2.2/q8_0`, poisoning a
	// store the fleet reads for placement with a family whose real VRAM profile is
	// nothing like it (measured: an int8-convrot LTX-2.5 render logged 24.1 GiB
	// under Wan's key). The quant only means anything for the Wan GGUF recipe, so
	// it is reported for that family alone rather than guessed for the others.
	p.footprintSampling(videoFootprintFamily(renderFamily), videoFootprintQuant(p.cfg, renderFamily), "video-gen").ApplyTo(&spec)
	outPath, gerr := gpugen.Generate(ctx, spec)
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
		out = filepath.Join(p.cfg.MediaDir, kind+"-"+sha256hex(text + tasks.StableParamsKey(req.Params))[:8]+ext)
	}

	// CLI: tts.mjs <out> "<text>" [--clone ref] [--lang es]
	//      music worker <out> "<style>" --seed N [--seconds N] [--lyrics ..] [--reserve-vram ..]
	args := []string{out, text}
	if kind == "voice" {
		switch voice := paramStr(req.Params, "voice"); voice {
		case "", "generalist":
			// stock multilingual path: a request clone wins; else the machine's default es-MX ref.
			ref := paramStr(req.Params, "clone")
			if ref == "" {
				ref = p.cfg.VoiceGenRef
			}
			if ref != "" {
				args = append(args, "--clone", ref)
			}
			if lang := paramStr(req.Params, "lang"); lang != "" {
				args = append(args, "--lang", lang)
			}
		case "finetuned":
			// per-machine fine-tuned voice; requires a model + base dir, else defer (never cloud).
			if p.cfg.VoiceGenFTModel == "" || p.cfg.VoiceGenFTBaseDir == "" {
				return p.deferGen(req, meta, start, len(req.Input), "no fine-tuned voice configured")
			}
			meta.Model = "chatterbox-tts-ft"
			args = append(args, "--engine", "finetuned",
				"--model", p.cfg.VoiceGenFTModel, "--base-dir", p.cfg.VoiceGenFTBaseDir)
			ref := paramStr(req.Params, "clone")
			if ref == "" {
				ref = p.cfg.VoiceGenFTRef
			}
			if ref != "" {
				args = append(args, "--clone", ref)
			}
			lang := p.cfg.VoiceGenFTLang
			if l := paramStr(req.Params, "lang"); l != "" {
				lang = l
			}
			if lang != "" {
				args = append(args, "--lang", lang)
			}
			args = appendVoiceRecipe(args, p.cfg)
		default:
			return p.deferGen(req, meta, start, len(req.Input), "unknown voice "+voice)
		}
		// voice path is unchanged re: seed — Chatterbox takes no seed, so no --seed flag.
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
	leaseEnv, releaseLease, lerr := p.acquireMediaLease("audio-gen ("+kind+")", timeout, p.gpuWait())
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer releaseLease()
	// voice never starts ComfyUI → skip the post-run ComfyUI /free (still tree-kills
	// the python worker on timeout). music drives ComfyUI → keep the /free.
	spec := gpugen.Spec{
		Exe:           p.cfg.NodePath,
		Script:        script,
		Args:          args,
		Env:           append(p.genEnv(), leaseEnv...),
		Out:           out,
		Timeout:       timeout,
		SkipFreeComfy: kind == "voice",
	}
	// Passive fleet footprint: acestep for the ComfyUI music worker, chatterbox
	// for the TTS voice paths (incl. finetuned — same engine family).
	audioFamily := "chatterbox"
	if kind == "music" {
		audioFamily = "acestep"
	}
	p.footprintSampling(audioFamily, "", "audio-gen").ApplyTo(&spec)
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

// appendVoiceRecipe appends the per-machine fine-tuned generate() recipe knobs as CLI
// flags, omitting any that are zero (the worker then uses its own default). The worker
// binds them as kwargs — never positionally — because the English and multilingual
// Chatterbox generate() signatures order their params differently.
func appendVoiceRecipe(args []string, cfg config.Config) []string {
	add := func(flag string, v float64) {
		if v > 0 {
			args = append(args, flag, strconv.FormatFloat(v, 'g', -1, 64))
		}
	}
	add("--temperature", cfg.VoiceGenFTTemperature)
	add("--cfg-weight", cfg.VoiceGenFTCFGWeight)
	add("--exaggeration", cfg.VoiceGenFTExaggeration)
	add("--repetition-penalty", cfg.VoiceGenFTRepetitionPenalty)
	return args
}

// genEnv builds the extra env for a GPU-gen child: COMFY_DIR + MEMORY_STACK
// (invariant 1: the CPU-only models freeLlamaSwap must never unload, sourced from
// config rather than a buried const).
//
// It does NOT carry the lease. Callers append the env returned by acquireMediaLease,
// which is the only thing that makes a runner willing to touch the GPU — keeping the
// two separate is what makes "did this call site take a lease?" answerable by reading
// the call site. There is no longer a GPU_LOCK_WAIT_MS: queueing moved to the Go side
// with the acquisition, and the runners never read that variable.
func (p *Pipeline) genEnv() []string {
	env := []string{"COMFY_DIR=" + p.cfg.ComfyDir}
	if len(p.cfg.MemoryStack) > 0 {
		env = append(env, "MEMORY_STACK="+strings.Join(p.cfg.MemoryStack, ","))
	}
	return env
}

// gpuWait is how long a GPU task may queue behind the current holder before deferring.
// One ceiling for every task: per-task overrides bought nothing at 90s and would have
// silently restored the old 20-minute video wait from every existing config file.
// Zero means a single try.
func (p *Pipeline) gpuWait() time.Duration {
	if p.cfg.GPUWaitMs <= 0 {
		return 0
	}
	return time.Duration(p.cfg.GPUWaitMs) * time.Millisecond
}

// lockEnv threads a configured GPU-lock override to a render runner as the
// GPU_LOCK env, so the Go-side vision gate (LO-1) and the Node runners always
// contend on the SAME lock path. Empty when gpu_lock_path is unset — both
// sides then resolve the identical default on their own via gpulease.LeaseDir.
func (p *Pipeline) lockEnv() []string {
	if p.cfg.GPULockPath != "" {
		return []string{"GPU_LOCK=" + p.cfg.GPULockPath}
	}
	return nil
}

// ambientLeaseEnv reports the lease this process is ALREADY running under, if any.
//
// `local-offload gpu reserve --class media -- local-offload …` is a documented
// standalone flow: the reserve verb takes the lease and runs the harness as its CHILD,
// threading GPU_LEASE_DIR/EPOCH/CLASS down. Acquiring again inside that child is a
// self-deadlock — the parent holds the only slot, so the child queues behind itself
// until its window lapses and then defers. Inheriting is also the right semantic: the
// operator asked for ONE reservation to cover the whole command.
//
// (nil, nil) means no ambient lease — acquire normally. A lease that is PRESENT but no
// longer current is an error, not an inheritance: the parent was fenced out, so nothing
// is protecting the card and a silent fresh acquisition would hide that.
func ambientLeaseEnv() ([]string, error) {
	dir := strings.TrimSpace(os.Getenv("GPU_LEASE_DIR"))
	rawEpoch := strings.TrimSpace(os.Getenv("GPU_LEASE_EPOCH"))
	class := strings.TrimSpace(os.Getenv("GPU_LEASE_CLASS"))
	if dir == "" && rawEpoch == "" && class == "" {
		return nil, nil
	}
	if dir == "" || rawEpoch == "" || class == "" {
		return nil, fmt.Errorf("inherited gpu lease is incomplete (GPU_LEASE_DIR=%q EPOCH=%q CLASS=%q); "+
			"refusing to render unarbitrated", dir, rawEpoch, class)
	}
	epoch, perr := strconv.ParseUint(rawEpoch, 10, 64)
	if perr != nil || epoch == 0 {
		return nil, fmt.Errorf("inherited gpu lease epoch %q is not a fencing token; refusing to render unarbitrated", rawEpoch)
	}
	if !gpulease.Class(class).Valid() {
		return nil, fmt.Errorf("inherited gpu lease class %q is unknown; refusing to render unarbitrated", class)
	}
	// The fence, applied at the boundary: an epoch that is no longer current means the
	// card was handed to somebody else while our parent was suspended.
	if info := gpulease.InspectDir(dir); !info.Held || info.Epoch != epoch {
		return nil, fmt.Errorf("inherited gpu lease (epoch %d) is no longer current at %s; "+
			"the card was handed to another holder", epoch, dir)
	}
	return []string{
		"GPU_LEASE_DIR=" + dir,
		"GPU_LEASE_EPOCH=" + rawEpoch,
		"GPU_LEASE_CLASS=" + class,
	}, nil
}

// mediaSlot is the IN-PROCESS half of GPU mutual exclusion: one media job at a time
// inside this process, whatever else is true.
//
// The file claim serializes across processes. An INHERITED lease has no claim to
// contend on — every job under `gpu reserve -- <cmd>` already holds the same one — so
// without this, concurrency inside one process is unarbitrated. That is fine for the
// one-shot command the wrapper was written for and wrong for a long-running server:
// fleet-serve runs Pipeline.Run inline in a net/http handler goroutine, so two
// dispatches under one reservation would both proceed, spawn two ComfyUI instances on
// one card, and race the unload election so one render runs with models still resident
// — precisely the condition the lease exists to prevent. The docs also recommend the
// wrapper form, which steers straight into it.
//
// It is a buffered channel rather than a Mutex for two reasons: a waiter BLOCKS instead
// of polling (no timers, no wakeups, no file reads — it is handed the slot the moment
// the holder releases), and the wait can be bounded, which a Mutex cannot.
//
// LOCK ORDER IS ALWAYS slot -> file lease, never the reverse, so the two cannot deadlock.
var mediaSlot = make(chan struct{}, 1)

// takeMediaSlot claims the in-process slot, waiting at most wait. Reports false on
// timeout, which the caller turns into the same clean defer a busy card produces.
func takeMediaSlot(wait time.Duration) bool {
	select {
	case mediaSlot <- struct{}{}:
		return true // free: no timer allocated at all in the common case
	default:
	}
	if wait <= 0 {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case mediaSlot <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

func releaseMediaSlot() { <-mediaSlot }

// errGPUBusy reports that the card is legitimately held by someone else. Callers turn
// it into a clean defer rather than a failure — the work is not broken, it waited its
// window behind a holder.
//
// detail is set when the holder is another job in THIS process, where there is no lease
// record to describe — the machine-wide Info would name our own lease and read as
// nonsense.
type errGPUBusy struct {
	info   gpulease.Info
	detail string
}

func (e *errGPUBusy) Error() string {
	if e.detail != "" {
		return "gpu busy: " + e.detail
	}
	return fmt.Sprintf("gpu busy: %s holds the lease (%ds, reason %q)",
		e.info.Class, int(e.info.Age/time.Second), e.info.Reason)
}

// IsGPUBusy reports whether err means the card was legitimately held by someone else
// for the whole wait window — the work is intact and was queued, not broken.
//
// It is exported because errGPUBusy is unexported and RunImageBatch returns an error
// rather than a core.Result: with no predicate the CLI turned a busy card into a hard
// failure and exited non-zero, which is precisely what a defer exists to avoid.
func IsGPUBusy(err error) bool {
	var busy *errGPUBusy
	return errors.As(err, &busy)
}

// deferForLease turns a lease-acquisition failure into the same clean defer the render
// runner used to produce on a busy card — but WITHOUT spawning a process that could
// only have queued and timed out. A busy card is not a failure, it is work behind a
// holder; an unusable lease location is a configuration fault and is classed apart so
// the two are distinguishable in the ledger.
func (p *Pipeline) deferForLease(err error, task core.TaskType, meta core.Meta, inputChars int, start time.Time) core.Result {
	meta.LatencyMs = time.Since(start).Milliseconds()
	var busy *errGPUBusy
	if errors.As(err, &busy) {
		meta.ErrClass = "gpu_busy"
	} else {
		meta.ErrClass = "gpu_lease_unavailable"
	}
	p.recordDefer(task, meta, inputChars, err.Error())
	return core.Deferf(err.Error(), "", meta)
}

// acquireMediaLease takes the machine-wide MEDIA lease for one generation job and
// returns the env that hands it down to the render runner, plus a release func.
//
// THIS IS WHERE ARBITRATION MOVED TO. Acquisition used to live only in
// render/gpu-lock.mjs, which meant the Go side could not refuse a job before spawning
// it, and — because two languages each implemented the rules — the two disagreed about
// who owned the card. The Go side now acquires and the runner INHERITS
// (GPU_LEASE_DIR/EPOCH/CLASS), so there is exactly one implementation.
//
// A busy card is QUEUED for up to `wait` and only then returns *errGPUBusy, so the
// caller defers with the holder's detail instead of spawning a process that could only
// have waited and timed out. The heartbeat keeps a long render's lease alive; the
// reclaim rule needs both a stale heartbeat and an expired window, so a missed tick
// inside the declared window is harmless.
func (p *Pipeline) acquireMediaLease(reason string, ttl, wait time.Duration) ([]string, func(), error) {
	noop := func() {}
	start := time.Now()

	// THE IN-PROCESS SLOT COMES FIRST, on both paths. Blocking here is free — no timer
	// in the uncontended case, no polling in the contended one — and it is the only
	// thing arbitrating two jobs that INHERIT the same lease. See mediaSlot.
	if !takeMediaSlot(wait) {
		return nil, noop, &errGPUBusy{detail: fmt.Sprintf(
			"another generation job in this process still holds the card after %s", wait)}
	}
	slotHeld := true
	defer func() {
		// Every failure path below must give the slot back, or the first bad lease
		// wedges every later job in this process.
		if slotHeld {
			releaseMediaSlot()
		}
	}()

	// Already inside someone's lease? Inherit it. Acquiring again would queue behind
	// ourselves until the window lapsed.
	inherited, ierr := ambientLeaseEnv()
	if ierr != nil {
		return nil, noop, ierr
	}
	if inherited != nil {
		// The holder above us owns renewal and release; doing either here would drop a
		// lease that is not ours. The slot is still ours to give back.
		slotHeld = false
		var once sync.Once
		return append(inherited, p.lockEnv()...), func() { once.Do(releaseMediaSlot) }, nil
	}
	m, err := gpulease.OpenAt(p.cfg.GPULockPath, p.cfg.StateDir)
	if err != nil {
		// Refuse rather than run unarbitrated. This is the fail-closed half of the
		// design: an unusable lease location means we cannot promise mutual exclusion,
		// and quietly rendering anyway is exactly the behaviour that let a job tear the
		// text tier down. The message names the fix (state_dir).
		return nil, noop, err
	}
	// Spend only what is LEFT of the window on the machine-wide lease, so the two waits
	// compose into the one budget the caller asked for rather than doubling it.
	remaining := wait - time.Since(start)
	if remaining < 0 {
		remaining = 0
	}
	lease, err := m.Acquire(gpulease.ClassMedia, gpulease.Options{
		Reason: reason, Origin: "pipeline", TTL: ttl, Wait: remaining,
	})
	if err != nil {
		var held *gpulease.ErrHeld
		if errors.As(err, &held) {
			return nil, noop, &errGPUBusy{info: held.Info}
		}
		return nil, noop, err
	}

	// The heartbeat exists only while a render is actually running, and one media job
	// runs at a time per process, so this is at most ONE 15s timer for the duration of
	// GPU work that lasts minutes. Nothing ticks while the harness is idle.
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_ = lease.Renew()
			}
		}
	}()
	var once sync.Once
	release := func() {
		once.Do(func() {
			close(stop)
			// JOIN, do not just signal. Release mutates the Lease that a Renew() already
			// in flight is reading, and it would also let that Renew re-create the
			// heartbeat file after the release swept it. Waiting costs microseconds.
			<-stopped
			_ = lease.Release()
			releaseMediaSlot()
		})
	}
	slotHeld = false
	env := []string{
		"GPU_LEASE_DIR=" + lease.Dir(),
		"GPU_LEASE_EPOCH=" + strconv.FormatUint(lease.Epoch(), 10),
		"GPU_LEASE_CLASS=" + string(lease.Class()),
	}
	return append(env, p.lockEnv()...), release, nil
}

// footprintsPath resolves the shared fleet footprint store path: a
// "footprints.json" sibling of the ledger (else cache) file — the same
// ~/.local-offload base the default config resolves those to, and
// automatically isolated in tests that point them at temp dirs. Falls back to
// ~/.local-offload/footprints.json when both are opted out; "" = no store
// (sampling stays off).
func (p *Pipeline) footprintsPath() string {
	for _, anchor := range []string{p.cfg.LedgerPath, p.cfg.CachePath} {
		if anchor != "" {
			return filepath.Join(filepath.Dir(anchor), "footprints.json")
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local-offload", "footprints.json")
	}
	return ""
}

// FootprintStore returns the lazily-opened shared footprint store (nil when no
// path resolves). Exported for the fleet-serve/fleet-measure verbs: health
// advertises its Entries(), fleet-measure prints its on-disk records.
func (p *Pipeline) FootprintStore() *fleetnode.Footprints {
	p.footOnce.Do(func() {
		if path := p.footprintsPath(); path != "" {
			p.foot = fleetnode.OpenFootprints(path)
		}
	})
	return p.foot
}

// footprintSampling composes the passive per-render VRAM sampling hook for one
// GPU render: the footprint key from THIS machine's bindings, the sampler
// cfg.FleetSampler selects, and a record-into-the-shared-store callback
// (gpugen fires it on SUCCESS only, peak > 0 only). nil when no store
// resolves — gpugen then keeps its legacy exec path byte-identical.
func (p *Pipeline) footprintSampling(family, quant, task string) *gpugen.Sampling {
	store := p.FootprintStore()
	if store == nil {
		return nil
	}
	return &gpugen.Sampling{
		Footprint:   &gpugen.FootprintKey{Family: family, Quant: quant, Task: task},
		SampleFunc:  p.footprintSampleFunc(),
		OnFootprint: func(peakGiB float64) { store.Record(family, quant, task, peakGiB) },
	}
}

// footprintSampleFunc selects the per-render VRAM source per cfg.FleetSampler:
// "pdh-shared" (J3, UMA iGPUs — the amd-rdna3 seed sets it) → the PDH tree
// summing Dedicated+Shared, because on unified memory allocations land in
// SHARED and Dedicated reads ~0 (footprints would silently never record);
// Windows + not-"global" → the PDH Dedicated tree (measures OUR job's cost,
// uncontaminated by the desktop/other apps); otherwise an nvidia-smi
// global-delta closure. p.fleetSample overrides in tests.
func (p *Pipeline) footprintSampleFunc() func(childPid int) (float64, error) {
	if p.fleetSample != nil {
		return p.fleetSample
	}
	if runtime.GOOS == "windows" && p.cfg.FleetSampler == "pdh-shared" {
		return fleetnode.TreeDedicatedPlusSharedGiB
	}
	if runtime.GOOS == "windows" && p.cfg.FleetSampler != "global" {
		return fleetnode.TreeDedicatedGiB
	}
	return globalDeltaSampleFunc(runNvidiaSmiMemory)
}

// globalDeltaSampleFunc builds the fallback sampler: global VRAM used minus a
// baseline captured by the closure on its first call — which gpugen makes
// immediately at child start, before the render loads anything. Called only
// from gpugen's single sampler goroutine, so the baseline needs no lock.
func globalDeltaSampleFunc(run func() (string, error)) func(childPid int) (float64, error) {
	baseline := -1.0
	return func(int) (float64, error) {
		out, err := run()
		if err != nil {
			return 0, err
		}
		_, used, err := fleetnode.ParseSmiMemory(out)
		if err != nil {
			return 0, err
		}
		if baseline < 0 {
			baseline = used
		}
		d := used - baseline
		if d < 0 {
			d = 0
		}
		return d, nil
	}
}

// runNvidiaSmiMemory shells the global VRAM query the global-delta sampler
// parses (the same query fleet-serve's 2s health sampler runs).
func runNvidiaSmiMemory() (string, error) {
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.total,memory.used", "--format=csv,noheader,nounits").Output()
	return string(out), err
}

// imageModelFromConfig is the ONE mapping from config to the render-model
// binding, shared by the single and batch paths. It exists because the two
// paths each carried their own Model literal and the second silently dropped
// five newly added fields (a lightning4-bound seat rendered the 50-step full
// recipe in batch, review-caught pre-merge 2026-08-10) — a drift class this
// helper deletes. TestImageModelFromConfig reflect-checks that every field
// added to imagegen.Model gets mapped here.

func imageModelFromConfig(cfg config.Config) imagegen.Model {
	return imagegen.Model{
		Ckpt:         cfg.ImageGenCkpt,
		VAE:          cfg.ImageGenVAE,
		Steps:        cfg.ImageGenSteps,
		CFG:          cfg.ImageGenCFG,
		Sampler:      cfg.ImageGenSampler,
		Scheduler:    cfg.ImageGenScheduler,
		Family:       cfg.ImageGenFamily,
		Preset:       cfg.ImageGenPreset,
		CLIP:         cfg.ImageGenCLIP,
		LoRA:         cfg.ImageGenLoRA,
		LoRAStrength: cfg.ImageGenLoRAStrength,
		Shift:        cfg.ImageGenShift,
		PoolVvramGB:  cfg.ImageGenPoolVvramGB,
		PoolCompute:  cfg.ImageGenPoolCompute,
		PoolDonor:    cfg.ImageGenPoolDonor,
	}
}

// imageFootprintKey is this box's image-render footprint identity: the
// configured imagegen_family (else the script's SDXL default), with quant
// "bf16" only for the HiDream-O1 checkpoint binding (the bf16 recipe) — every
// other binding is "node default" per the contract.
func imageFootprintKey(cfg config.Config) (family, quant string) {
	family = cfg.ImageGenFamily
	// J2 sdcpp engine: family still comes from imagegen_family (per-machine truth);
	// with no family set, "sdcpp" keeps its ledger tier distinct from the ComfyUI
	// SDXL default. Quant is read from the bound model's filename (GGUF quants
	// encode it: ...-Q8_0.gguf / Q4_K...), else node default.
	if cfg.ImageGenEngine == "sdcpp" {
		if family == "" {
			family = "sdcpp"
		}
		// Basename only (a "models-bf16" DIR must not hit) and longest-token-first
		// (BF16 before F16, Q4_K_M before Q4_K) so subsets never shadow supersets.
		up := strings.ToUpper(filepath.Base(cfg.SdcppModel))
		for _, q := range []string{"Q8_0", "Q6_K", "Q5_K_M", "Q5_K_S", "Q5_K", "Q5_1", "Q4_K_M", "Q4_K_S", "Q4_K", "Q4_1", "Q4_0", "Q3_K", "Q2_K", "BF16", "F16"} {
			if strings.Contains(up, q) {
				quant = strings.ToLower(q)
				break
			}
		}
		return family, quant
	}
	if family == "" {
		family = "sdxl"
	}
	if strings.HasPrefix(family, "hidream-o1") {
		quant = "bf16"
	}
	return family, quant
}

// videoFamilyWanSentinel is the config value that means "the runner's own default
// family" rather than a distinct graph. The runner spells that family "wan" on its
// command line while config spells it "wan22"; both denote Wan 2.2, so provenance
// canonicalises to the config spelling and never records two keys for one family.
const videoFamilyWanSentinel = "wan22"

// resolveVideoFamily is the SINGLE source of truth for "which video family does
// this request render with". It returns the runner argument (argModel — "" means
// pass no --model and let the runner apply its own default) and the canonical
// family for provenance (renderFamily — "" only when nothing is bound anywhere).
//
// Precedence, unchanged from the router it replaces: an explicit per-request
// `model` wins; else the machine's configured videogen_family; else the runner
// default. It exists because 0.73.0 derived the ledger label from config while
// the runner arg was derived here, and the two disagreed on exactly the requests
// that used the documented override.
func resolveVideoFamily(cfg config.Config, reqModel string) (argModel, renderFamily string) {
	req := strings.TrimSpace(reqModel)
	if req != "" {
		// An explicit request is passed through verbatim (the runner owns the
		// arg namespace, including values this function has never heard of), but
		// provenance is canonicalised.
		return req, canonicalVideoFamily(req)
	}
	if fam := strings.TrimSpace(cfg.VideoGenFamily); fam != "" {
		if fam == videoFamilyWanSentinel {
			// Bound to the runner default: pass no arg (byte-identical to the
			// pre-0.73.1 behavior) but the render IS Wan, so say so.
			return "", videoFamilyWanSentinel
		}
		return fam, canonicalVideoFamily(fam)
	}
	// Nothing bound: the runner picks its own default. renderFamily stays ""
	// so the ledger keeps the historical "comfyui-video" label — changing it on
	// every unbound box would fragment existing health tiers, which is the one
	// thing this label is documented not to do.
	return "", ""
}

// videoRunnerFamilies is the CLOSED set the render runner dispatches on, and it is
// matched EXACTLY and CASE-SENSITIVELY because that is what the runner does:
// render/comfy-video.mjs tests `flags.model === "ace"`, `model === "ltx25"` and
// `model === "hunyuan"`, then falls through to the Wan builder for everything else
// — silently, with Wan's own resolution and frame defaults. "wan" is absent from
// this map on purpose: it takes the same fallthrough as any unrecognized value.
// TestVideoRunnerFamiliesMatchTheRunner pins the set against the runner's own
// dispatch literals so a family added there cannot drift away from this resolver.
var videoRunnerFamilies = map[string]string{
	"ltx25":   "ltx25",
	"hunyuan": "hunyuan",
	"ace":     "ace",
}

// canonicalVideoFamily maps a runner argument onto the config family namespace, so
// one family never keys two tiers AND provenance never names a family that did not
// render.
//
// The unrecognized case is the load-bearing one. An earlier revision passed unknown
// strings through verbatim, reasoning that "the runner owns the arg namespace" — but
// the runner's namespace is CLOSED, so `model:"LTX25"` (wrong case) or a typo like
// "ltx2.5" renders WAN while the ledger row claimed `comfyui-video:LTX25` and the
// footprint store gained a key holding Wan's VRAM profile. That is exactly the
// false-provenance class this change exists to end, re-created one layer out.
//
// Matching is case-SENSITIVE for the same reason: "LTX25" does not match the
// runner's `=== "ltx25"`, so it renders Wan, so it must be RECORDED as Wan.
// Lower-casing here would report ltx25 for a Wan render — the same lie in a
// friendlier shape. Unrecognized input records the family that WILL run.
func canonicalVideoFamily(fam string) string {
	if canon, ok := videoRunnerFamilies[fam]; ok {
		return canon
	}
	return videoFamilyWanSentinel
}

// videoFootprintWanFamily is the Wan family's spelling in the FOOTPRINT-STORE
// namespace, which is not the config namespace: the store has always keyed Wan as
// "wan2.2" (with the dot) and `fleetnode.familyFor` advertises that same string on
// /fleet/health. They must intersect exactly — an earlier revision of this fix
// keyed the writer as "wan22" while the advertiser still said "wan2.2", which
// split the namespace FLEET-WIDE and orphaned the store's accumulated Wan history
// on the happy path. Keeping the store's own spelling preserves that history and
// keeps writer and advertiser on one key.
const videoFootprintWanFamily = "wan2.2"

// videoFootprintFamily is the footprint-store key for a render family. The store
// is keyed family+quant+task and read by the fleet for placement, so it must name
// the family that ran. "" (nothing bound) and the wan22 sentinel both mean Wan —
// an unbound box still renders Wan — and both map to the store's spelling.
func videoFootprintFamily(renderFamily string) string {
	if renderFamily == "" || renderFamily == videoFamilyWanSentinel {
		return videoFootprintWanFamily
	}
	return renderFamily
}

// videoFootprintQuant reports "q8_0" when this box's bound Wan expert weights
// are the Q8_0 GGUFs, else "" (node default — fp8_scaled/fp16 bindings and the
// script's own defaults).
//
// It is scoped to the Wan family ON PURPOSE. The Wan GGUF keys stay bound on a
// box whose seat is another family (they are the recorded fallback), so before
// 0.73.1 this returned "q8_0" for LTX-2.5 renders whose transformer is
// int8-convrot — a quant the render never used, stamped on a store the fleet
// reads. Other families carry their quant in their own weight filenames; until a
// family declares one, "" (unknown) is the honest answer.
func videoFootprintQuant(cfg config.Config, renderFamily string) string {
	// Gate on the CONFIG-namespace family, not on videoFootprintFamily's return:
	// that helper now yields "wan2.2", so comparing it to the "wan22" sentinel
	// would be false for every input and silently suppress the quant everywhere.
	if renderFamily != "" && renderFamily != videoFamilyWanSentinel {
		return ""
	}
	if strings.Contains(strings.ToUpper(cfg.VideoGenUnetHigh+cfg.VideoGenUnetLow), "Q8_0") {
		return "q8_0"
	}
	return ""
}

// videoModelLabel is the ledger's model_tier for a video render: the family that
// ACTUALLY rendered, resolved by resolveVideoFamily. An unbound family keeps the
// historical "comfyui-video" label so health tiers don't fragment.
//
// Recording the family is what makes a family-binding verdict provable from
// telemetry. The ledger previously recorded only THAT a video render happened, so
// after the 2026-08-12 ltx25 seat binding the record could not distinguish an
// ltx25 render from a wan22 one — and the binding's status was still being
// questioned six days later while config, code and binary all carried it.
//
// It takes the RESOLVED family, not the config, because taking the config was the
// 0.73.0 defect: a per-request override changed what rendered without changing
// what was recorded. Do not reintroduce a cfg parameter here.
func videoModelLabel(renderFamily string) string {
	if renderFamily == "" {
		return "comfyui-video"
	}
	return "comfyui-video:" + renderFamily
}

// runGraphFootprintFamily is the run-graph footprint family: payload-declared
// model_family when the caller supplied one (the fleet dispatch path), else
// the generic "comfy-graph".
func runGraphFootprintFamily(params map[string]any) string {
	if fam := paramStr(params, "model_family"); fam != "" {
		return fam
	}
	return "comfy-graph"
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

// sttRoute picks the STT model and protocol for a transcribe request. Pure — the
// selection logic is the feature's actual switch, so it is unit-tested directly
// (the pipeline holds a concrete client, so routing is not stubbable above it).
// The HQ upstream may speak the OpenAI /v1/audio/transcriptions protocol instead of
// whisper-server's /inference (llama-server mtmd STT, e.g. Qwen3-ASR): binding such a
// model without stt_hq_api="openai" 404'd the whisper endpoint (live finding 2026-07-21).
func sttRoute(cfg config.Config, hq bool) (model string, useOAI bool) {
	model = cfg.STTModel
	if hq && cfg.STTModelHQ != "" {
		model = cfg.STTModelHQ
		useOAI = strings.EqualFold(cfg.STTHQAPI, "openai")
	}
	return model, useOAI
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
//
// entryChars is the ENTRY view's input length: req.Input may hold a TO-3
// repacked tier view, but input_chars is a trained confhead feature (loginput)
// whose label stream is entry-scale, so every recorded row keeps the entry
// semantics (round-1 review finding 2026-08-14).
func (p *Pipeline) attempt(ctx context.Context, req core.Request, built tasks.Built, ck, model string, meta core.Meta, start time.Time, record bool, entryChars int) (core.Result, bool) {
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
				meta.EscSource = core.EscSchema
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
					meta.EscSource = core.EscGrounding
				}
			}
			if v.OK {
				reason, margin, src, low := p.confidenceGate(req, data, gen.Logprobs)
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
						// entryChars, NOT len(req.Input): the head's loginput
						// feature is trained on entry-scale label rows, and a
						// repacked mid-chain view would be served
						// out-of-distribution (round-1 review finding).
						e := entryFrom(req.Task, meta, false, entryChars)
						pc := chHead.Predict(string(req.Task), confhead.FeatureRow(e))
						if pc >= 0 && pc < tau {
							low = true
							reason = fmt.Sprintf("low confhead p(correct)=%.3f < threshold %.3f", pc, tau)
							src = core.EscConfhead
						}
					}
				}
				if low {
					meta.LatencyMs = time.Since(start).Milliseconds()
					meta.EscSource = src
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
					if b, e := json.Marshal(cacheVal{Data: data, TokensIn: gen.TokensIn, Model: meta.Model, InLoop: p.tierCache}); e == nil {
						_ = p.cache.Put(ck, b)
					}
				}
				p.record(req.Task, meta, entryChars)
				// Phase A.3: sampled shadow-queue capture (non-escalated classify/triage/extract; config-gated, off by default).
				p.captureShadow(req, entryFrom(req.Task, meta, false, entryChars), core.Result{OK: true, Data: data, Meta: meta})
				// Phase 6: harvest a verified-good (input, output) exemplar for the sidecar.
				// ENTRY rows only (TierPack empty): a climbed tier's req.Input may be
				// the unbounded original (sidecar bloat) or a cut carrying
				// repackMarker, which future prompts would re-inject as few-shot
				// CONTENT (round-1 review finding).
				if p.cfg.ExemplarsDir != "" && goodExemplar(meta) && meta.TierPack == "" {
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
		// Schema/grounding already stamped their own source above; anything
		// still unattributed here failed parse/verify.
		if meta.EscSource == core.EscNone {
			meta.EscSource = core.EscVerifier
		}
		// A terminal failure (e.g. truncation — input too large for ANY local
		// tier) defers straight to Opus; escalating would just burn the slow 26B.
		return core.Deferf(v.Reason, gen.Content, meta), !v.Terminal
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	meta.EscSource = core.EscRetries
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
func (p *Pipeline) attemptReasoning(ctx context.Context, req core.Request, built tasks.Built, ck string, meta core.Meta, start time.Time, entryChars int) (core.Result, bool) {
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
		if b, e := json.Marshal(cacheVal{Data: data, TokensIn: gen.TokensIn, Model: meta.Model, InLoop: p.tierCache}); e == nil {
			_ = p.cache.Put(ck, b)
		}
	}
	p.record(req.Task, meta, entryChars) // input_chars keeps ENTRY semantics (see attempt)
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
		EscSource: string(meta.EscSource),
		TierPack:  meta.TierPack,
		// Call identity (Phase 0.1) — hashes only, never content.
		InputSHA256:        meta.InputSHA256,
		PromptPrefixSHA256: meta.PromptPrefixSHA256,
		ContextHash:        meta.ContextHash,
		ExemplarIDs:        meta.ExemplarIDs,
		CacheBypass:        meta.CacheBypass,
		CacheHitInLoop:     meta.CacheHitInLoop,
		PrefillSteps:       meta.PrefillSteps,
		AgentProfile:       meta.AgentProfile,
		PrefillTokens:      meta.PrefillTokens,
		CacheTokens:        meta.CacheTokens,
		PrefillMS:          meta.PrefillMS,
		// Same read the delegation log does (delegate.record): per-row, so a
		// long-lived process whose environment never changes still labels
		// every row consistently, and an untagged process writes nothing.
		Arm: strings.TrimSpace(os.Getenv("OFFLOAD_DELEGATE_ARM")),
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
// constant as fallback. Returns (reason, margin, source, escalate?).
//
// The SOURCE is returned beside the human-readable reason because the two serve
// different readers: the reason is for a person reading one deferred row, the
// source is a closed enum the ledger can group by. Without it, "was this the
// model's self-report or a structural signal?" is unanswerable in aggregate —
// which is exactly the gap measured on 2026-08-11.
func (p *Pipeline) confidenceGate(req core.Request, data []byte, lps []llamaclient.TokenLogprob) (string, float64, core.EscalationSource, bool) {
	var margin float64
	switch req.Task {
	case core.TaskClassify:
		if labels := labelClasses(req.Params); len(labels) >= 2 {
			if m, ok := confidence.Margin(lps, "label", labels); ok {
				margin = m
			}
		}
		if conf, low := lowConfidence(data, p.cfg.ClassifyMinConfidence); low {
			return fmt.Sprintf("low confidence %.2f", conf), margin, core.EscSelfConfidence, true
		}
		if t := p.marginThreshold(req.Task); t > 0 && margin > 0 && margin < t {
			return fmt.Sprintf("low decision margin %.2f<%.2f", margin, t), margin, core.EscMargin, true
		}
	case core.TaskTriage:
		if m, ok := confidence.Margin(lps, "decision", []string{"yes", "no", "unsure"}); ok {
			margin = m
		}
		if t := p.marginThreshold(req.Task); t > 0 && margin > 0 && margin < t {
			return fmt.Sprintf("low decision margin %.2f<%.2f", margin, t), margin, core.EscMargin, true
		}
	}
	return "", margin, core.EscNone, false
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
		// COUNT WHAT IS DISCARDED. answersAgree returns ok=false for two very different
		// reasons -- a task it does not judge (anything but classify/triage), and a
		// candidate it could not parse -- and both used to vanish here without trace.
		//
		// That silence is not neutral: an unparseable candidate is disproportionately an
		// EXTREME disagreement (a truncated or malformed answer), so dropping it quietly
		// biases the measured agreement rate UPWARD, in the direction that argues against
		// acting. A rate whose losses are uncountable cannot state its own coverage, and
		// this file's whole job is to produce a rate somebody will make a decision from.
		// SPLIT, because the two causes support different conclusions. A task answersAgree
		// does not judge (anything but classify/triage) is a STRUCTURAL exclusion with no
		// bias implication -- it never belonged to this population. An unparseable candidate
		// IS a bias term: it is disproportionately an extreme disagreement, so losing it
		// pulls the published rate upward. Counting both in one number would let a box whose
		// escalations are mostly summarize report "92% dropped, the rate is badly biased"
		// when zero judgeable candidates were lost and coverage is in fact 100%.
		if task == core.TaskClassify || task == core.TaskTriage {
			p.labelDropsUnparseable.Add(1)
			// PERSISTED, because the consumer is a different process. loupe reads the
			// sidecar from disk; an in-memory counter on a serving Pipeline could only ever
			// describe that process's uptime against a file spanning weeks -- and
			// NewInLoopPipeline is a second instance writing the same file. Best-effort:
			// telemetry must never fail the work it describes.
			appendDropCounter(p.cfg.ConfHeadLabelsPath, p.labelDropsUnparseable.Load())
		} else {
			p.labelDropsUnjudgeableTask.Add(1)
		}
		return
	}
	entry.Grounded = nil
	entry.EscalatedAgreed = &agreed
	entry.LabelSource = ledger.LabelSourceLiveEscalation
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
	// T2-D / T2-A: the key is built by cacheKeyForTier, which WRAPS the same
	// cacheKeyFor constructor Run uses inside RunTier's own keyspace — so the
	// ingredient list has one source and cannot drift, while the two paths can
	// never collide. Two corrections over the former hand-rolled cache.Key call:
	//
	//  1. It keys on the ACTUAL TIER (model), not p.cfg.Model. RunTier pins one
	//     named tier, so its answer belongs to that tier; keying on the primary
	//     model would let two different tiers share one entry — harmless while
	//     nothing read the key, fatal the moment anything did.
	//  2. It carries the template tag, so editing a task's prompt invalidates
	//     these entries exactly as it does the cascade's. The old call here had
	//     the pre-fix shape and was simply never exercised; reviving it as-is
	//     would have reinstated the stale-prompt bug on a brand-new path.
	//
	// ...and it lives in RunTier's OWN keyspace, so Run and RunTier can never
	// compute the same key and overwrite each other's entries. See
	// tierKeyspaceTag for why guarding only the read was not enough.
	ck := cacheKeyForTier(req.Task, req.Input, tasks.StableParamsKey(req.Params), model, built)
	meta := core.Meta{Model: model, Feat: feat}
	meta.InputSHA256 = inputFingerprint(req.Input)
	meta.PromptPrefixSHA256 = promptPrefixFingerprint(built.System, userPreambleOf(built.User, req.Input))
	meta.ContextHash = p.contextHash()

	// Cache participation is a property of THIS pipeline, never of RunTier itself.
	// The shadow-labelling flywheel calls RunTier on the MAIN pipeline (cache
	// open) to evaluate counterfactual tiers; serving those from cache — or
	// writing them — would corrupt the very measurement it exists to produce. Only
	// NewInLoopPipeline sets tierCache.
	useCache := p.tierCache && p.cache != nil
	if useCache {
		if raw, ok := p.cache.Get(ck); ok {
			var cv cacheVal
			// cv.Model == model is DEFENCE IN DEPTH and is unreachable by
			// construction today — say so rather than letting the comment assert a
			// live hazard that no longer exists.
			//
			// It was load-bearing when Run and RunTier shared a keyspace: a RunTier
			// call pinned to the workhorse was served whatever tier the cascade had
			// answered with (measured: the E2B triage tier), while meta.Model
			// reported the workhorse that never ran. Guarding the read fixed the
			// wrong answer but left both paths WRITING that key, so the real fix was
			// cacheKeyForTier's separate keyspace — and `model` is already an
			// ingredient of it, so nothing but a same-tier RunTier can reach here.
			//
			// Kept because it costs one comparison and fails CLOSED. Note a pre-0.63
			// entry cannot reach it either: tierKeyspaceTag is the first ingredient
			// of this key, so an old entry misses at the LOOKUP, not at this guard.
			// The only thing it can still catch is a hand-crafted or externally
			// written entry.
			if json.Unmarshal(raw, &cv) == nil && len(cv.Data) > 0 && cv.Model == model {
				meta.CacheHit = true
				meta.CacheHitInLoop = cv.InLoop
				meta.TokensIn = cv.TokensIn
				meta.LatencyMs = time.Since(start).Milliseconds()
				return core.Result{OK: true, Data: cv.Data, Meta: meta}, true
			}
		}
	}

	res, _ := p.attempt(ctx, req, built, ck, model, meta, start, false /* record=false: ledger/shadow/exemplars stay untouched */, len(req.Input))
	// escalatable ignored: RunTier never escalates.
	//
	// The Put lives here rather than inside attempt because `record` means "write
	// the SAVINGS-ACCOUNTING side-effects" (ledger, shadow queue, exemplar
	// harvest) and must stay false here — that is invariant (a) in
	// NewInLoopPipeline's doc. Caching is invariant (b), and only these two lines
	// grant it. A defer never reaches this point (res.OK is false), so a
	// low-confidence or ungrounded answer is never cached.
	if useCache && res.OK && len(res.Data) > 0 {
		// InLoop stamps the provenance: only NewInLoopPipeline sets tierCache, so
		// every entry written here came from the agent loop talking to itself,
		// whose generation was never costed in the savings ledger.
		if b, e := json.Marshal(cacheVal{Data: res.Data, TokensIn: res.Meta.TokensIn, Model: model, InLoop: true}); e == nil {
			_ = p.cache.Put(ck, b)
		}
	}
	return res, res.OK
}

// appendDropCounter writes the running unparseable-drop count beside the label sidecar so a
// SEPARATE process (loupe) can read it.
//
// Whole-file rewrite of a tiny integer rather than an append: the value is a running total,
// so an append-only log would need summing and would grow without bound for one number.
// Best-effort throughout -- a telemetry write must never fail the work it describes, which
// is the same posture pipeline.record takes.
func appendDropCounter(labelsPath string, n int64) {
	if labelsPath == "" {
		return
	}
	_ = os.WriteFile(labelsPath+".drops", []byte(strconv.FormatInt(n, 10)), 0o644)
}
