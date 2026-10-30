// The image and graph verbs: generate-image, inpaint, upscale, edit-image,
// run-graph and media, with the temp-file seams the inline graph uses.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediaremote"
)

// runGenerateImage handles `local-offload generate-image "<prompt>" [--negative ...]
// [--width N] [--height N] [--steps N] [--seed N] [--out path] [--json]`. The positional
// argument is the PROMPT (not stdin). It renders on the LOCAL ComfyUI for free.
func runGenerateImage(args []string) error {
	fs := flag.NewFlagSet("generate-image", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	negative := fs.String("negative", "", "negative prompt (hard exclusions, e.g. 'people, text')")
	out := fs.String("out", "", "output PNG path (default under the media dir)")
	width := fs.Int("width", 0, "image width px (default 1024)")
	height := fs.Int("height", 0, "image height px (default 1024)")
	steps := fs.Int("steps", 0, "sampler steps (default: machine binding)")
	seed := fs.Int("seed", 0, "RNG seed (default random)")
	refine := fs.Bool("refine", true, "set --refine=false (the =form is required) to render the prompt verbatim, skipping this machine's opt-in prompt refiner; on --batch it applies to every job without its own \"refine\" value (no-op unless imagegen_refiner_model is configured)")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	family := fs.String("family", "", "named image family to render with (ADR 0058; offload_status media.image_families / doctor list them); default = this machine's default binding")
	transparent := fs.Bool("transparent", false, "keep an alpha channel (RGBA PNG; qwen-image-2.1 families only — any other binding defers)")
	batchFile := fs.String("batch", "", "render a JSONL batch of jobs (one line per job: {\"prompt\":...,\"out\"?,\"negative\"?,\"width\"?,\"height\"?,\"steps\"?,\"seed\"?,\"refine\"?}): through ONE warm ComfyUI session when this machine's lane is free, else each job a ticket on the first matching lane that frees (ADR 0083)")
	resultsFlag := fs.String("results", "", "with --batch: write one JSON row per job to this file as it ends ({i,out,seed,ok,ms,error,node,family,sha256,verdict}; verdict rendered | skipped-existing | failed | queued)")
	resume := fs.Bool("resume", false, "with --batch: skip a job whose out already holds a PNG that decodes whole and has the size the job names; anything doubtful (empty, cut short, another size, missing) is rendered again")
	jobsFlag := fs.Int("jobs", 0, "with --batch: the most jobs in flight on the ticket path (default: this machine's lane plus one per roster node that renders the recipe, at most 8)")
	route := fs.String("route", "auto", "local | auto (here when this machine has the lane, derived from the files its route loads, else a fleet node from delegate_remotes) | remote (a fleet node; input files travel in a hash-checked bundle and the output is fetched back verified; ADR 0077)")
	var remotes repeatedFlag
	fs.Var(&remotes, "remote", "fleet node base URL for this call (repeatable); must be one of delegate_remotes")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "negative": true, "out": true,
		"width": true, "height": true, "steps": true, "seed": true,
		"batch": true, "family": true, "route": true, "remote": true, "results": true, "jobs": true,
	})
	_ = fs.Parse(flagArgs)
	// Boolean flags in space form ("--refine false") make the value a stray
	// argument: flag.Parse stops there, so later flags are SILENTLY dropped and
	// the stray can even become the prompt. Refuse loudly instead.
	if err := leftoverArgErr(fs, "generate-image"); err != nil {
		return err
	}

	if *batchFile != "" {
		// A batch renders ONE binding for every job: --transparent (a per-render alpha channel) has no place on it.
		if *transparent {
			return fmt.Errorf("generate-image: --transparent selects an alpha-channel render of ONE image; a batch renders its jobs opaque (render the jobs one at a time)")
		}
		if *jobsFlag < 0 {
			return fmt.Errorf("generate-image: --jobs %d: the number of jobs in flight cannot be negative (0 is the default)", *jobsFlag)
		}
		return runImageBatchVerb(fs, imageBatchArgs{
			file: *batchFile, family: *family, route: *route, results: *resultsFlag, resume: *resume, jobs: *jobsFlag,
			refine: *refine, remotes: remotes, asJSON: *asJSON, compact: *compactFlag,
		})
	}
	for _, name := range []string{"results", "resume", "jobs"} {
		set := false
		fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
		if set {
			return fmt.Errorf("generate-image: --%s belongs to --batch (jobs.jsonl), and no batch was given", name)
		}
	}

	if positional == "" || positional == "-" {
		return fmt.Errorf("generate-image requires a prompt (not stdin): local-offload generate-image \"<prompt>\" [--width 1024]")
	}

	cfg, src := loadCfgWithSource(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	params := map[string]any{}
	if *negative != "" {
		params["negative"] = *negative
	}
	if *out != "" {
		params["out"] = *out
	}
	if *width > 0 {
		params["width"] = *width
	}
	if *height > 0 {
		params["height"] = *height
	}
	if *steps > 0 {
		params["steps"] = *steps
	}
	if *seed > 0 {
		params["seed"] = *seed
	}
	// Only an EXPLICIT --refine=false reaches the pipeline (the opt-in
	// refiner's only request-level knob turns it off; the default is inert).
	if !*refine {
		params["refine"] = false
	}
	if *family != "" {
		params["family"] = *family
	}
	if *transparent {
		params["transparent"] = true
	}
	// This verb IS the wait: when every lane is busy it becomes a ticket (ADR 0083) and blocks until the first matching lane
	// that frees has rendered it, telling the operator where it stands on stderr. The first Ctrl-C cancels a ticket that has
	// sent nothing anywhere (a job already on a node cannot be recalled; run the same command to collect it); the second
	// interrupt is the default one.
	ictx, stopInterrupt := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopInterrupt()
	go func() {
		<-ictx.Done()
		stopInterrupt()
	}()
	wait := mediaremote.Wait{Window: -1, Notify: func(line string) { fmt.Fprintln(os.Stderr, line) }}
	if src.Loaded() {
		wait.RosterFile = src.Path
	}
	res := mediaremote.RunWith(ictx, cfg, p, core.Request{
		Task:   core.TaskGenerateImage,
		Door:   "cli:generate-image",
		Input:  positional,
		Params: params,
	}, *route, remotes, wait)
	emitResult(res, *asJSON, "", *compactFlag)
	return nil
}

