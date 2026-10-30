// Run-graph and pipeline-job doors: run-graph params and envelope, runRunGraph, runPipelineJob, artifact publishing.

package pipeline

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/rungraph"
)

// runGraphTemplateName is what a template name looks like: the file name of a template of the package, never a path or a flag.
var runGraphTemplateName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// buildRunGraphParams maps request params → rungraph.Params. A missing graph path is a
// hard error (mapped to a clean defer upstream), never a silent empty run. A run names its
// graph (graph_path) or a template of the node's installed package (template; with overrides_path
// and seed), never both: the runner converts the template once ComfyUI is up.
func buildRunGraphParams(req core.Request) (rungraph.Params, error) {
	gp := paramStr(req.Params, "graph_path")
	tpl := paramStr(req.Params, "template")
	if tpl != "" {
		if gp != "" {
			return rungraph.Params{}, fmt.Errorf("run_graph: graph_path and template are exclusive")
		}
		if !runGraphTemplateName.MatchString(tpl) || strings.Contains(tpl, "..") {
			return rungraph.Params{}, fmt.Errorf("run_graph: template must be a template name (letters, digits, . _ -), got %q", tpl)
		}
	} else if gp == "" {
		return rungraph.Params{}, fmt.Errorf("run_graph: graph_path required")
	}
	p := rungraph.Params{
		GraphPath:    gp,
		ManifestPath: paramStr(req.Params, "manifest_path"),
		OutDir:       paramStr(req.Params, "out_dir"),
		ResultPath:   paramStr(req.Params, "result_path"),
		ReserveVram:  paramStr(req.Params, "reserve_vram"),
	}
	if tpl != "" {
		p.Template = tpl
		p.OverridesPath = paramStr(req.Params, "overrides_path")
		p.Seed = paramStr(req.Params, "seed")
	}
	return p, nil
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

// runGraphEnvelopePath names the result envelope of one call: a random name under the media dir, so two runs, whichever graphs they
// hold, never read or remove each other's.
func runGraphEnvelopePath(mediaDir string) string {
	var b [6]byte
	_, _ = crand.Read(b[:]) // cannot fail on this toolchain (Go >= 1.24)
	return filepath.Join(mediaDir, "run-graph-"+hex.EncodeToString(b[:])+".json")
}

// removeRunGraphEnvelope deletes the envelope of a call that has read it. A scanner that holds the file for a moment after the
// runner wrote it (a sharing violation on Windows) is waited out for a short while; a file that is not there is not an error.
func removeRunGraphEnvelope(path string) {
	for i := 0; i < 20; i++ {
		if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
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
	// Default the result envelope under the media dir so an inline caller need not pick a path. It is the runner's note to this
	// one call: read once below and removed when the call ends, whatever the run came to, so nothing it leaves behind grows with the
	// number of calls. (It used to be named from the graph's path, "so a re-run reuses one file", but a caller that hands a graph
	// file of its own for every call, which is what a graph ticket, an inline graph_json and a fleet node's job all do, got an
	// envelope of its own that nothing ever removed.) A caller that names result_path owns that file: it is written there and kept.
	if params.ResultPath == "" {
		_ = os.MkdirAll(p.cfg.MediaDir, 0o755)
		params.ResultPath = runGraphEnvelopePath(p.cfg.MediaDir)
		defer removeRunGraphEnvelope(params.ResultPath)
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
	// run-graph holds the whole node unless the operator DECLARES devices: the caller's graph owns
	// its placement, so nothing here can know which cards it will use.
	grant, lerr := p.acquireMediaLease(ctx, "run-graph", timeout, p.gpuWait(), declaredNeed(declaredDevices(req.Params), paramStr(req.Params, "waiter_token")).declaresRAM(graphRAM(p.cfg, req.Params)).resumableBy(req))
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer grant.Release()
	// run-graph gets the launch-wide keys but never the device pin: the caller's graph
	// owns its placement (comfyLaunch).
	env, gerr := rungraph.Run(ctx, p.cfg.NodePath, script, p.cfg.ComfyDir, params, timeout,
		p.footprintSampling(runGraphFootprintFamily(req.Params), "", "run-graph"), append(grant.Env, grant.launch(comfyLaunch(p.cfg, false)).Env()...)...)
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
// only the in-process media slot (the whole node) is acquired (takeMediaSlot/releaseMediaSlot),
// NEVER the machine-wide media lease (acquireMediaLease is not called). The
// CMP CLI's own nested per-stage calls take the machine-wide lease themselves
// for each stage (mirroring how a manual scene-swap run already works today —
// see the brief); acquiring it AGAIN here would self-deadlock the same
// process waiting on its own nested acquisition. A second concurrent
// pipeline-job in this process still can't race the first: it queues on
// the media slot for up to gpuWait() and then defers as gpu_busy, exactly like
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

// runGraphFootprintFamily is the run-graph footprint family: payload-declared
// model_family when the caller supplied one (the fleet dispatch path), else
// the generic "comfy-graph".
func runGraphFootprintFamily(params map[string]any) string {
	if fam := paramStr(params, "model_family"); fam != "" {
		return fam
	}
	// A template run is the same template every time, so its footprint is learnt per template name.
	if tpl := paramStr(params, "template"); tpl != "" {
		return "comfy-template-" + tpl
	}
	return "comfy-graph"
}
