// The image batch path: job normalisation, JSONL, result parsing and RunImageBatch*.

package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/imagegen"
	"github.com/dmmdea/offload-harness/internal/tasks"
)

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
	// The binding that rendered the item and its license (ADR 0058), the same keys
	// the single path's imageResultPayload carries. A batch always renders the
	// DEFAULT binding (there is no family seam on this path), so every item carries
	// that binding's family and — when imagegen_license declares one — its license
	// and commercial_use. Absent license = the binding declares none (UNKNOWN, never
	// "commercial-safe").
	Family        string `json:"family"`
	License       string `json:"license,omitempty"`
	CommercialUse *bool  `json:"commercial_use,omitempty"`
	// Devices are the two cards a batch on the card-only placement ran on (the lease ids, as the queued answer names them). Only a batch
	// that held that pair carries them; every other item is byte-identical to what it was.
	Devices []string `json:"devices,omitempty"`
}

// tagLicense stamps a batch item with the rendering binding's family and license —
// the batch twin of addLicenseData.
func (it *ImageBatchItem) tagLicense(fam config.FamilyInfo) {
	it.Family = fam.Name
	it.License = fam.License
	if fam.CommercialUse != nil {
		v := *fam.CommercialUse
		it.CommercialUse = &v
	}
}

// normalizeImageBatch fills the per-job invariants the single-render path enforces
// (a concrete seed BEFORE the render so the report is reproducible; a stable output
// path under mediaDir) and renders the jobs file the render script consumes. Pure.
func normalizeImageBatch(jobs []ImageBatchJob, mediaDir string) ([]ImageBatchJob, string) {
	return normalizeImageBatchFor(jobs, mediaDir, "")
}