// runInpaintImage handles `local-offload inpaint-image <image> --mask <mask.png>
// --prompt "<text>" [--negative ...] [--denoise F] [--grow-mask N] [--steps N]
// [--seed N] [--out path] [--json] [--compact]`. The positional argument is the
// IMAGE PATH; the mask is a white-on-black image the same size (white = repaint).
// It re-renders ONLY the masked region on the LOCAL ComfyUI for free (SDXL-class
// inpaint_* binding required).
func runInpaintImage(args []string) error {
	fs := flag.NewFlagSet("inpaint-image", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	mask := fs.String("mask", "", "white-on-black mask image path (white = repaint); required unless --auto-text")
	autoText := fs.Bool("auto-text", false, "EXPERIMENTAL: replace --mask with a vision-detected mask of rendered-text regions (defers when detection is unreliable)")
	prompt := fs.String("prompt", "", "what the masked region should become; required")
	negative := fs.String("negative", "", "hard exclusions for the repainted region")
	denoise := fs.Float64("denoise", 0, "0-1; default 1.0 (full re-imagination inside the mask)")
	growMask := fs.Int("grow-mask", -1, "expand+feather the mask by N px in latent space (default 16; 0 = no dilation)")
	steps := fs.Int("steps", 0, "sampler steps (default: machine binding)")
	seed := fs.Int("seed", 0, "RNG seed (default random)")
	out := fs.String("out", "", "output PNG path (default under the media dir)")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "mask": true, "prompt": true, "negative": true,
		"denoise": true, "grow-mask": true, "steps": true, "seed": true, "out": true,
	})
	_ = fs.Parse(flagArgs)

	if positional == "" || positional == "-" {
		return fmt.Errorf("inpaint-image requires an image path (not stdin): local-offload inpaint-image <image> --mask m.png --prompt \"...\"")
	}
	if (*mask == "" && !*autoText) || *prompt == "" {
		return fmt.Errorf("inpaint-image requires --mask (or --auto-text) and --prompt: local-offload inpaint-image <image> --mask m.png --prompt \"...\"")
	}

	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	params := map[string]any{"image": positional}
	if *mask != "" {
		params["mask"] = *mask
	}
	if *autoText {
		params["auto_text"] = true
	}
	if *negative != "" {
		params["negative"] = *negative
	}
	if *denoise > 0 {
		params["denoise"] = *denoise
	}
	if *growMask >= 0 {
		params["grow_mask"] = *growMask
	}
	if *steps > 0 {
		params["steps"] = *steps
	}
	if *seed > 0 {
		params["seed"] = *seed
	}
	if *out != "" {
		params["out"] = *out
	}
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskInpaintImage,
		Door:   "cli:inpaint-image",
		Input:  *prompt,
		Params: params,
	})
	emitResult(res, *asJSON, "", *compactFlag)
	return nil
}

