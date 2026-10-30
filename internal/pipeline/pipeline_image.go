// Image doors: generate (ComfyUI and sd.cpp), inpaint, upscale, generative edit and SVG.

package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/hostneed"
	"github.com/dmmdea/offload-harness/internal/imagegen"
	"github.com/dmmdea/offload-harness/internal/svgkit"
	"github.com/dmmdea/offload-harness/internal/tasks"
)

// runGenerateImage renders req.Input (the prompt) to a PNG on the LOCAL ComfyUI by shelling
// out to comfy-generate.mjs (which takes the shared GPU lock and starts/stops ComfyUI). Its
// own branch — no text models, no grammar, no vision call. Any failure (no route, empty
// prompt, ComfyUI down, render error, timeout) defers to Claude. params: negative (string),
// width/height/steps/seed (int).
func (p *Pipeline) runGenerateImage(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	// Named families (ADR 0058): the request's `family` selects the binding. Absent (or
	// the default binding's own family name) = the node's default binding, byte-for-byte
	// what every request rendered before families existed; a named overlay renders
	// under its own config copy and tags the result with its license. Resolved BEFORE
	// the engine seam: a family may bind a different engine than the default.
	cfg, fam, ferr := p.cfg.ResolveImageFamily(paramStr(req.Params, "family"))
	if ferr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), ferr.Error())
		return core.Deferf(ferr.Error(), "", meta)
	}
	meta.License = fam.License
	if paramBool(req.Params, "transparent") && !cfg.SupportsTransparentImage() {
		reason := fmt.Sprintf("transparent output needs the %s family (the only one with an RGBA VAE, on either engine); family %s has no alpha channel — pass family:%q or drop transparent",
			config.FamilyQwenImage21, familyLabel(fam), config.FamilyQwenImage21)
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), reason)
		return core.Deferf(reason, "", meta)
	}
	// J2: per-machine media-engine seam. "sdcpp" routes to the stable-diffusion.cpp
	// runner (single Vulkan binary, no ComfyUI) — its own function so the ComfyUI
	// path below stays byte-for-byte unchanged. ""/"comfy" = the standing default.
	if cfg.ImageGenEngine == "sdcpp" {
		return p.runGenerateImageSdcpp(ctx, req, meta, start, cfg, fam)
	}
	if cfg.ImageGenScript == "" {
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
	script, serr := gpugen.ResolveScript(cfg.ImageGenScript)
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
	if cfg.ImageGenCkpt != "" {
		meta.Model = "comfyui:" + cfg.ImageGenCkpt
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
	// A flux3-klein binding with an upsampler expands the prompt in-graph (upsampler.go), so the node refiner stays off for it:
	// one caption, written by the model BFL trained for it, never a refined prompt upsampled again.
	refineOff := refineExplicitlyOff(req.Params)
	ups := llmUpsampler(cfg)
	renderPrompt, refined, refineNote, _ := p.maybeRefinePrompt(ctx, prompt, refineOff || comfyUpsampler(cfg) != "" || ups != "")
	// The model form of the upsampler (a llama-swap model) writes the caption here, before the lease; the file form runs in the graph.
	var upsampled bool
	var upsampleNote string
	if ups != "" && !refineOff {
		renderPrompt, upsampled, upsampleNote = p.harnessUpsample(ctx, ups, prompt)
	}

	timeout := time.Duration(cfg.ImageGenTimeoutSec) * time.Second
	// This machine's image-model binding (per-machine config; never hardcoded here —
	// an 8GB box runs SDXL, a 16GB box may run an all-in-one DiT). All fields are
	// optional: a zero Model passes no flags and the renderer keeps its own defaults.
	model := imageModelFromConfig(cfg)
	// refine:false is "render my prompt verbatim": the caller's caption is already the full scene, so the graph skips its upsampler.
	if refineOff {
		model.Upsampler = ""
	}
	// Passive fleet footprint: key this render by the machine's image binding
	// (family + the O1 bf16 quant) so measured peaks accumulate during normal use.
	imgFamily, imgQuant := imageFootprintKey(cfg)
	grant, lerr := p.acquireMediaLease(ctx, "image-gen", timeout, p.gpuWait(), imageNeed(cfg, paramStr(req.Params, "waiter_token")).forCanvas(req.Params).resumableBy(req))
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer grant.Release()
	// On a per-card instance the launch names the instance and the card (and blanks the index).
	model.Launch = grant.launch(model.Launch)
	outPath, gerr := imagegen.Generate(ctx, imagegen.GenerateRequest{
		Node: cfg.NodePath, Script: script, ComfyDir: cfg.ComfyDir, Out: out, Prompt: renderPrompt,
		Params: req.Params, Model: model, Timeout: timeout,
		Sampling: p.footprintSampling(imgFamily, imgQuant, "image-gen"), ExtraEnv: grant.Env,
	})
	if gerr != nil {
		meta.LatencyMs = time.Since(start).Milliseconds()
		meta.ErrClass = classifyErr(gerr)
		p.recordDefer(req.Task, meta, len(req.Input), "image generation failed: "+gerr.Error())
		return core.Deferf("image generation failed: "+gerr.Error(), "", meta)
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	payload := imageResultPayload(outPath, req.Params, seed, fam)
	addRefineData(payload, p.cfg.ImageGenRefinerModel, refined, renderPrompt, refineNote)
	switch {
	case refineOff:
	case comfyUpsampler(cfg) != "":
		// the runner keeps the in-graph caption beside the picture (render/comfy-render.mjs captionPath)
		addUpsampleData(payload, model.Upsampler, true, readCaption(outPath), "")
	case ups != "":
		addUpsampleData(payload, ups, upsampled, renderPrompt, upsampleNote)
	}
	grant.name(payload) // a card-only call says which two cards it ran on; any other answer is unchanged
	data, _ := json.Marshal(payload)
	p.record(req.Task, meta, len(prompt))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// imageResultPayload is the generate_image result both engines return. width/height
// are MEASURED from the written file: the request value (or a 1024 guess) was wrong for
// every family with a native size (qwen-image 1328, HiDream/qwen-image-2.1 2048) and
// for every size a builder snaps (2.1 floors to /32). The request is the fallback only
// when the header cannot be read. The family and its license ride every result.
func imageResultPayload(outPath string, params map[string]any, seed int, fam config.FamilyInfo) map[string]any {
	w, h := imagegen.OutputSize(outPath)
	if w <= 0 || h <= 0 {
		w, h = paramIntOr(params, "width", 1024), paramIntOr(params, "height", 1024)
	}
	payload := map[string]any{
		"image_path": outPath,
		"width":      w,
		"height":     h,
		"seed":       seed,
		"family":     fam.Name,
	}
	if paramBool(params, "transparent") {
		payload["transparent"] = true
	}
	addLicenseData(payload, fam)
	return payload
}

// addLicenseData stamps a result with the binding's license (ADR 0058): license and
// commercial_use when the binding declares them. No warning sentence rides along
// (operator order 2026-10-01).
func addLicenseData(payload map[string]any, fam config.FamilyInfo) {
	if fam.License != "" {
		payload["license"] = fam.License
	}
	if fam.CommercialUse != nil {
		payload["commercial_use"] = *fam.CommercialUse
	}
}

// familyLabel names a resolved binding in a defer reason.
func familyLabel(fam config.FamilyInfo) string {
	switch {
	case fam.Name == "" && fam.Default:
		return "(the default binding)"
	case fam.Default:
		return fmt.Sprintf("%q (the default binding)", fam.Name)
	}
	return fmt.Sprintf("%q", fam.Name)
}

// runGenerateImageSdcpp renders req.Input via stable-diffusion.cpp (J2): a single
// native binary spawned per job under the same GPU lock — zero-warm by construction,
// no ComfyUI anywhere on the path (no COMFY_DIR in the env, no post-run /free). The
// AMD/Vulkan tier's engine; any failure defers exactly like the ComfyUI path. cfg is
// the resolved binding (the default, or a named family's overlay).
func (p *Pipeline) runGenerateImageSdcpp(ctx context.Context, req core.Request, meta core.Meta, start time.Time, cfg config.Config, fam config.FamilyInfo) core.Result {
	deferf := func(reason string) core.Result {
		meta.LatencyMs = time.Since(start).Milliseconds()
		p.recordDefer(req.Task, meta, len(req.Input), reason)
		return core.Deferf(reason, "", meta)
	}
	if cfg.SdcppBin == "" {
		return deferf("imagegen_engine is sdcpp but sdcpp_bin is not configured")
	}
	if cfg.SdcppModel == "" {
		return deferf("imagegen_engine is sdcpp but sdcpp_model is not configured")
	}
	prompt := strings.TrimSpace(req.Input)
	if prompt == "" {
		return deferf("empty image prompt")
	}
	scriptCfg := cfg.SdcppScript
	if scriptCfg == "" {
		scriptCfg = "render/sdcpp-generate.mjs"
	}
	// Ledger/health tier: the sdcpp engine is its own tier keyed by the bound model
	// file — never the ComfyUI labels (health.groupByTier must not merge engines).
	meta.Model = "sdcpp:" + filepath.Base(cfg.SdcppModel)
	// CT-54: a file the binding names (a weight, the engine binary, the runner script) and the disk lacks is a typed defer before
	// anything is created, refined or leased, and before the script is resolved below (that resolution defers untyped). cfg is the
	// resolved binding (a named family's overlay renders under its own copy), and a derivation of it names its route
	// "generate_image".
	if res, notReady := p.routeReadyOrDefer(req, meta, start, cfg, "generate_image", "image generation"); notReady {
		return res
	}
	script, serr := gpugen.ResolveScript(scriptCfg)
	if serr != nil {
		return deferf(serr.Error())
	}
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
	// A flux3-klein binding's upsampler (upsampler.go) takes the refiner's place: BFL's system prompt on the bound model,
	// before the lease, falling back to the raw prompt on any problem.
	refineOff := refineExplicitlyOff(req.Params)
	ups := llmUpsampler(cfg)
	renderPrompt, refined, refineNote, _ := p.maybeRefinePrompt(ctx, prompt, refineOff || ups != "")
	var upsampled bool
	var upsampleNote string
	if ups != "" && !refineOff {
		renderPrompt, upsampled, upsampleNote = p.harnessUpsample(ctx, ups, prompt)
	}
	timeout := time.Duration(cfg.ImageGenTimeoutSec) * time.Second
	m := imagegen.SdcppModel{
		Bin:       cfg.SdcppBin,
		Model:     cfg.SdcppModel,
		ModelKind: cfg.SdcppModelKind,
		VAE:       cfg.SdcppVAE,
		ClipL:     cfg.SdcppClipL,
		ClipG:     cfg.SdcppClipG,
		T5:        cfg.SdcppT5,
		LLM:       cfg.SdcppLLM,
		Steps:     cfg.ImageGenSteps,
		CFG:       cfg.ImageGenCFG,
		Sampler:   cfg.ImageGenSampler,
		ExtraArgs: cfg.SdcppExtraArgs,
	}
	imgFamily, imgQuant := imageFootprintKey(cfg)
	// sd.cpp has no ComfyUI instance to bind to a card: the whole node, as always.
	// It declares the weights it parks itself (sdcppneed.go, F60), not only the class default: an sd.cpp-only box has none.
	grant, lerr := p.acquireMediaLease(ctx, "image-gen (sdcpp)", timeout, p.gpuWait(), p.sdcppImageNeed(cfg, imgFamily, imgQuant, paramStr(req.Params, "waiter_token")).resumableBy(req))
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer grant.Release()
	outPath, gerr := imagegen.GenerateSdcpp(ctx, imagegen.SdcppRequest{
		Node: cfg.NodePath, Script: script, Out: out, Prompt: renderPrompt,
		Params: req.Params, Model: m, Timeout: timeout,
		Sampling: p.footprintSampling(imgFamily, imgQuant, "image-gen"), ExtraEnv: grant.Env,
	})
	if gerr != nil {
		meta.ErrClass = classifyErr(gerr)
		return deferf("image generation failed: " + gerr.Error())
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	payload := imageResultPayload(outPath, req.Params, seed, fam)
	addRefineData(payload, p.cfg.ImageGenRefinerModel, refined, renderPrompt, refineNote)
	if ups != "" && !refineOff {
		addUpsampleData(payload, ups, upsampled, renderPrompt, upsampleNote)
	}
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
	// (measured 2026-07-17 in the private development record).
	if mask == "" && paramBool(req.Params, "auto_text") {
		// The detector's vqa row is an inner row of this call's row (plan D12):
		// every exit below records through meta, so the job id rides on all of them.
		meta.JobID = mintJobID("ip")
		am, aerr := p.autoTextMask(ctx, image, req.Door, meta.JobID)
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
	grant, lerr := p.acquireMediaLease(ctx, "inpaint", timeout, p.gpuWait(), singleCardNeed(p.cfg, paramStr(req.Params, "waiter_token")).declaresRAM(routeRAM(hostneed.RouteInpaint, p.cfg)).resumableBy(req))
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer grant.Release()
	// Single-card route: the device pin applies (comfyLaunch), or the instance bound to the card.
	outPath, gerr := imagegen.Inpaint(ctx, imagegen.InpaintRequest{
		Node: p.cfg.NodePath, Script: script, ComfyDir: p.cfg.ComfyDir, Out: out, Image: image, Mask: mask,
		Prompt: prompt, Params: req.Params, Model: m, Timeout: timeout,
		ExtraEnv: append(grant.Env, grant.launch(comfyLaunch(p.cfg, true)).Env()...),
	})
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
	grant, lerr := p.acquireMediaLease(ctx, "upscale", timeout, p.gpuWait(), singleCardNeed(p.cfg, paramStr(req.Params, "waiter_token")).resumableBy(req))
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer grant.Release()
	// Single-card route: the device pin applies (comfyLaunch), or the instance bound to the card.
	outPath, gerr := imagegen.Upscale(ctx, imagegen.UpscaleRequest{
		Node: p.cfg.NodePath, Script: script, ComfyDir: p.cfg.ComfyDir, Out: out, Image: image,
		Params: req.Params, Model: imagegen.UpscaleModel{Model: model}, Timeout: timeout,
		ExtraEnv: append(grant.Env, grant.launch(comfyLaunch(p.cfg, true)).Env()...),
	})
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
	// Named edit families (ADR 0058), same contract as generate_image: absent (or
	// the default's own name) = the node's default edit binding; a named overlay
	// renders under its own config copy and tags the result with its license.
	cfg, fam, ferr := p.cfg.ResolveEditFamily(paramStr(req.Params, "family"))
	if ferr != nil {
		return defer1(ferr.Error())
	}
	meta.License = fam.License
	if cfg.GenEditScript == "" || cfg.GenEditUnet == "" {
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
	is21 := cfg.GenEditFamily == config.FamilyQwenImage21
	// Multi-reference: `image` stays the edit TARGET (image_1); `images` are the
	// references after it (image_2..). The graph takes 10 in all, the target included.
	refs := imagegenStrings(req.Params["images"])
	if len(refs) > 0 && !is21 {
		return defer1(fmt.Sprintf("multi-reference edits need a %s edit family; %s takes one image (pass family, or drop images)",
			config.FamilyQwenImage21, familyLabel(fam)))
	}
	if 1+len(refs) > maxEditImages {
		return defer1(fmt.Sprintf("a %s edit takes at most %d images in all (the target plus %d references); got %d",
			config.FamilyQwenImage21, maxEditImages, maxEditImages-1, 1+len(refs)))
	}
	for _, r := range refs {
		if fi, err := os.Stat(r); err != nil || fi.IsDir() {
			return defer1("reference image not found: " + r)
		}
	}
	if paramBool(req.Params, "transparent") && !cfg.SupportsTransparentEdit() {
		return defer1(fmt.Sprintf("transparent output needs a %s edit family (the only RGBA VAE); %s renders opaque",
			config.FamilyQwenImage21, familyLabel(fam)))
	}
	if is21 && paramStr(req.Params, "preset") != "" {
		return defer1(fmt.Sprintf("preset is a Qwen-Image-Edit 2511 steps+cfg+LoRA pairing; the %s edit graph has none (its recipe is 40 steps / cfg 1 — pass steps and cfg together to change it)",
			config.FamilyQwenImage21))
	}
	script, serr := gpugen.ResolveScript(cfg.GenEditScript)
	if serr != nil {
		return defer1(serr.Error())
	}
	meta.Model = "comfyui-edit:" + cfg.GenEditUnet
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
		Unet: cfg.GenEditUnet, Preset: cfg.GenEditPreset, LoRA: cfg.GenEditLoRA,
		LoRAStrength: cfg.GenEditLoRAStrength, CLIP: cfg.GenEditCLIP, VAE: cfg.GenEditVAE,
		Steps: cfg.GenEditSteps, CFG: cfg.GenEditCFG,
		Sampler: cfg.GenEditSampler, Scheduler: cfg.GenEditScheduler,
		Megapixels:  cfg.GenEditMegapixels,
		Family:      cfg.GenEditFamily,
		Resolution:  cfg.GenEditResolution,
		CacheDevice: cfg.GenEditCacheDevice,
		Schedule:    cfg.GenEditSchedule,
		Placement:   cfg.GenEditPlacement,
		// The edit route renders on ONE card: the device pin applies.
		Launch: comfyLaunch(cfg, true),
	}
	if cfg.EditMulticard() {
		m.Launch = multicardLaunch(cfg) // two cards: no pin, no dynamic VRAM
	}
	timeout := time.Duration(cfg.GenEditTimeoutSec) * time.Second
	grant, lerr := p.acquireMediaLease(ctx, "edit", timeout, p.gpuWait(), editNeed(cfg, paramStr(req.Params, "waiter_token")).resumableBy(req))
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer grant.Release()
	m.Launch = grant.launch(m.Launch)
	outPath, gerr := imagegen.Edit(ctx, imagegen.EditRequest{
		Node: cfg.NodePath, Script: script, ComfyDir: cfg.ComfyDir, Out: out, Image: image, Prompt: prompt,
		Params: req.Params, Model: m, Timeout: timeout, ExtraEnv: grant.Env,
	})
	if gerr != nil {
		meta.ErrClass = classifyErr(gerr)
		return defer1("generative edit failed: " + gerr.Error())
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	payload := map[string]any{"image_path": outPath, "seed": seed, "family": fam.Name}
	// Measured, like generate_image: the output size follows the source (2511 canvas)
	// or the target's grid (2.1), never a value the caller could have predicted.
	if w, h := imagegen.OutputSize(outPath); w > 0 && h > 0 {
		payload["width"], payload["height"] = w, h
	}
	if len(refs) > 0 {
		payload["images"] = 1 + len(refs)
	}
	if paramBool(req.Params, "transparent") {
		payload["transparent"] = true
	}
	addLicenseData(payload, fam)
	grant.name(payload) // a card-only call says which two cards it ran on; any other answer is unchanged
	data, _ := json.Marshal(payload)
	p.record(req.Task, meta, len(prompt))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// maxEditImages is the Qwen-Image-2.1 reference limit, target included (the model's
// official limit; render/wf-qwen-image-21.mjs QWEN_IMAGE_21_MAX_REFS).
const maxEditImages = 10

// imagegenStrings reads a []string param from its Go shape or the []any a JSON
// decode produces, dropping empty entries.
func imagegenStrings(v any) []string {
	var out []string
	switch t := v.(type) {
	case []string:
		for _, s := range t {
			if s != "" {
				out = append(out, s)
			}
		}
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
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
