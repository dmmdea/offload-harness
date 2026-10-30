// Video doors: generate-video and animate-character, plus the video family binding and footprint labels.

package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpugen"
	"github.com/dmmdea/offload-harness/internal/hostneed"
	"github.com/dmmdea/offload-harness/internal/tasks"
)

// runGenerateVideo animates req.Image (a still) into a short clip on the LOCAL ComfyUI by
// shelling out (via internal/gpugen) to comfy-video.mjs, which holds the shared GPU lock,
// runs the ComfyUI lifecycle, and is now process-tree-killed on timeout. Its own branch —
// no text models, no grammar. Any failure (no route, empty prompt, render error, timeout)
// defers to Claude. params: still (string image path), model (hunyuan|wan), frames/width/
// height/steps/seed (int), negative (string), reserve_vram (float, per-workflow override).
func (p *Pipeline) runGenerateVideo(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	// CT-49: a family bound to the sdcpp engine renders through render/sdcpp-video.mjs
	// (igpumedia.go) and never reaches ComfyUI. The lookup is pure, and no family binds
	// an engine unless the operator set one, so every other box falls through unchanged.
	if renderFamily, fb, ok := p.sdcppVideoBinding(req); ok {
		return p.runGenerateVideoSdcpp(ctx, req, meta, start, renderFamily, fb)
	}
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
	// The per-family weight/behavior binding for whatever family ACTUALLY
	// renders. An explicit videogen_families[renderFamily] entry, when bound,
	// wins outright for a family other than this box's own default; absent one
	// (including the box's own default family), it falls back to the flat
	// videogen_* keys unchanged — back-compat, since those keys may
	// intentionally carry a different family's weights as a fallback (e.g. Wan
	// GGUF files bound here on an ltx25-seated box for a bare `model:"wan"`
	// override). See gap: before videogen_families existed there was no way to
	// scope a leak away — an ltx25-bound box's text encoder could reach an
	// overridden Wan render with no opt-out at all (bigger-models-2026-09-24.md).
	fb := p.cfg.ResolveVideoFamilyBinding(renderFamily)
	// The family's license rides on the result and the ledger row exactly as an image
	// family's does (ADR 0058; CT-47). It is the RENDERING family's, so an override
	// request is tagged with the overridden family's license, not the seat's. Set here,
	// before any defer, so a deferred render's row names it too.
	meta.License = fb.License

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
	// default (map lookup -> 0 -> unaffected). Sourced from fb (the family that
	// ACTUALLY renders), not the flat keys directly, so an explicit family
	// override never inherits another family's resolution/frame defaults either.
	machineDefault := map[string]int{"width": fb.Width, "height": fb.Height, "frames": fb.Frames}
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
	if fb.UnetHigh != "" {
		args = append(args, "--high-unet", fb.UnetHigh)
	}
	if fb.UnetLow != "" {
		args = append(args, "--low-unet", fb.UnetLow)
	}
	if fb.TextEncoder != "" {
		args = append(args, "--text-encoder", fb.TextEncoder)
	}
	// This card's DisTorch2 split for the Wan experts (GiB of each parked in RAM). A
	// per-card measurement, so it is config, never a constant in the graph builder; only
	// the Wan graph reads it. 0/negative passes nothing: the builder keeps its default.
	if fb.WanVirtualVramGB > 0 {
		args = append(args, "--wan-vvram-gb", strconv.FormatFloat(fb.WanVirtualVramGB, 'f', -1, 64))
	}
	// videogen_wan_loader (this box's, or the resolved family's, override): auto
	// (unset) lets the builder decide per expert file by extension, unchanged.
	if fb.WanLoader != "" {
		args = append(args, "--wan-loader", fb.WanLoader)
	}
	// videogen_wan_decode (this box's, or the resolved family's, override): plain, tiled, or auto,
	// which the runner decides from the render card's size. Unset passes nothing and the runner
	// defaults to tiled. Only the Wan graph reads it; like the two Wan flags above it is passed
	// whatever the family, and the other builders ignore it.
	if fb.WanDecode != "" {
		args = append(args, "--wan-decode", fb.WanDecode)
	}
	// videogen_placement multicard (the Wan 2.2 and LTX-2.5 graphs only): the card-only placement on the lease's two cards, no host RAM. The runner owns the
	// placement and its ComfyUI instance; --wan-resident-gb is the GiB of each expert (of the DiT, for LTX-2.5) kept on the compute card (unset = the runner's default).
	wanMulticard := cardOnlyVideo(fb, renderFamily)
	if wanMulticard {
		args = append(args, "--wan-placement", config.PlacementMulticard)
		if fb.WanResidentGB > 0 {
			args = append(args, "--wan-resident-gb", strconv.FormatFloat(fb.WanResidentGB, 'f', -1, 64))
		}
	}
	// LTX-2.5 family bindings (quality-first weight binding, same pattern as the
	// Wan flags above): filenames + fps + the pooled-DiT placement from config.
	// Gap 5: an explicit per-request `transformer` param wins over this box's
	// (or the resolved family's) bound file — a caller can pick e.g. the bf16
	// LTX transformer for one hero render without a config edit (the study's
	// recommendation: int8 default, bf16 opt-in). Harmless on any other family's
	// graph, which never reads --transformer.
	transformer := fb.Transformer
	if t := paramStr(req.Params, "transformer"); t != "" {
		transformer = t
	}
	if transformer != "" {
		args = append(args, "--transformer", transformer)
	}
	if fb.VideoVAE != "" {
		args = append(args, "--video-vae", fb.VideoVAE)
	}
	if fb.AudioVAE != "" {
		args = append(args, "--audio-vae", fb.AudioVAE)
	}
	if fb.LatentUpscaler != "" {
		args = append(args, "--latent-upscaler", fb.LatentUpscaler)
	}
	if fb.FPS > 0 {
		args = append(args, "--fps", strconv.Itoa(fb.FPS))
	}
	if fb.PoolVvramGB > 0 && !ltxMulticardVideo(fb, renderFamily) { // the card-only placement is the other placement of the same DiT: the runner refuses both
		args = append(args, "--pool-vvram-gb", strconv.FormatFloat(fb.PoolVvramGB, 'f', -1, 64))
		if fb.PoolCompute != "" {
			args = append(args, "--pool-compute", fb.PoolCompute)
		}
		if fb.PoolDonor != "" {
			args = append(args, "--pool-donor", fb.PoolDonor)
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
	if paramBool(req.Params, "upscale") && fb.UpscaleModel != "" {
		args = append(args, "--upscale-model", fb.UpscaleModel)
		if fb.UpscaleWidth > 0 && fb.UpscaleHeight > 0 {
			args = append(args, "--upscale-width", strconv.Itoa(fb.UpscaleWidth), "--upscale-height", strconv.Itoa(fb.UpscaleHeight))
		}
	}

	timeout := time.Duration(p.cfg.VideoGenTimeoutSec) * time.Second
	// The host RAM this call declares is sized from the files the runner will load: the family's binding
	// with the request's own `transformer` over it (the runner was just handed that file, above).
	over := hostneed.VideoOverrides{Transformer: paramStr(req.Params, "transformer")}
	grant, lerr := p.acquireMediaLease(ctx, "video-gen", timeout, p.gpuWait(), videoNeed(p.cfg, paramStr(req.Params, "waiter_token"), renderFamily, over).resumableBy(req))
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer grant.Release()
	// COMFY_WAIT_SEC aligns the render script's poll budget with the harness timeout
	// (quality-first: the native recipe at 720p legitimately exceeds the script's old
	// hardcoded ceiling; the Go timeout stays the hard stop).
	// The device pin applies when NOT pooled (see comfyLaunch's doc comment): a pooled
	// video seat is placed by its pool keys, and the 3x16 pool must compute on
	// ComfyUI's default device (MultiGPU #220) — an un-pooled seat has no pool keys to
	// place it and must not silently land on ComfyUI's default device instead, which
	// on this box's enumeration is the display card.
	var env []string
	if wanMulticard {
		// two cards: no pin, no dynamic VRAM (the runner drops both for the launch anyway); the operator's extra args stay
		env = append(append(p.genEnv(), multicardLaunch(p.cfg).Env()...), grant.Env...)
	} else {
		env = append(p.comfyGenEnvFor(grant, !p.cfg.VideoPooled()), grant.Env...)
	}
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
	p.footprintSampling(videoFootprintFamily(renderFamily), videoFootprintQuant(fb, renderFamily), "video-gen").ApplyTo(&spec)
	outPath, gerr := gpugen.Generate(ctx, spec)
	if gerr != nil {
		meta.ErrClass = gpugen.ClassifyErr(gerr)
		return p.deferGen(req, meta, start, len(req.Input), "video generation failed: "+gerr.Error())
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	payload := map[string]any{"video_path": outPath, "seed": seed}
	addLicenseData(payload, config.FamilyInfo{License: fb.License, CommercialUse: fb.CommercialUse})
	data, _ := json.Marshal(payload)
	p.record(req.Task, meta, len(prompt))
	return core.Result{OK: true, Data: data, Meta: meta}
}

// runAnimateCharacter retargets the motion of a driver video (req.Video / params.driver)
// onto a reference character image (req.Image / params.ref) on the LOCAL ComfyUI by
// shelling out (via internal/gpugen) to comfy-animate.mjs — WAN-Animate-2 distilled,
// single 81-frame Motion Transfer chunk, pose cache pinned cpu (the template's gpu/int8
// cache hard-kills ComfyUI; see the T4 closeout in docs/ROADMAP.md). Its own branch —
// no text models, no grammar. Any failure defers. params: ref (string image path),
// driver (string video path), motion_prompt/negative (string), width/height/frames/
// steps/seed (int), pose_strength/ref_strength (float), reserve_vram (float).
func (p *Pipeline) runAnimateCharacter(ctx context.Context, req core.Request, meta core.Meta, start time.Time) core.Result {
	// CT-49: animategen_engine "sdcpp" = ffmpeg frames + depth-anything.cpp + sd.cpp VACE.
	if p.cfg.AnimateGenEngine == config.EngineSdcpp {
		return p.runAnimateCharacterSdcpp(ctx, req, meta, start)
	}
	if p.cfg.AnimateGenScript == "" {
		return p.deferGen(req, meta, start, len(req.Input), "no animate route configured")
	}
	prompt := strings.TrimSpace(req.Input)
	if prompt == "" {
		return p.deferGen(req, meta, start, len(req.Input), "empty character/background prompt")
	}
	ref := paramStr(req.Params, "ref")
	if ref == "" {
		ref = req.Image
	}
	driver := paramStr(req.Params, "driver")
	if driver == "" {
		driver = req.Video
	}
	if ref == "" || driver == "" {
		return p.deferGen(req, meta, start, len(req.Input), "animate needs both a reference image and a driver video")
	}
	script, serr := gpugen.ResolveScript(p.cfg.AnimateGenScript)
	if serr != nil {
		return p.deferGen(req, meta, start, len(req.Input), serr.Error())
	}
	meta.Model = "comfyui-animate:wan-animate2"

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
		out = filepath.Join(p.cfg.MediaDir, "animate-"+sha256hex(prompt + tasks.StableParamsKey(req.Params))[:8]+".mp4")
	}

	// comfy-animate.mjs CLI: <out> <ref> <driver> "<prompt>" [flags]
	args := []string{out, ref, driver, prompt}
	if mp := paramStr(req.Params, "motion_prompt"); mp != "" {
		args = append(args, "--motion-prompt", mp)
	}
	if n := paramStr(req.Params, "negative"); n != "" {
		args = append(args, "--negative", n)
	}
	// Per-machine resolution defaults; a per-request value wins; 0 config default
	// means "use the builder's 482x854 template default". frames/steps/seed have
	// no machine default (map lookup -> 0 -> unaffected).
	machineDefault := map[string]int{"width": p.cfg.AnimateGenWidth, "height": p.cfg.AnimateGenHeight}
	for _, k := range []string{"width", "height", "frames", "steps", "seed"} {
		v := paramIntOr(req.Params, k, 0)
		if v <= 0 {
			v = machineDefault[k]
		}
		if v > 0 {
			args = append(args, "--"+k, strconv.Itoa(v))
		}
	}
	for _, k := range []string{"pose_strength", "ref_strength"} {
		if v := paramStr(req.Params, k); v != "" {
			args = append(args, "--"+strings.ReplaceAll(k, "_", "-"), v)
		}
	}
	if rv := paramStr(req.Params, "reserve_vram"); rv != "" {
		args = append(args, "--reserve-vram", rv)
	}
	// Per-machine WAN-Animate-2 weight binding (quality-first, same pattern as the
	// videogen flags). Unset = the render script's defaults (unchanged).
	if p.cfg.AnimateGenUnet != "" {
		args = append(args, "--unet", p.cfg.AnimateGenUnet)
	}
	if p.cfg.AnimateGenTextEncoder != "" {
		args = append(args, "--text-encoder", p.cfg.AnimateGenTextEncoder)
	}
	if p.cfg.AnimateGenClipVision != "" {
		args = append(args, "--clip-vision", p.cfg.AnimateGenClipVision)
	}
	if p.cfg.AnimateGenVAE != "" {
		args = append(args, "--vae", p.cfg.AnimateGenVAE)
	}

	timeout := time.Duration(p.cfg.AnimateGenTimeoutSec) * time.Second
	grant, lerr := p.acquireMediaLease(ctx, "animate", timeout, p.gpuWait(), singleCardNeed(p.cfg, paramStr(req.Params, "waiter_token")).declaresRAM(routeRAM(hostneed.RouteAnimate, p.cfg)).resumableBy(req))
	if lerr != nil {
		return p.deferForLease(lerr, req.Task, meta, len(req.Input), start)
	}
	defer grant.Release()
	// Single-card route: the device pin applies (comfyLaunch), or the instance bound to the card.
	env := append(p.comfyGenEnvFor(grant, true), grant.Env...)
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
	// Passive fleet footprint under the route's own family key (int8 is the only
	// shipped WAN-Animate-2 variant; the quant slot follows the videogen rule of
	// reporting a quant only where it names a real recipe axis).
	p.footprintSampling("wan-animate2", "", "animate").ApplyTo(&spec)
	outPath, gerr := gpugen.Generate(ctx, spec)
	if gerr != nil {
		meta.ErrClass = gpugen.ClassifyErr(gerr)
		return p.deferGen(req, meta, start, len(req.Input), "character animation failed: "+gerr.Error())
	}
	meta.LatencyMs = time.Since(start).Milliseconds()
	data, _ := json.Marshal(map[string]any{"video_path": outPath, "seed": seed})
	p.record(req.Task, meta, len(prompt))
	return core.Result{OK: true, Data: data, Meta: meta}
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
		// An sdcpp family (CT-49) has no runner-dispatch literal: it is matched by its own
		// name, exactly, and renders under that name.
		if cfg.SdcppVideoFamily(req) {
			return req, req
		}
		// An explicit request is passed through verbatim (the runner owns the
		// arg namespace, including values this function has never heard of), but
		// provenance is canonicalised.
		return req, canonicalVideoFamily(req)
	}
	if fam := strings.TrimSpace(cfg.VideoGenFamily); fam != "" {
		if cfg.SdcppVideoFamily(fam) {
			return fam, fam
		}
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
	"h3":      "h3",
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

// videoFootprintQuant reports "q8_0" when the EFFECTIVE Wan expert weights for
// the family that actually rendered (fb — the same per-family binding
// runGenerateVideo resolved via ResolveVideoFamilyBinding, never the box's flat
// keys directly) are the Q8_0 GGUFs, else "" (node default — fp8_scaled/fp16
// bindings and the script's own defaults).
//
// It is scoped to the Wan family ON PURPOSE. The Wan GGUF keys stay bound on a
// box whose seat is another family (they are the recorded fallback), so before
// 0.73.1 this returned "q8_0" for LTX-2.5 renders whose transformer is
// int8-convrot — a quant the render never used, stamped on a store the fleet
// reads. Other families carry their quant in their own weight filenames; until a
// family declares one, "" (unknown) is the honest answer.
//
// Taking fb (not cfg) matters once videogen_families is in play: an operator
// binding videogen_families["wan22"].unet_high on a box whose OWN default
// family is something else must have THAT override reflected here — reading
// the box's flat cfg.VideoGenUnetHigh/Low directly would report a quant that
// does not match what the render actually used (the exact cross-family leak
// class this PR's gap 1 fix exists to close everywhere else).
func videoFootprintQuant(fb config.VideoFamilyBinding, renderFamily string) string {
	// Gate on the CONFIG-namespace family, not on videoFootprintFamily's return:
	// that helper now yields "wan2.2", so comparing it to the "wan22" sentinel
	// would be false for every input and silently suppress the quant everywhere.
	if renderFamily != "" && renderFamily != videoFamilyWanSentinel {
		return ""
	}
	if strings.Contains(strings.ToUpper(fb.UnetHigh+fb.UnetLow), "Q8_0") {
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