// runUpscaleImage handles `local-offload upscale-image <image> [--scale F]
// [--width N --height N] [--method m] [--model name] [--out path] [--json]
// [--compact]`. The positional argument is the IMAGE PATH. It enlarges the image
// with this machine's ESRGAN-family upscale_model on the LOCAL ComfyUI for free.
func runUpscaleImage(args []string) error {
	fs := flag.NewFlagSet("upscale-image", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	scale := fs.Float64("scale", 0, "overall factor relative to the source, made exact by measuring it (0 = unset: the model's own factor)")
	width := fs.Int("width", 0, "exact output width (give with --height; wins over --scale)")
	height := fs.Int("height", 0, "exact output height (give with --width)")
	method := fs.String("method", "", "resampler for the scale/size step: lanczos (default) | bicubic | bilinear | area | nearest-exact")
	model := fs.String("model", "", "override this machine's upscale_model (ComfyUI upscale_models filename)")
	out := fs.String("out", "", "output PNG path (default under the media dir)")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "scale": true, "width": true, "height": true, "method": true, "model": true, "out": true,
	})
	_ = fs.Parse(flagArgs)

	if positional == "" || positional == "-" {
		return fmt.Errorf("upscale-image requires an image path (not stdin): local-offload upscale-image <image> [--scale 2]")
	}

	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	params := map[string]any{"image": positional}
	if *scale != 0 {
		params["scale"] = *scale
	}
	if *width != 0 {
		params["width"] = *width
	}
	if *height != 0 {
		params["height"] = *height
	}
	if *method != "" {
		params["method"] = *method
	}
	if *model != "" {
		params["model"] = *model
	}
	if *out != "" {
		params["out"] = *out
	}
	res := p.Run(context.Background(), core.Request{Task: core.TaskUpscaleImage, Door: "cli:upscale-image", Params: params})
	emitResult(res, *asJSON, "", *compactFlag)
	return nil
}

