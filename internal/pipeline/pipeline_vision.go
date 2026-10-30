// Vision and video-describe calls: the vision seat helpers, runVision/runVisionGen, runVideoDescribe and extract-image.

package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpulock"
	"github.com/dmmdea/offload-harness/internal/imageio"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/placement"
	"github.com/dmmdea/offload-harness/internal/tasks"
	"github.com/dmmdea/offload-harness/internal/videoio"
)

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

// visionRunOrigin is a vision record's Origin: the door, then whom the work is for. A node's
// call for another box names the asker and the job; a call on its own box names the parent
// process, which for a CLI call is the script that loops it (on 2026-10-28 finding that
// script took a walk of the process table).
func visionRunOrigin(req core.Request) string {
	door := req.Door
	if door == "" {
		door = string(req.Task)
	}
	if req.Requester != "" || req.FleetJobID != "" {
		return strings.TrimSpace(fmt.Sprintf("%s for %s %s", door, req.Requester, req.FleetJobID))
	}
	return fmt.Sprintf("%s, parent pid %d", door, os.Getppid())
}

// visionRunGoal is a vision record's Goal: the question when the task asks one, else the
// name of the file it reads (never an inline data URI).
func visionRunGoal(req core.Request) string {
	if q, ok := req.Params["question"].(string); ok && strings.TrimSpace(q) != "" {
		return q
	}
	for _, src := range []string{req.Image, req.Video} {
		if src != "" && !strings.HasPrefix(src, "data:") {
			return string(req.Task) + " " + filepath.Base(src)
		}
	}
	return string(req.Task)
}