// normalizeImageBatchFor is normalizeImageBatch for a batch of one named family: the family is part of a default out's
// hash, as it is part of the single render's (its `family` param is in the hashed params), so the same prompt and seed
// under two families are two files and not one overwriting the other. No family hashes exactly as it always did.
func normalizeImageBatchFor(jobs []ImageBatchJob, mediaDir, family string) ([]ImageBatchJob, string) {
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
			if family = strings.TrimSpace(family); family != "" {
				params["family"] = family
			}
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

// The rows a batch that stopped itself leaves (render/batch-jobs.mjs, the cross-process contract of runBatchJobs):
//   - a job the batch never started is recorded "not run: <why>" for the stop (a ComfyUI that can run no more work, a full disk);
//   - the job that hit an unusable ComfyUI is recorded with its child's reason, "comfy-render exited 3: ..."
//     (renderExitError, RENDER_EXIT_SERVER_UNUSABLE).
//
// TestBatchErrClassFilesTheAbortRowsWithTheBatchsTypedClass and the end-to-end batch test run the real runBatchJobs, so a change to
// either text turns them red.
const (
	batchNotRunPrefix         = "not run: "
	batchServerUnusablePrefix = "comfy-render exited 3"
)

// batchErrClass classifies a failed batch item for the ledger's ErrClass, mirroring
// the single path's classifyErr. A job with no recorded result line died with the
// batch itself, so the batch-level error (gerr) is its true cause; so is a job the
// batch stopped on or never ran, when gerr carries a class gpugen decided structurally
// (a *gpugen.RunError: the IGPU_CLASS= line the batch's output ends with, or a deadline):
// without that, a ComfyUI that died mid-batch filed every row "other", the words of
// "not run: ComfyUI became unusable at job 3/6 (...)" naming no class (D-38, issue #72).
// A job with its own error line is classified from that: one that failed on its own before
// the stop keeps the class its own words give. Pure.
func batchErrClass(itemErr string, gerr error) string {
	if gerr != nil {
		if strings.Contains(itemErr, "no result recorded") {
			return classifyErr(gerr)
		}
		var re *gpugen.RunError
		if errors.As(gerr, &re) && re.Class != "" {
			switch {
			case strings.HasPrefix(itemErr, batchNotRunPrefix):
				return re.Class
			// the row of the job the batch stopped on describes only the ComfyUI class: another typed class (a deadline, a reset)
			// is not what that child's exit 3 said.
			case re.Class == "comfy_unusable" && strings.HasPrefix(itemErr, batchServerUnusablePrefix):
				return re.Class
			}
		}
	}
	return classifyErr(fmt.Errorf("%s", itemErr))
}

// NormalizeImageBatch is normalizeImageBatchFor without the jobs file: the batch's jobs with a concrete seed and a concrete
// out filled in, exactly as RunImageBatchAs fills them. The CLI calls it before it chooses where the batch runs, so a job
// that names neither has the same seed and out whether the whole batch renders warm here or each job is placed on the
// fleet (the default out is a hash of the seed, so a seed minted twice would be two files). Idempotent: a job that already
// has both is unchanged.
func NormalizeImageBatch(jobs []ImageBatchJob, mediaDir, family string) []ImageBatchJob {
	norm, _ := normalizeImageBatchFor(jobs, mediaDir, family)
	return norm
}

// RunImageBatch renders N prompts through ONE warm ComfyUI session under the machine's default binding: the batch of
// RunImageBatchAs with no family.
func (p *Pipeline) RunImageBatch(ctx context.Context, jobs []ImageBatchJob) ([]ImageBatchItem, error) {
	return p.RunImageBatchAs(ctx, "", jobs)
}

// RunImageBatchAs renders N prompts through ONE warm ComfyUI session (the checkpoint
// loads once) while preserving zero-always-warm AT THE BATCH BOUNDARY: the render
// script's single teardown + gpugen's deferred /free restore a clean GPU when the
// batch ends, however it ends. Ledger: one entry per job, same model label as the
// single-render path (health tiers must not fragment).
//
// family selects the binding exactly as the single render's `family` does (ADR 0058): "" or the default binding's own
// name is this machine's default, a named overlay renders under its own config copy (its checkpoint, steps, card pin,
// license) and tags every item with it. EVERY read of the binding below goes through that resolved copy, never p.cfg: the
// lease is asked for on the family's card and sized from the family's files, the launch is the family's pin, and the
// ledger label and the tag are the family's. Only what no overlay changes (the prompt refiner, the ledger) stays on p.cfg.
// A family on stable-diffusion.cpp has no warm session to share, so it is refused by name: its jobs render one at a time.
func (p *Pipeline) RunImageBatchAs(ctx context.Context, family string, jobs []ImageBatchJob) ([]ImageBatchItem, error) {
	return p.RunImageBatchWaiting(ctx, family, jobs, 0)
}

// RunImageBatchWaiting is RunImageBatchAs for a batch that has to wait for a busy lane: the fleet cannot relieve it (no
// roster node renders its recipe), so it keeps its place in the lease line instead of giving up at the pipeline's own window
// and coming back `queued`, which the caller would only run again from the back of the line. wait > 0 is how long it keeps
// that place; 0 is the pipeline's own gpu_wait_ms window, the call RunImageBatchAs always was. The wait ends the moment ctx
// does, with an error that says so and is not a busy answer (IsGPUBusy is false for it).
func (p *Pipeline) RunImageBatchWaiting(ctx context.Context, family string, jobs []ImageBatchJob, wait time.Duration) ([]ImageBatchItem, error) {
	cfg, fam, ferr := p.cfg.ResolveImageFamily(family)
	if ferr != nil {
		return nil, ferr
	}
	if strings.TrimSpace(family) != "" && cfg.ImageGenEngine == "sdcpp" {
		return nil, fmt.Errorf("image family %q renders on stable-diffusion.cpp, which has no warm ComfyUI session to batch through: render its jobs one at a time", family)
	}
	if cfg.ImageGenScript == "" {
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
	script, serr := gpugen.ResolveScript(cfg.ImageGenScript)
	if serr != nil {
		return nil, serr
	}
	_ = os.MkdirAll(cfg.MediaDir, 0o755)
	norm, jsonl := normalizeImageBatchFor(jobs, cfg.MediaDir, family)
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
	jobsPath := filepath.Join(cfg.MediaDir, "batch-"+stamp+".jobs.jsonl")
	resultsPath := filepath.Join(cfg.MediaDir, "batch-"+stamp+".results.jsonl")
	if err := os.WriteFile(jobsPath, []byte(jsonl), 0o644); err != nil {
		return nil, err
	}

	// Same labeling rule as runGenerateImage: report the checkpoint this machine
	// actually renders with; UNBOUND keeps the historical "comfyui-sdxl" label so
	// health tiers don't fragment.
	modelLabel := "comfyui-sdxl"
	if cfg.ImageGenCkpt != "" {
		modelLabel = "comfyui:" + cfg.ImageGenCkpt
	}
	model := imageModelFromConfig(cfg)
	// The whole batch shares one timeout: per-image budget × N (the first job also
	// absorbs the ComfyUI cold start, which the per-image budget already covers today).
	timeout := time.Duration(cfg.ImageGenTimeoutSec) * time.Second * time.Duration(len(norm))
	// ONE lease for the WHOLE batch. That is the point of batching: the tier is torn
	// down once for N renders instead of once each, which is the arithmetic behind the
	// 3,356 unloads in the server log.
	// This helper returns items+error rather than a core.Result, so a busy card surfaces
	// as an error for the caller to classify — no items were produced.
	var grant mediaGrant
	var lerr error
	if wait > 0 {
		grant, lerr = p.acquireMediaLeaseHeld(ctx, "image-gen batch", timeout, wait, imageNeed(cfg, ""))
	} else {
		grant, lerr = p.acquireMediaLease(ctx, "image-gen batch", timeout, p.gpuWait(), imageNeed(cfg, ""))
	}
	if lerr != nil {
		return nil, lerr
	}
	defer grant.Release()
	model.Launch = grant.launch(model.Launch)
	gerr := imagegen.GenerateBatch(ctx, imagegen.BatchRequest{
		Node: cfg.NodePath, Script: script, ComfyDir: cfg.ComfyDir, JobsPath: jobsPath, ResultsPath: resultsPath,
		Model: model, Timeout: timeout, ExtraEnv: grant.Env,
	})

	raw, _ := os.ReadFile(resultsPath) // best-effort even on gerr: partial results are real work
	items := parseBatchResults(raw, norm)
	// The binding the batch rendered under (the default for no family, exactly what the
	// single path resolves for a request without one): every item is tagged with it (ADR 0058).
	for i, it := range items {
		items[i].tagLicense(fam)
		// A batch on the card-only placement says which two cards it ran on, on every item; any other batch's items are unchanged.
		if len(grant.Devices) > 0 {
			items[i].Devices = append([]string(nil), grant.Devices...)
		}
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
		// Every row carries the license the rendering binding declares, if any.
		meta := core.Meta{Model: modelLabel, LatencyMs: it.Ms, License: fam.License}
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