// runRunGraph handles `local-offload run-graph --graph <g.json> [--graph-json '<json>']
// [--manifest <m.json>] [--manifest-json '<json>'] [--out-dir <d>] [--reserve-vram F]
// [--json] [--compact]`. It executes an arbitrary ComfyUI API-format graph on the LOCAL
// ComfyUI, satisfying its per-workflow node manifest first — generic, the caller owns all
// graph semantics. Any failure defers (never cloud).
func runRunGraph(args []string) error {
	fs := flag.NewFlagSet("run-graph", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	// graph/manifest/out-dir/reserve-vram are owned by runGraphParams (the testable arg
	// seam); register them here too so this parser accepts them without erroring.
	fs.String("graph", "", "path to a ComfyUI API-format graph JSON (required unless --graph-json)")
	fs.String("graph-json", "", "inline ComfyUI API-format graph JSON (alternative to --graph)")
	fs.String("manifest", "", "path to a node manifest JSON")
	fs.String("manifest-json", "", "inline node manifest JSON (alternative to --manifest)")
	fs.String("out-dir", "", "directory for output files (default under the media dir)")
	fs.String("reserve-vram", "", "ComfyUI --reserve-vram override")
	fs.String("min-vram-gb", "", "only a fleet node whose largest card is at least N GB may take the graph (0 lifts the default floor)")
	fs.String("model-family", "", "the family this graph belongs to (wan22 for Wan 2.2); only the default --min-vram-gb floor reads it")
	route := fs.String("route", "auto", "local | auto (here when this machine has the lane, derived from the files its route loads, else a fleet node from delegate_remotes) | remote (a fleet node; input files travel in a hash-checked bundle and the output is fetched back verified; ADR 0077)")
	var remotes repeatedFlag
	fs.Var(&remotes, "remote", "fleet node base URL for this call (repeatable); must be one of delegate_remotes")
	_ = fs.Parse(args)

	params, cleanupInline, err := runGraphParams(args)
	if err != nil {
		return err
	}
	// The temp files an inline --graph-json or --manifest-json became are this call's own, and go when it ends, whichever way it ends:
	// the pipeline that cannot open (below), an answer, a deferral. Registered before anything else can return. A graph ticket this
	// call made is not left holding them: it keeps its own copy of the bytes (internal/mediaremote/ticket.go writeBodies).
	defer cleanupInline()

	cfg, src := loadCfgWithSource(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	// This verb IS the wait, as generate-image's is (ADR 0083 Amendment 1): when this machine's lane is busy the graph becomes a
	// ticket and the verb blocks until the first matching lane that frees has run it, telling the operator where it stands on
	// stderr. The first Ctrl-C cancels a ticket that has sent nothing anywhere (a graph already on a node cannot be recalled; run
	// the same command to collect it); the second interrupt is the default one.
	ictx, stopInterrupt := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopInterrupt()
	go func() {
		<-ictx.Done()
		stopInterrupt()
	}()
	wait := mediaremote.Wait{Window: -1, Notify: func(line string) { fmt.Fprintln(os.Stderr, line) }}
	if src.Loaded() {
		wait.RosterFile = src.Path
	}
	res := mediaremote.RunWith(ictx, cfg, p, core.Request{Task: core.TaskRunGraph, Door: "cli:run-graph", Params: params}, *route, remotes, wait)
	emitResult(res, *asJSON, "", *compactFlag)
	return nil
}

// runGraphParams parses the run-graph flags into the pipeline request params map. It is
// the testable validation seam: --graph OR --graph-json is required (else an error), and
// inline JSON is materialized to a temp file whose path is threaded on (the runner reads
// files). Kept independent of runRunGraph's pipeline wiring so the arg contract is unit-
// testable without a ComfyUI/pipeline.
//
// The returned cleanup is never nil and removes the temp files THIS call made for inline JSON (a file
// the caller named with --graph or --manifest is never touched); on an error there is nothing to clean,
// because a half-made pair is removed before the error returns.
func runGraphParams(args []string) (map[string]any, func(), error) {
	noop := func() {}
	fs := flag.NewFlagSet("run-graph-params", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.String("config", "", "config file path")
	fs.Bool("json", false, "")
	fs.Bool("compact", false, "")
	graph := fs.String("graph", "", "")
	graphJSON := fs.String("graph-json", "", "")
	manifest := fs.String("manifest", "", "")
	manifestJSON := fs.String("manifest-json", "", "")
	outDir := fs.String("out-dir", "", "")
	reserve := fs.String("reserve-vram", "", "")
	minVRAM := fs.String("min-vram-gb", "", "")
	family := fs.String("model-family", "", "")
	fs.String("route", "", "")
	fs.Var(new(repeatedFlag), "remote", "")
	if err := fs.Parse(args); err != nil {
		return nil, noop, err
	}
	if *graph == "" && *graphJSON == "" {
		return nil, noop, fmt.Errorf("run-graph: --graph or --graph-json required")
	}
	var floor *float64
	if s := strings.TrimSpace(*minVRAM); s != "" {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, noop, fmt.Errorf("run-graph: --min-vram-gb must be a number of GB, 0 or more; got %q", s)
		}
		floor = &v
	}
	graphPath, cleanGraph, err := materializeArg(*graph, *graphJSON, "run-graph-*.json")
	if err != nil {
		return nil, noop, fmt.Errorf("graph: %w", err)
	}
	manifestPath, cleanManifest, err := materializeArg(*manifest, *manifestJSON, "run-graph-manifest-*.json")
	if err != nil {
		cleanGraph() // the graph's file was made for a call that now ends
		return nil, noop, fmt.Errorf("manifest: %w", err)
	}
	cleanup := func() {
		cleanGraph()
		cleanManifest()
	}
	params := map[string]any{
		"graph_path":    graphPath,
		"manifest_path": manifestPath,
		"out_dir":       *outDir,
		"reserve_vram":  *reserve,
	}
	if floor != nil {
		params["min_vram_gb"] = *floor
	}
	if f := strings.TrimSpace(*family); f != "" {
		params["model_family"] = f
	}
	return params, cleanup, nil
}

// createTemp makes the temp file an inline graph or manifest is written to; a seam, so that a test can make the write fail.
var createTemp = os.CreateTemp

// removeFile, removeAttempts and removePause are how the temp files an inline graph or manifest became are deleted, and tempWarnings
// is where one that cannot be is named; seams, so that a test can make a delete fail the way a file held for a moment fails on Windows.
var (
	removeFile               = os.Remove
	removeAttempts           = 20
	removePause              = 25 * time.Millisecond
	tempWarnings   io.Writer = os.Stderr
)

// removeInlineTemp deletes a temp file this call made. A scanner that holds the file for a moment, or a runner that has only just
// exited, makes the delete fail with a sharing violation on Windows, so it is tried again for about half a second, as
// pipeline.removeRunGraphEnvelope does for the envelope; a file that is not there is not a failure. One that still cannot be removed is
// named on stderr, so that the leak this cleanup exists to stop is never silent.
func removeInlineTemp(name string) {
	var err error
	for i := 0; i < removeAttempts; i++ {
		if i > 0 {
			time.Sleep(removePause)
		}
		if err = removeFile(name); err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
	}
	fmt.Fprintf(tempWarnings, "local-offload: run-graph: could not remove the temp file %s (%v); it stays in the OS temp dir until someone deletes it\n", name, err)
}

// materializeArg returns path if set, else writes inline json to a temp file and returns
// that path (the runner reads files, not inline). Empty+empty → no path and no error.
//
// The cleanup it returns is never nil and removes only a file THIS call made: it does nothing for a path
// the caller named, nor for an empty one. A write that fails removes its own file before the error returns.
func materializeArg(path, inline, pattern string) (string, func(), error) {
	noop := func() {}
	if path != "" {
		return path, noop, nil
	}
	if inline == "" {
		return "", noop, nil
	}
	f, err := createTemp("", pattern)
	if err != nil {
		return "", noop, err
	}
	name := f.Name()
	_, werr := f.WriteString(inline)
	cerr := f.Close() // closed before any removal: Windows will not delete a file that is still open
	if werr == nil {
		werr = cerr
	}
	if werr != nil {
		removeInlineTemp(name)
		return "", noop, werr
	}
	return name, func() { removeInlineTemp(name) }, nil
}

// runGenerateSVG handles `local-offload generate-svg <kind> --spec '<json>' [--spec-file path] [--out file.svg] [--json]`.
// kind is the positional (gauge|comparison-bar|chromatogram|icon). It renders locally for free (no model/GPU).
// runEditImage handles `local-offload edit-image <image> --ops '<json array>'
// [--out path] [--json]`. Deterministic CPU pipeline (PIL + GIMP flatten_design)
// via internal/mediaops — no GPU lock.
func runEditImage(args []string) error {
	fs := flag.NewFlagSet("edit-image", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	opsStr := fs.String("ops", "", `edit ops as a JSON array, e.g. '[{"op":"crop","x":0,"y":0,"width":100,"height":100},{"op":"resize","width":512}]'`)
	opsFile := fs.String("ops-file", "", "path to a JSON file with the ops array")
	out := fs.String("out", "", "output image path (default under the media dir)")
	renditions := fs.String("renditions", "", `export matrix from the master out, e.g. '[{"width":1080,"format":"webp","suffix":"-ig"},{"width":1920,"format":"jpg","suffix":"-web"}]'`)
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "ops": true, "ops-file": true, "out": true, "renditions": true,
	})
	_ = fs.Parse(flagArgs)
	if positional == "" || positional == "-" {
		return fmt.Errorf("edit-image requires an input image: local-offload edit-image <image> --ops '<json array>'")
	}
	raw := *opsStr
	if raw == "" && *opsFile != "" {
		b, rerr := os.ReadFile(*opsFile)
		if rerr != nil {
			return rerr
		}
		raw = string(b)
	}
	if raw == "" {
		return fmt.Errorf("edit-image: --ops (or --ops-file) is required")
	}
	var ops []map[string]any
	if jerr := json.Unmarshal([]byte(raw), &ops); jerr != nil {
		return fmt.Errorf("edit-image: invalid --ops JSON: %w", jerr)
	}
	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	params := map[string]any{"ops": ops}
	if *out != "" {
		params["out"] = *out
	}
	if *renditions != "" {
		var rends []map[string]any
		if jerr := json.Unmarshal([]byte(*renditions), &rends); jerr != nil {
			return fmt.Errorf("edit-image: invalid --renditions JSON: %w", jerr)
		}
		params["renditions"] = rends
	}
	res := p.Run(context.Background(), core.Request{Task: core.TaskEditImage, Door: "cli:edit-image", Image: positional, Params: params})
	emitResult(res, *asJSON, "", *compactFlag)
	return nil
}

// runMedia handles `local-offload media <op> [--in x] [--inputs a,b] [--out y]
// [op flags] [--json]`. One ffmpeg operation via internal/mediaops — no GPU lock.
func runMedia(args []string) error {
	fs := flag.NewFlagSet("media", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	in := fs.String("in", "", "input media path")
	inputs := fs.String("inputs", "", "concat: comma-separated input paths (>=2)")
	out := fs.String("out", "", "output path (extract_frames: a directory; default under the media dir)")
	start := fs.String("start", "", "trim: start (seconds or hh:mm:ss)")
	end := fs.String("end", "", "trim: absolute end time")
	duration := fs.String("duration", "", "trim: duration seconds (alternative to --end)")
	reencode := fs.Bool("reencode", false, "trim: re-encode for exact cuts (default: keyframe-snapped stream copy)")
	fps := fs.Float64("fps", 0, "extract_frames: sampling rate")
	count := fs.Int("count", 0, "extract_frames: total frames (resolved via probe)")
	audio := fs.String("audio", "", "mux_audio: audio input path")
	audioOnly := fs.Bool("audio-only", false, "convert: drop video")
	videoOnly := fs.Bool("video-only", false, "convert: drop audio")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "in": true, "inputs": true, "out": true, "start": true,
		"end": true, "duration": true, "fps": true, "count": true, "audio": true,
	})
	_ = fs.Parse(flagArgs)
	if positional == "" || positional == "-" {
		return fmt.Errorf("media requires an op: local-offload media <trim|concat|extract_frames|convert|mux_audio|probe> --in <path>")
	}
	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	params := map[string]any{"op": positional}
	for k, v := range map[string]string{"in": *in, "out": *out, "start": *start, "end": *end, "duration": *duration, "audio": *audio} {
		if v != "" {
			params[k] = v
		}
	}
	if *inputs != "" {
		params["inputs"] = strings.Split(*inputs, ",")
	}
	if *reencode {
		params["reencode"] = true
	}
	if *audioOnly {
		params["audio_only"] = true
	}
	if *videoOnly {
		params["video_only"] = true
	}
	if *fps > 0 {
		params["fps"] = *fps
	}
	if *count > 0 {
		params["count"] = *count
	}
	res := p.Run(context.Background(), core.Request{Task: core.TaskMedia, Door: "cli:media", Params: params})
	emitResult(res, *asJSON, "", *compactFlag)
	return nil
}
