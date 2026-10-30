// Package pipeline orchestrates one offload request end to end:
// trivial-check -> context-budget trim -> cache -> build -> generate(grammar)
// -> parse -> verify -> validate -> (retry|defer|accept) -> cache + ledger.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmmdea/offload-harness/internal/breaker"
	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/comfyinst"
	"github.com/dmmdea/offload-harness/internal/confhead"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/contextbudget"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/embedmemo"
	"github.com/dmmdea/offload-harness/internal/exemplars"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/forecastclient"
	"github.com/dmmdea/offload-harness/internal/gpualloc"
	"github.com/dmmdea/offload-harness/internal/gpulock"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/judge"
	"github.com/dmmdea/offload-harness/internal/knn"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/mediahash"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	"github.com/dmmdea/offload-harness/internal/placement"
	"github.com/dmmdea/offload-harness/internal/router"
	"github.com/dmmdea/offload-harness/internal/seatguard"
	"github.com/dmmdea/offload-harness/internal/sttclient"
	"github.com/dmmdea/offload-harness/internal/tasks"
)

type tierOverrides struct {
	TierTimeoutsMs map[string]int `json:"tier_timeouts_ms"`
	Degraded       []string       `json:"degraded"`
}

type Pipeline struct {
	// Media admission (plan P13, mediaadmit.go). alloc is the seam for the live reads behind the
	// card allocator (the zero value reads the real machine); instanceAPI names the endpoint of the
	// ComfyUI instance bound to a card (nil = the base port plus the card's index); stopKept stops
	// the ComfyUI instances kept under a lease when it is released (nil = internal/comfyinst).
	alloc       gpualloc.Deps
	instanceAPI func(gpuprobe.Card) string
	stopKept    func(ctx context.Context, comfyDir string, epoch uint64) []comfyinst.Outcome

	// clearReads caches, for a few seconds, what the lane probe read of llama-swap's roster and /running to decide whether a
	// render defers its host RAM until its own unload has run (mediahostram.go). The grant never reads it.
	clearMu    sync.Mutex
	clearReads map[string]mediaClearRead

	// afterClaimsRead is a test seam, nil in production. A media call that NAMED its cards (a pin, a
	// pool, a declared device) runs it once it has read what is claimed and before it makes its gated
	// claim (acquireCards). That call reads no card table, so the allocator's input never runs for it
	// and the allocator's last read of live state (alloc.HostMemory) cannot mark the window between its read and its claim.
	afterClaimsRead func()

	// seatRatesPath is the per-seat rate store under the state root, resolved
	// by seatRates() on each agent run (empty = no usable root).
	seatRatesPath string
	cfg           config.Config
	client        *llamaclient.Client
	stt           *sttclient.Client      // whisper-server transcribe client (audio never hits the text cascade)
	forecast      *forecastclient.Client // Timestral forecast client (a JSON payload never hits the text cascade)
	cache         *cache.Cache           // may be nil
	led           *ledger.Ledger         // may be nil
	tracker       CallTracker            // nil = no PAIR running cards
	pair          *pairworkloads.Emitter // nil = remote calls open no PAIR card (remoteattr.go)
	thresholds    map[string]float64     // per-task conformal margin thresholds (Phase 2); nil = config constant
	breakers      *breaker.Group         // per-tier circuit breakers (Phase 3)
	router        *router.Model          // entry-tier router (Phase 5); nil = static rule
	overrides     *tierOverrides         // health-driven per-tier timeouts/degraded (Phase 4); nil = none
	healMu        sync.Mutex             // Phase 7 autoheal rate-limit
	lastHeal      map[string]time.Time
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
	// D-129: per-name memo of "is this seat served by vLLM?" and a once-per-name
	// record of a roster probe that could not answer. Guarded by vllmSeatMu;
	// both maps are lazily built, so the zero Pipeline answers "no" for every
	// name without allocating. See vllmseat.go.
	vllmSeatMu     sync.Mutex
	vllmSeatCache  map[string]vllmSeatAnswer
	vllmSeatWarned map[string]bool
	// Composite tier (ADR 0052): placementLive is the seam tests use to inject
	// the machine's live readers (occupancy, free VRAM, host RAM, presence);
	// nil = the PROCESS-WIDE memoised Snapshot over cfg
	// (placement.SharedSnapshot: one 2 s reading serves every decision in
	// that window across every pipeline in the process — the in-loop offload
	// builds one Pipeline per contract, and a spread must not exec nvidia-smi
	// per subtask per tick). Read only through live(); consulted only when
	// cfg.Composite().
	placementLive func() placement.Live
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
	// visionColdLoadWait (F49) bounds the wait for a COLD vision seat to load before the
	// request is sent (cfg.VisionColdLoadWaitSec, floored at one admission poll). Zero turns
	// the wait off. Resolved once in New; a field so tests can shrink it.
	visionColdLoadWait time.Duration
	// Passive fleet footprint recording (docs/FLEET-NODE.md): every GPU render
	// carries a gpugen sampling hook keyed by this machine's bindings, so
	// measured VRAM peaks accumulate during NORMAL harness use, not just fleet
	// jobs. footOnce lazily opens the shared store (a footprints.json sibling
	// of the ledger/cache files); fleetSample overrides the composed sampler in
	// tests (nil = select per cfg.FleetSampler).
	footOnce    sync.Once
	foot        *fleetnode.Footprints
	fleetSample func(childPid int) (float64, error)
	// hostSample overrides the host-memory sampler of the same hook in tests (nil = gpulease.TreeMemory:
	// the private and the resident memory of the render's process tree).
	hostSample func(childPid int) (privateGiB, residentGiB float64, err error)
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
	// Cascade seat guard (seatguard.go): seatGuard is the process-wide guard
	// for this box (nil = inert: no vllm_seats, or cascade_seat_guard off),
	// seatVerdictFn the test seam that replaces it.
	seatGuard     *seatguard.Guard
	seatVerdictFn func(ctx context.Context, model string) seatguard.Verdict
	// laneFaults keeps the media lane probe's card-table fault lines to one per probe and cause per minute (laneCardTable).
	laneFaults laneFaultLog
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
	// A held card is waited for gpu_wait_ms, like every other GPU door, and the HTTP timeout
	// only caps that: the lease wait used to BE stt_request_timeout_sec (1,800 s), which is
	// where the MCP client aborts an idle call (register C-89).
	p.stt = sttclient.New(cfg.Endpoint, time.Duration(cfg.STTRequestTimeoutSec)*time.Second).WithFenceWait(p.gpuWait())
	p.forecast = forecastclient.New(cfg.Endpoint, time.Duration(cfg.ForecastRequestTimeoutSec)*time.Second).WithFenceWait(p.gpuWait())
	// LO-1: resolve the shared GPU lock path ONCE, the same way the Node render
	// runners do, so the vision gate watches the exact lock the gen jobs hold.
	p.gpuLockPath = gpulock.Path(cfg.GPULockPath, cfg.StateDir)
	p.visionGPUWait = time.Duration(cfg.VisionGPUWaitSec) * time.Second
	p.visionGPUPoll = 2 * time.Second
	p.visionRetryWait = 3 * time.Second
	if sec := cfg.VisionColdLoadWaitSec; sec > 0 {
		// A wait shorter than warmSeat's one-poll floor would be read as "no budget left" and
		// skip the warm-up silently; the floor makes a small setting mean the shortest real wait.
		p.visionColdLoadWait = max(time.Duration(sec)*time.Second, admissionPoll)
	}
	p.thresholds = loadThresholds(cfg.ThresholdsPath)    // Phase 2
	p.router = router.Load(cfg.RouterWeightsPath)        // Phase 5
	p.overrides = loadOverrides(cfg.TierOverridesPath)   // Phase 4
	p.breakers = breaker.NewGroup(5, 10, 20*time.Second) // Phase 3: 5 infra-fails / 10-window, 20s cooldown
	// Cascade seat guard: nil (inert) unless this box declares a vLLM seat and
	// the guard is on; process-wide, so every pipeline shares one reading.
	p.seatGuard = seatguard.Shared(cfg)
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
// CallTracker opens a PAIR Jobs card when a long call starts and closes it
// when the call ends (pairworkloads.Emitter.Begin). working turns the card
// running once the lane holds its engine (core.MarkWorking); end closes it
// with the call's Result (a success completes it, a call held back by another
// job's hold on the card closes it quiet, anything else fails it with the
// reason); both are nil when nothing was opened.
//
// callID is the id of the card Begin opened ("" when none was): Run stamps it on
// the call's Meta, the ledger row carries it, and the row closes exactly that
// card (pairworkloads.Emitter.claim) instead of the oldest open card of the task.
type CallTracker interface {
	Begin(task, door string) (callID string, working func(), end func(res core.Result))
}

// SetCallTracker wires the tracker Run reports call starts to; nil = none.
func (p *Pipeline) SetCallTracker(t CallTracker) { p.tracker = t }

// closeCall closes a tracked call's card with the call's outcome. Deferred
// directly (recover works only there): a panic leaves the result zero, which
// would read as success, so the card closes failed and the panic goes on. end
// returns once the close is on the wire (pairworkloads.Emitter.Begin), so Run
// does not return, and its door does not answer, before the card is closed.
func closeCall(end func(res core.Result), res *core.Result) {
	if r := recover(); r != nil {
		end(core.Result{Deferred: true, Reason: fmt.Sprintf("panic: %v", r)})
		panic(r)
	}
	end(*res)
}

func (p *Pipeline) Run(ctx context.Context, req core.Request) (res core.Result) {
	start := time.Now()
	callID := ""
	if p.tracker != nil && req.Task.Valid() {
		if id, working, end := p.tracker.Begin(string(req.Task), req.Door); end != nil {
			callID = id
			ctx = core.WithWorkingMark(ctx, working)
			defer closeCall(end, &res)
		}
	}
	meta := core.Meta{Model: p.cfg.Model, CallID: callID, ParentJobID: req.ParentJobID}
	// Register A-102: carry the caller's door into telemetry so the ledger row
	// names the surface that admitted the call. Documentary only — nothing below
	// reads it, and every sub-branch takes meta by value from here.
	meta.Door = req.Door
	// ADR 0064 (register C-63): the job id a fleet node was dispatched this call
	// under, carried to the ledger row the same way the door is, so a node's row
	// joins to the delegator's on one equality.
	meta.FleetJobID = req.FleetJobID
	// The asker's name a fleet node was told (X-Offload-Asker), carried to the ledger row the same way.
	meta.Requester = req.Requester
	// The session and run class of the box that asked (ADR 0087), carried to the ledger row the same way: the row
	// names the session the node worked for, not the node process's own.
	meta.AskerSession, meta.AskerClass = req.OriginSession, req.RunClass

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

	// forecast posts req.Input (the forecast server's JSON payload) to the Timestral seat. Its own
	// branch: numbers in, numbers out, never the text cascade.
	if req.Task == core.TaskForecast {
		return p.runForecast(ctx, req, meta, start)
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
	// media slot, the whole node; see runPipelineJob's doc comment).
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

	// accel runs ONE local accelerator tool for a fleet caller that lacks the
	// device (acceltask.go). Its own branch: no cascade, no grammar, no GPU.
	if req.Task == core.TaskAccel {
		return p.runAccelTask(ctx, req, meta, start)
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

	// animate_character retargets the motion of req.Video (a driver clip) onto
	// req.Image (a reference character) on the local ComfyUI by shelling out (via
	// internal/gpugen) to comfy-animate.mjs. Its own branch — no text cascade.
	if req.Task == core.TaskAnimateCharacter {
		return p.runAnimateCharacter(ctx, req, meta, start)
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
	// compose_video renders an HTML/CSS composition with HyperFrames (composevideo.go):
	// CPU-class like media — software GL + CPU encode, NO GPU lease and no withGpuSlot
	// (ADR 0026/0059), only its own in-process compose slot.
	if req.Task == core.TaskComposeVideo {
		return p.runComposeVideo(ctx, req, meta, start)
	}
	// browse drives the operator's own browser through the sidecar (browse.go,
	// ADR 0060): opt-in, loopback decision endpoint only, no GPU lease, its own slot.
	if req.Task == core.TaskBrowse {
		return p.runBrowse(ctx, req, meta, start)
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
	//
	// The params ingredient is tasks.CacheParamsKey, the typed canonical form, and NOT
	// StableParamsKey: that one prints nested values with %v, which is type-erased, so a
	// schema's "required":["a b"] and "required":["a","b"] rendered alike and one request
	// was served the other's cached answer before its own schema was ever checked.
	// ck == "" means the params cannot be keyed at all (resultCacheParamsKey): this call
	// then neither reads nor writes the cache.
	ck := ""
	if paramsKey, keyable := p.resultCacheParamsKey(req.Task, req.Params, &meta); keyable {
		ck = cacheKeyFor(req.Task, orig, paramsKey, p.cfg.Model, built, shots)
	}
	if p.cache != nil && ck != "" {
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
	// Composite tier (ADR 0052): ONE placement decision for this call — the
	// chain is built from it and every rung's result is stamped with it.
	placed := p.cascadePlacement()
	chain := p.modelChainOn(req.Task, meta.Feat, knnSkip, placed)
	// The terminal reasoning tier has no display twin: on the display layer it
	// is skipped (it would evict the pair seat from device 0).
	reasoningModel := ""
	if p.cfg.ReasoningModel != "" && (placed == nil || placed.Layer != placement.LayerDisplay) {
		reasoningModel = p.cfg.ReasoningModel
	}
	// Cascade seat guard (seatguard.go): no rung whose load would evict a
	// loaded vLLM seat reaches this box's llama-swap. Inactive = chain as is.
	guard := p.planSeatGuard(ctx, chain, reasoningModel)
	chain = guard.chain
	var last core.Result
	// Task 1.5: entry-tier (ci==0) snapshot + candidate, so a later agreeing tier
	// can record a cascade-agreement correctness-proxy label for classify/triage.
	var entrySnapshot *ledger.Entry // value copy — safe vs meta mutation across iterations
	var entryCandidate string       // entry-tier candidate JSON (its Partial)
	// tried: models this call already sent a completion to — a lane rung whose
	// lane was gone at send is served by the seat, and the seat is then not
	// asked again later in the same chain.
	tried := map[string]bool{}
	for ci, model := range chain {
		if tried[model] {
			continue
		}
		meta.Model = model
		meta.Placed = rungPlaced(placed, model)
		meta.Escalations = ci
		// TO-3: a climbed-to tier re-reads the ORIGINAL source against its own
		// served window instead of inheriting the entry cut. Fail-open to the
		// entry packing — byte-identical to the pre-TO-3 behavior — with the
		// disposition recorded on the row (tier_pack). The entry tier (ci==0)
		// is untouched: no probes, no tokenize round-trips on the hot path.
		if _, offBox := guard.lane[model]; ci > 0 && offBox {
			// The window probe behind packForTier asks THIS box's
			// /upstream/<rung>/props, which loads the rung here — the eviction
			// the guard is keeping this rung off the box to avoid. The lane
			// rung reads the entry packing instead (fail-open, labelled).
			meta.TierPack = "entry-inherited (seat guard: the rung is served off this box, and probing its window here would load it)"
			req.Input = entryPacked
			built = entryBuilt
		} else if ci > 0 {
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
		res, escalatable := p.attempt(ctx, req, built, ck, model, meta, start, true, entryLen, guard.opts(model)...)
		if res.Meta.ErrClass == errClassLaneGone {
			// The plan sent this rung to its lane; at the send the lane no
			// longer served it and the client refused rather than evict the
			// seat here. Take the guard's next door instead (seatguard.go).
			model = guard.laneGone(model)
			meta.Model = model
			meta.Placed = rungPlaced(placed, model)
			res, escalatable = p.attempt(ctx, req, built, ck, model, meta, start, true, entryLen)
		}
		tried[model] = true
		// An escalating attempt that wrote an inner row minted the call's job id;
		// every later row of this call (the answer, or the final defer) carries it.
		if res.Meta.JobID != "" {
			meta.JobID = res.Meta.JobID
		}
		meta.CardsCarried = res.Meta.CardsCarried
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
	// The reasoning model has no display twin either: on the display layer it
	// is skipped for the same reason the escalation rung is dropped (it would
	// evict the pair seat from device 0 — the eviction the layer exists to avoid).
	// The seat guard keeps it on its lane, or — when its load would evict a
	// loaded vLLM seat with no lane to carry it — gives the reasoning ATTEMPT
	// to that seat (guard.reasoningOn): still one attempt, the reasoning
	// budget, marked Reasoning, behind this same gate. It is not run at all
	// when that seat already answered this call.
	reasoningTarget := reasoningModel
	if guard.reasoningOn != "" {
		reasoningTarget = guard.reasoningOn
	}
	if reasoningModel != "" && guard.reasoningOn != "" && tried[guard.reasoningOn] {
		log.Printf("cascade seat guard: reasoning %s -> not run: the loaded seat %s it would be served by already answered this call", reasoningModel, guard.reasoningOn)
		reasoningModel = ""
	}
	if reasoningModel != "" && built.Grammar != "" && !last.Meta.Truncated {
		meta.Placed = rungPlaced(placed, reasoningTarget)
		// TO-3: the terminal reasoning tier is a callee too — re-pack from the
		// original against ITS served window (same fail-open contract). Its
		// REAL completion request is MaxTokens+reasoningThinkBudget (the
		// think-wrapped grammar emits a <think> span before the JSON) — the
		// round-1 CRITICAL finding: budgeting against bare MaxTokens overshot
		// the served window by ~384 tokens on exactly the large inputs this
		// feature exists for.
		if guard.reasoningLane != "" {
			// Served off this box: its window probe here would load it here.
			meta.TierPack = "entry-inherited (seat guard: the rung is served off this box, and probing its window here would load it)"
			req.Input = entryPacked
			built = entryBuilt
		} else {
			rInput, rPath := p.packForTier(ctx, reasoningTarget, orig, entryPacked, req, built.MaxTokens+reasoningThinkBudget, decorate)
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
		}
		rres, ok := p.attemptReasoningOn(ctx, reasoningTarget, req, built, ck, meta, start, entryLen, guard.reasoningOpts()...)
		if !ok && rres.Meta.ErrClass == errClassLaneGone {
			// Its lane was gone at the send: the seat takes the reasoning
			// attempt when one is known and has not answered yet; with no seat
			// known the reasoning tier runs as configured (the guard's
			// documented residual).
			switch seat := guard.reasoningLaneGone(p.cfg.ReasoningModel, tried); {
			case seat != "":
				meta.Placed = rungPlaced(placed, seat)
				rres, ok = p.attemptReasoningOn(ctx, seat, req, built, ck, meta, start, entryLen)
			case guard.reasoningSeat == "":
				rres, ok = p.attemptReasoning(ctx, req, built, ck, meta, start, entryLen)
			default:
				rres = last // the seat already answered this call: keep its outcome
			}
		}
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

// recordExtractImageCall writes the composite's own row: the parent the ocr and
// extract rows name. Each token is counted once by every reader:
//
//   - TokensIn is only what no inner row carries: the prompts of sub-calls a cache
//     hit answered (an inner cache-hit row saves nothing). The prompts of the
//     sub-calls that ran stay on their inner rows, whose TokensIn the savings
//     summary adds (C-62).
//   - TokensOut is the call's whole output; an inner row's is never counted.
//   - CardsTokens is the work the cards did across the sub-calls that ran,
//     set explicitly because Record writes 0 on an inner row and would derive
//     this row's figure from the reduced TokensIn.
//   - The row is a cache hit only when every sub-call that ran was one, so a call
//     that did no card work makes no card.
//
// A sub-call's own escalating attempts are inner rows too (flattened under this
// composite). Their prompt tokens reach the savings summary (a non-deferred inner
// row's TokensIn is added, C-62) and their card work reaches CardsTokens through
// the sub-call's Meta.CardsCarried; their output is on no call row, because only
// the answering tier's figures are in res.Meta.
func (p *Pipeline) recordExtractImageCall(task core.TaskType, meta core.Meta, start time.Time, res core.Result, subs []core.Result) {
	if p.led == nil {
		return
	}
	row := meta
	row.Model = res.Meta.Model
	row.ErrClass = res.Meta.ErrClass
	row.LatencyMs = time.Since(start).Milliseconds()
	row.TokensIn, row.TokensOut = 0, 0
	row.CacheHit = len(subs) > 0
	cards, cachedIn := 0, 0
	for _, s := range subs {
		row.TokensOut += s.Meta.TokensOut
		if s.Meta.CacheHit {
			cachedIn += s.Meta.TokensIn
			continue
		}
		row.CacheHit = false
		cards += s.Meta.TokensIn + s.Meta.TokensOut
	}
	for _, s := range subs {
		// A sub-call's own climbing attempts (inner rows under this composite) did
		// card work that no row but the answering tier's could carry.
		cards += s.Meta.CardsCarried
	}
	row.TokensIn = cachedIn
	e := entryFrom(task, row, res.Deferred, 0)
	if res.Deferred {
		e.Reason = res.Reason
	}
	e.CardsTokens = cards
	_ = p.led.Record(e)
}

// RecordAccel writes one ledger row for an accelerator call (register E-04).
// The accelerator MCP tools bypass the pipeline — the sidecar or a fleet node
// answers directly — so until 0.130.3 nothing they ran reached the savings
// ledger. task is the sidecar tool (classify_image, …), modelTier the device
// id ("coral-edgetpu", "hailo-8l"), "<node>:<id>" for a forwarded call, or
// "<id>@fleet" when the forward failed before a node answered. A nil ledger
// (held by another process) records nothing, like every other task.
func (p *Pipeline) RecordAccel(task, modelTier string, latencyMs int64, deferred bool, reason string) {
	if p == nil || p.led == nil {
		return
	}
	_ = p.led.Record(ledger.Entry{Task: task, ModelTier: modelTier, LatencyMs: latencyMs, Deferred: deferred, Reason: reason})
}

// entryFrom builds a ledger entry from per-call meta + the enriched signals.
//
// It is shared by every caller that snapshots a row (the entry tier's correctness-label snapshot, the
// confidence head's feature row, the shadow capture), so it carries no card work of earlier attempts:
// those are the call's own row's business (callRowFrom).
func entryFrom(task core.TaskType, meta core.Meta, deferred bool, inputChars int) ledger.Entry {
	return ledger.Entry{
		Task: string(task), TokensIn: meta.TokensIn, TokensOut: meta.TokensOut, SeatTokensIn: meta.SeatTokensIn,
		LatencyMs: meta.LatencyMs, TokPerSec: meta.TokPerSec, CacheHit: meta.CacheHit,
		Deferred: deferred,
		Margin:   meta.Margin, ModelTier: meta.Model, Escalations: meta.Escalations,
		MarginScale: meta.MarginScale, MarginDeclaredMass: meta.MarginDeclaredMass, MarginAmbiguous: meta.MarginAmbiguous,
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
		Layer:              placedLayer(meta.Placed),
		// The job behind an agent row and the route's placement note (D-101):
		// empty — omitted — on a plain cascade call.
		JobID:          meta.JobID,
		ParentJobID:    meta.ParentJobID,
		CallID:         meta.CallID,
		QueuedMs:       meta.QueuedMs,
		Placement:      meta.Placement,
		Steps:          meta.Steps,
		StopReason:     meta.StopReason,
		RepackMs:       meta.RepackMs,
		RepackAttempts: meta.RepackAttempts,
		// The surface that admitted the call (A-102): "offload_summarize",
		// "cli:summarize", "fleet". Empty when no door stamped the request.
		Door: meta.Door,
		// The fleet job id of a dispatched call (ADR 0064); empty on any other.
		FleetJobID: meta.FleetJobID,
		// Who a fleet node ran the job for (D11); empty on any other row.
		Requester: meta.Requester,
		// The session and run class the job was for (ADR 0087); empty on any row no asker named one for, which
		// Record then stamps with this process's own.
		OriginSession: meta.AskerSession,
		RunClass:      meta.AskerClass,
		// The media binding's license (ADR 0058); empty on text rows.
		License: meta.License,
		// Same read the delegation log does (delegate.record): per-row, so a
		// long-lived process whose environment never changes still labels
		// every row consistently, and an untagged process writes nothing.
		Arm: strings.TrimSpace(os.Getenv("OFFLOAD_DELEGATE_ARM")),
	}
}

// callRowFrom is entryFrom for a row the ledger will hold as the CALL's row (the answering tier's, or
// the final defer): it adds the card work of the call's earlier attempts' inner rows (whose
// cards_tokens Record zeroes) to its own, so the call's row holds all of the call's work and a total
// over the ledger counts each token once. An inner row (ParentJobID set) adds nothing. Only the two
// writers of a call's row use it; a snapshot built by entryFrom already has the attempt's work in
// its own figures, and adding the carried work to it counts that work twice.
func callRowFrom(task core.TaskType, meta core.Meta, deferred bool, inputChars int) ledger.Entry {
	e := entryFrom(task, meta, deferred, inputChars)
	if meta.CardsCarried > 0 && meta.ParentJobID == "" {
		e.CardsTokens = meta.CardsCarried + ledger.CardsTokensOf(e)
	}
	return e
}

// placedLayer is the ledger's read of a placed block: the layer, or "" (the
// column is omitted) when the box published none.
func placedLayer(pl *core.Placed) string {
	if pl == nil {
		return ""
	}
	return pl.Layer
}

func (p *Pipeline) record(task core.TaskType, meta core.Meta, inputChars int) {
	if p.led == nil {
		return
	}
	_ = p.led.Record(callRowFrom(task, meta, false, inputChars))
}

// recordDefer logs a single deferred ledger entry for the final cascade
// outcome, carrying the human-readable defer reason (LO-8: err_class alone
// made incidents invisible — the Jul-1 GPU-contention defers all read as bare
// timeouts with no way to see WHY from the ledger).
func (p *Pipeline) recordDefer(task core.TaskType, meta core.Meta, inputChars int, reason string) {
	if p.led == nil {
		return
	}
	e := callRowFrom(task, meta, true, inputChars)
	e.Reason = reason
	_ = p.led.Record(e)
}

// recordDeferClassed is recordDefer for a defer that carries its class (the agent door's, core.DeferClass*).
// The class is the typed fact the reason's prose cannot give a reader who groups rows: "output failed
// schema", "structured re-pack skipped" and "step budget exhausted" are three sentences and one class
// (F22: 31 + 20 + 4 of the agent door's deferred rows on one night filed as "other"). The ledger settles the
// reason code from it when it knows the class, and from the reason otherwise.
func (p *Pipeline) recordDeferClassed(task core.TaskType, meta core.Meta, inputChars int, reason, deferClass string) {
	if p.led == nil {
		return
	}
	e := callRowFrom(task, meta, true, inputChars)
	e.Reason = reason
	e.ReasonCode = ledger.CodeForDeferClass(deferClass)
	_ = p.led.Record(e)
}