// visionPlaced is the documentary placement of a vision call on a composite
// box (placement row 3: ocr → the single layer's ocr seat, everything else →
// the pair's vision seat), with Seat = the alias that actually ran so the
// block never claims a seat the call did not use. nil on a plain box or when
// no layer declares the role.
func (p *Pipeline) visionPlaced(task core.TaskType, model string) *core.Placed {
	if !p.cfg.Composite() {
		return nil
	}
	class := placement.ClassVision
	if task == core.TaskOCR {
		class = placement.ClassOCR
	}
	dec := placement.Decide(placement.Request{Class: class}, p.cfg.Layers, p.live())
	if dec.Defer || dec.Layer == "" {
		return nil
	}
	pl := dec.Placed
	pl.Seat = model
	return &pl
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
	meta.Placed = p.visionPlaced(req.Task, model)
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
		return p.client.GenerateVision(gctx, llamaclient.VisionRequest{Model: model, System: built.System, User: built.User, ImageDataURIs: []string{dataURI}, Grammar: built.Grammar, MaxTokens: built.MaxTokens, Temperature: p.cfg.Temperature}, llamaclient.WithoutThinking())
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
	//
	// Input and cacheKeyExtra are separate PARTS of the key, not one part joined with
	// "|": req.Input is arbitrary caller text, so joined, the input "x|y" with the extra
	// "z" was the same key as the input "x" with the extra "y|z". The extra is the only
	// carrier of the source file's identity, so that is one file's cached result served
	// for another.
	//
	// The params ingredient is the typed canonical form (tasks.CacheParamsKey); params
	// that cannot be keyed make the call uncacheable, like an unknown file identity does
	// (resultCacheParamsKey says why).
	paramsKey, keyable := p.resultCacheParamsKey(req.Task, req.Params, &meta)
	ck := cache.Key(string(req.Task), req.Input, cacheKeyExtra, paramsKey, meta.Model, built.Grammar,
		templateCacheTag(built.System, built.Grammar, built.User, req.Input))
	cacheable = cacheable && keyable
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
	//
	// The wait is for a lease on the cards THIS seat is pinned to (plan P4): the delegator's auto
	// route reads the lease the same way, and a render on another card is not one to wait for
	// (reading it whole-node here deferred a call the fleet used to serve).
	//
	// A lease this process runs UNDER is not one to wait for: `gpu reserve ... -- local-offload
	// vqa|ocr|assess-image` is the command the lease was taken for, exactly as the render runners
	// inherit it (ambientLeaseEnv) and the delegator exempts it (delegate.ForeignFence). Waiting
	// on it deferred every call of a vision bake run under its own lease (2026-09-12), so a
	// measured vision run had to stop the fleet node instead of queueing like every other GPU
	// job. Per lease: a child of lease A still waits for any other lease on the seat's cards.
	visionModel := meta.Model
	readLease := func(dir string) gpulease.Info {
		return modelaffinity.ScopeToModel(modelaffinity.InspectLease(dir), visionModel).Where(func(l gpulease.Info) bool {
			return !gpulease.Inherited(l)
		})
	}
	if info := gpulock.WaitFreeScoped(ctx, p.gpuLockPath, p.visionGPUWait, p.visionGPUPoll, readLease); info.Held {
		meta.LatencyMs = time.Since(start).Milliseconds()
		meta.ErrClass = errClassGPUBusy
		reason := fmt.Sprintf("gpu busy: generation job holds the lock (%ds)", int(info.Age/time.Second))
		p.recordDefer(req.Task, meta, len(req.Input), reason)
		return core.Deferf(reason, "", meta)
	}

	// The call is on its seat from here to its return: registered (gpuactivity.KindVision), so
	// `gpu status`, `offload_status` and a node's health name it and who asked for it. A cache
	// hit and a lease defer above never reach the seat and are not registered. It registers in
	// admission, like every run (register C-60), while a cold seat loads, and is running from
	// the request on.
	act := gpuactivity.Start(p.cfg.GPULockPath, p.cfg.StateDir, gpuactivity.Run{Seat: meta.Model, Kind: gpuactivity.KindVision, Origin: visionRunOrigin(req), Goal: visionRunGoal(req), Phase: gpuactivity.PhaseAdmission})
	defer act.End()

	// F49: a COLD vision seat is loaded BEFORE the request is sent, not inside it. The seat
	// unloads after its idle ttl, and llama-swap holds the first request open for the whole
	// load, so the request's timeout (request_timeout_sec) had to cover the load AND the
	// inference: a 32B seat's cold load plus a many-frame prefill outlasted it and the call
	// failed with "Client.Timeout exceeded while awaiting headers" while the load was still
	// running. The bounded wait below (vision_coldload_wait_sec) is a separate passthrough
	// request, so the vision request keeps its full timeout for inference. A seat already
	// listed ready costs one /running read.
	coldNote := p.waitForColdVisionSeat(ctx, meta.Model)
	act.Phase(gpuactivity.PhaseRunning)

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
		reason := "vision model call failed: " + gerr.Error()
		if coldNote != "" {
			// Say that the seat was cold and how long the load was waited for, so a
			// failure after a long cold load is not read as a failure of a warm call.
			reason += coldNote
		}
		// A context-overflow defer that runVideoDescribe is about to retry at a
		// smaller frame width is a STEP of that call, not the call: its row is
		// an inner row of the row the final attempt writes.
		row := meta
		if rt, _ := ctx.Value(visionRetryKey{}).(*visionRetry); rt != nil && rt.canRetry && row.ParentJobID == "" && isContextOverflow(reason) {
			if rt.id == "" {
				rt.id = mintJobID("vd")
			}
			// An inner row names the call; it is never the call (JobID empty), even
			// on the second retried attempt, whose meta already carries the id.
			row.ParentJobID, row.JobID = rt.id, ""
		}
		p.recordDefer(req.Task, row, len(req.Input), reason)
		return core.Deferf(reason, "", meta)
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
	// The retry loop below writes one row per attempt. An overflowed attempt that
	// WILL be retried (width above the floor) is an inner row of the call; the
	// attempt that ends the loop is the call's row and carries the job id the
	// inner rows named. Nothing is minted unless an attempt really is retried, so
	// the common one-attempt call writes the same single plain row as before.
	retry := &visionRetry{}
	ctx = context.WithValue(ctx, visionRetryKey{}, retry)
	for {
		retry.canRetry = width > 256
		if retry.id != "" {
			meta.JobID = retry.id
		}
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
			return p.client.GenerateVisionInterleaved(gctx, llamaclient.InterleavedRequest{Model: p.cfg.VisionModel, System: built.System, FrameLabels: labels, ImageDataURIs: frames, TrailingUser: built.User, Grammar: built.Grammar, MaxTokens: built.MaxTokens, Temperature: p.cfg.Temperature}, llamaclient.WithoutThinking())
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

// runExtractImage is the COMPOSITE extract_image flow: it OCRs the image via the
// existing ocr task, then feeds the OCR text into the EXISTING text extract task.
// This reuses the proven extract path unchanged — GBNF object grammar, verbatim
// grounding (extracted values must appear in the OCR text), schema validation,
// and the escalation/defer ladder all come for free. There is no new extraction
// logic here; runExtractImage only composes ocr + extract.
//
// Telemetry (plan D12): the two sub-calls still record their own ledger rows (an
// `ocr` vision row + an `extract` text row), but as INNER rows of one row the
// composite writes for itself on EVERY exit (recordExtractImageCall). One call,
// one card; the sub-call rows stay for the per-task detail (register C-62).
func (p *Pipeline) runExtractImage(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	jobID := mintJobID("xi")
	meta.JobID = jobID
	var subs []core.Result
	done := func(res core.Result) core.Result {
		p.recordExtractImageCall(req.Task, meta, start, res, subs)
		return res
	}
	// 1. OCR the image via the existing ocr task (reuses runVision + the vision
	//    tier). A propagated defer covers image-load, empty-output, and model-fail.
	ocrRes := p.Run(ctx, core.Request{Task: core.TaskOCR, Image: req.Image, Door: req.Door, ParentJobID: jobID})
	subs = append(subs, ocrRes)
	if !ocrRes.OK {
		return done(ocrRes)
	}
	// 2. Pull the OCR text out of ocrRes.Data ({"text": "..."}).
	var m map[string]string
	if jerr := json.Unmarshal(ocrRes.Data, &m); jerr != nil {
		return done(core.Deferf("unreadable OCR result for extract_image: "+jerr.Error(), "", ocrRes.Meta))
	}
	ocrText := m["text"]
	if strings.TrimSpace(ocrText) == "" {
		return done(core.Deferf("empty OCR text for extract_image", "", ocrRes.Meta))
	}
	// 3. Run the EXISTING extract on the OCR text — grammar + grounding (against
	//    ocrText) + schema validation, all reused. The caller's schema rides in
	//    req.Params exactly as offload_extract passes it.
	exRes := p.Run(ctx, core.Request{Task: core.TaskExtract, Input: ocrText, Params: req.Params, Door: req.Door, ParentJobID: jobID})
	subs = append(subs, exRes)
	return done(exRes)
}
