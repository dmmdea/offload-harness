// The video verbs: compose-video, generate-video and animate-character, with
// their flag parsers and parameter builders.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/composeremote"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediaremote"
)

// composeFlags is the parsed CLI input for `compose-video`. Factored out so the
// param building (buildComposeParams) is unit-testable without a render.
type composeFlags struct {
	template, variables, variablesFile, htmlFile, projectDir, composition string
	out, format, quality, resolution, snapshots                           string
	fps, workers                                                          int
	strict                                                                bool
}

// buildComposeParams maps `compose-video` flags onto the params offload_compose_video
// sends, so the CLI and the MCP door hand the pipeline the same shapes. --html reads
// the file: the pipeline's html param is the page itself, never a path.
func buildComposeParams(f composeFlags) (map[string]any, error) {
	params := map[string]any{}
	for k, v := range map[string]string{"template": f.template, "project_dir": f.projectDir, "composition": f.composition,
		"out": f.out, "format": f.format, "quality": f.quality, "resolution": f.resolution} {
		if v != "" {
			params[k] = v
		}
	}
	if f.htmlFile != "" {
		b, err := os.ReadFile(f.htmlFile)
		if err != nil {
			return nil, fmt.Errorf("compose-video: --html: %w", err)
		}
		params["html"] = string(b)
	}
	vars := f.variables
	if vars != "" && f.variablesFile != "" {
		return nil, fmt.Errorf("compose-video: use --variables or --variables-file, not both")
	}
	if f.variablesFile != "" {
		b, err := os.ReadFile(f.variablesFile)
		if err != nil {
			return nil, fmt.Errorf("compose-video: --variables-file: %w", err)
		}
		vars = string(b)
	}
	if vars != "" {
		var obj map[string]any
		if err := json.Unmarshal([]byte(vars), &obj); err != nil || obj == nil {
			return nil, fmt.Errorf("compose-video: variables must be a JSON object")
		}
		params["variables"] = obj
	}
	if f.fps > 0 {
		params["fps"] = f.fps
	}
	if f.workers > 0 {
		params["workers"] = f.workers
	}
	if !f.strict {
		params["strict"] = false
	}
	if f.snapshots != "" {
		var ts []float64
		for _, s := range strings.Split(f.snapshots, ",") {
			v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil {
				return nil, fmt.Errorf("compose-video: --snapshots %q: %w", f.snapshots, err)
			}
			ts = append(ts, v)
		}
		params["snapshots"] = ts
	}
	return params, nil
}

// runComposeVideo is `local-offload compose-video` — the CLI door of the composition
// lane (ADR 0059), the same pipeline path offload_compose_video takes.
func runComposeVideo(args []string) error {
	fs := flag.NewFlagSet("compose-video", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	var f composeFlags
	fs.StringVar(&f.template, "template", "", "a vetted template on this machine (title-card, lower-third, stat-card, section-title, callout-label, checklist-card, captions-bar)")
	fs.StringVar(&f.variables, "variables", "", "template variables as a JSON object")
	fs.StringVar(&f.variablesFile, "variables-file", "", "path to a JSON file with the template variables")
	fs.StringVar(&f.htmlFile, "html", "", "path to a single-file composition (trusted code only)")
	fs.StringVar(&f.projectDir, "project-dir", "", "a local composition directory with index.html (trusted code only)")
	fs.StringVar(&f.composition, "composition", "", "a composition file relative to --project-dir")
	fs.StringVar(&f.out, "out", "", "output path (a directory for png-sequence; default under the media dir)")
	fs.StringVar(&f.format, "format", "", "mp4 (default) | webm | mov | png-sequence | gif")
	fs.StringVar(&f.quality, "quality", "", "draft | standard | high (default: compose_quality)")
	fs.StringVar(&f.resolution, "resolution", "", "landscape | portrait | landscape-4k | portrait-4k | square | square-4k")
	fs.StringVar(&f.snapshots, "snapshots", "", "comma-separated seconds to also save as PNG frames")
	fs.IntVar(&f.fps, "fps", 0, "frame rate 1-240 (default: the composition's data-fps)")
	fs.IntVar(&f.workers, "workers", 0, "Chrome workers 1-24 (default: compose_workers, else auto)")
	fs.BoolVar(&f.strict, "strict", true, "fail on lint errors / a failed check (--strict=false to report only)")
	route := fs.String("route", "auto", "local | auto (here when this machine has the composition lane, else a fleet node) | remote (a fleet node from delegate_remotes; ADR 0071)")
	_ = fs.Parse(args)
	params, perr := buildComposeParams(f)
	if perr != nil {
		return perr
	}
	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	res := composeremote.Run(context.Background(), cfg, p, core.Request{Task: core.TaskComposeVideo, Door: "cli:compose-video", Params: params}, *route)
	emitResult(res, *asJSON, "", *compactFlag)
	return nil
}

// videoFlags is the parsed CLI input for `generate-video`. Factored out so the
// param-building (buildVideoParams) is unit-testable without a live render.
type videoFlags struct {
	node        string
	minVRAM     *float64
	model       string
	still       string
	negative    string
	out         string
	transformer string
	frames      int
	width       int
	height      int
	steps       int
	seed        int
	reserveVRAM float64
	fast        bool
	hero        bool
	upscale     bool
}

// buildVideoParams maps the parsed CLI flags to the pipeline params map for a
// generate_video request. An EMPTY model is deliberately omitted so the pipeline
// applies the machine's videogen_family (the seat binding — e.g. ltx25); the old
// forced-"wan" default silently defeated the family on every CLI call, which is
// exactly how the Leg-3 verification rendered a broken Wan/LTX hybrid graph
// (live finding 2026-08-15). A machine with no family still gets wan — the
// render runner's own default. The still path is always carried as "still".
// Zero/empty optional flags are omitted so the pipeline/runner apply their own
// defaults (the workflow builder's settings + the machine's videogen_* config + the
// runner's --reserve-vram). reserve_vram is stringified to match the MCP wire shape.
func buildVideoParams(f videoFlags) map[string]any {
	params := map[string]any{}
	laneTargetParams(params, f.node, f.minVRAM)
	if f.model != "" {
		params["model"] = f.model
	}
	if f.still != "" {
		params["still"] = f.still
	}
	if f.negative != "" {
		params["negative"] = f.negative
	}
	if f.out != "" {
		params["out"] = f.out
	}
	if f.transformer != "" {
		params["transformer"] = f.transformer
	}
	if f.frames > 0 {
		params["frames"] = f.frames
	}
	if f.width > 0 {
		params["width"] = f.width
	}
	if f.height > 0 {
		params["height"] = f.height
	}
	if f.steps > 0 {
		params["steps"] = f.steps
	}
	if f.seed > 0 {
		params["seed"] = f.seed
	}
	if f.reserveVRAM > 0 {
		params["reserve_vram"] = strconv.FormatFloat(f.reserveVRAM, 'f', -1, 64)
	}
	if f.fast {
		params["fast"] = true
	}
	if f.hero {
		params["hero"] = true
	}
	if f.upscale {
		params["upscale"] = true
	}
	return params
}

// generateVideoCLI is one parsed `generate-video` command line.
type generateVideoCLI struct {
	fs      *flag.FlagSet
	video   videoFlags
	prompt  string
	asJSON  bool
	compact bool
	route   string
	remotes []string
}

const (
	nodeFlagUsage    = "name the ONE fleet node that runs this call: a node_id its /fleet/health publishes (any case) or one of the delegate_remotes URLs. No other node and not this machine is tried; a node that cannot take the call is refused by a word before anything is sent (with --json, data.refusal holds the word); --route local with --node is refused"
	minVRAMFlagUsage = "only a fleet node whose LARGEST card is at least this many GB may take the call (a number of GB, 0 or more; a 16 GB card reads 15.92 GiB); filters nodes only"
)

// generateVideoValueFlags are the flags that consume the next token. Bool flags
// (--fast, --hero, --upscale, --json, --compact) must NOT be listed: splitThreeArgs
// would take the following positional as their value.
var generateVideoValueFlags = map[string]bool{
	"config": true, "model": true, "negative": true, "frames": true,
	"width": true, "height": true, "steps": true, "seed": true, "reserve-vram": true,
	"transformer": true, "route": true, "remote": true, "node": true, "min-vram-gb": true,
}

// parseGenerateVideo parses the generate-video command line. The CLI carries every
// option the offload_generate_video MCP tool takes (prompt, still, out, model,
// negative, frames, width, height, steps, seed, reserve_vram, fast, hero, upscale):
// --fast was the missing one, which left the distilled Wan recipe reachable only
// through MCP (<node-e> parity audit, 2026-09-23).
func parseGenerateVideo(args []string, errorHandling flag.ErrorHandling) (generateVideoCLI, error) {
	fs := flag.NewFlagSet("generate-video", errorHandling)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	model := fs.String("model", "", "family override: wan | ltx25 | h3 | hunyuan (h3: still optional — t2v without). Empty = the machine's videogen_family seat (falls back to wan when no family is configured)")
	negative := fs.String("negative", "", "hard exclusions, e.g. 'blurry, distorted'")
	transformer := fs.String("transformer", "", "OPTIONAL per-request LTX-2.5 transformer file override (e.g. the bf16 transformer for one hero render — int8 stays this box's default; gap 5, bigger-models-2026-09-24.md). Wins over this box's (or the resolved family's) bound file; a no-op on any other family's graph")
	frames := fs.Int("frames", 0, "frame count (16fps; 81 ≈ 5s is the native ceiling)")
	width := fs.Int("width", 0, "width px")
	height := fs.Int("height", 0, "height px")
	steps := fs.Int("steps", 0, "sampler steps (0 = builder default: wan 20 native / 8 fast)")
	seed := fs.Int("seed", 0, "RNG seed for reproducibility")
	reserveVRAM := fs.Float64("reserve-vram", 0, "VRAM held back for the display (default per-workflow; ~2.0 for Hunyuan/Wan)")
	fast := fs.Bool("fast", false, "OPT-IN draft mode: 8-step lightx2v distill (wan; visibly weaker motion). The default is the native quality recipe")
	hero := fs.Bool("hero", false, "deprecated: the native quality pass IS the default now; accepted as a no-op")
	upscale := fs.Bool("upscale", false, "post-decode upscale using this machine's configured upscale model (e.g. 720p->1080p)")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	route := fs.String("route", "auto", "local | auto (here when this machine has the lane, derived from the files its route loads, else a fleet node from delegate_remotes) | remote (a fleet node; input files travel in a hash-checked bundle and the output is fetched back verified; ADR 0077)")
	remotes := new(repeatedFlag)
	fs.Var(remotes, "remote", "fleet node base URL for this call (repeatable); must be one of delegate_remotes")
	node := fs.String("node", "", nodeFlagUsage)
	minVRAM := fs.String("min-vram-gb", "", minVRAMFlagUsage)

	// generate-video takes THREE positionals (out path, still image, prompt); the
	// rest are flags.
	out, still, prompt, flagArgs := splitThreeArgs(args, generateVideoValueFlags)
	if err := fs.Parse(flagArgs); err != nil {
		return generateVideoCLI{}, err
	}
	floor, err := parseMinVRAMFlag("generate-video", *minVRAM)
	if err != nil {
		return generateVideoCLI{}, err
	}
	if out == "" || prompt == "" {
		return generateVideoCLI{}, fmt.Errorf("generate-video requires an output path, a still image, and a prompt: local-offload generate-video <out.mp4> <still.png> \"<prompt>\" [--model hunyuan|wan] [--frames 49] [--fast]")
	}
	return generateVideoCLI{
		fs: fs,
		video: videoFlags{
			model: *model, node: *node, minVRAM: floor, still: still, negative: *negative, out: out, transformer: *transformer,
			frames: *frames, width: *width, height: *height, steps: *steps,
			seed: *seed, reserveVRAM: *reserveVRAM, fast: *fast, hero: *hero, upscale: *upscale,
		},
		prompt: prompt, asJSON: *asJSON, compact: *compactFlag,
		route: *route, remotes: *remotes,
	}, nil
}

// runGenerateVideo handles `local-offload generate-video <out.mp4> <still.png>
// "<prompt>" [--model hunyuan|wan] [--frames 49] [--width N] [--height N]
// [--steps N] [--seed N] [--negative "..."] [--reserve-vram F] [--fast] [--upscale]
// [--json]`. The THREE positionals are the output path, the input still (I2V needs
// an image), and the prompt — mirroring the raw `node render/comfy-video.mjs out.mp4
// still.png "prompt"` CLI. It animates the still into a short clip on the LOCAL
// ComfyUI for free via the same runGenerateVideo pipeline branch the MCP tool uses.
// Shift and VAE temporal tiling are SETTLED inside the workflow builder and are
// intentionally NOT exposed here so a caller can't regress them.
func runGenerateVideo(args []string) error {
	c, err := parseGenerateVideo(args, flag.ExitOnError)
	if err != nil {
		return err
	}

	cfg := loadCfg(c.fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	res := mediaremote.Run(context.Background(), cfg, p, core.Request{
		Task:   core.TaskGenerateVideo,
		Door:   "cli:generate-video",
		Input:  c.prompt,
		Image:  c.video.still,
		Params: buildVideoParams(c.video),
	}, c.route, c.remotes)
	emitResult(res, c.asJSON, "", c.compact)
	return nil
}

// runAnimateCharacter handles `local-offload animate-character <out.mp4> <ref.png>
// <driver.mp4> "<prompt>" [--motion-prompt "..."] [--negative "..."] [--width N]
// [--height N] [--frames 81] [--steps N] [--seed N] [--pose-strength F]
// [--ref-strength F] [--reserve-vram F] [--route local|auto|remote] [--remote URL]... [--json]`. The FOUR positionals are the
// output path, the reference character image, the driver video whose motion is
// transferred, and the character/background prompt — mirroring the raw
// `node render/comfy-animate.mjs` CLI. Runs the same runAnimateCharacter pipeline
// branch the MCP tool uses. The pose-cache device is SETTLED inside the workflow
// builder (cpu — the template's gpu/int8 cache hard-kills ComfyUI) and is
// intentionally NOT exposed here so a caller can't regress it.
func runAnimateCharacter(args []string) error {
	c, err := parseAnimateCharacter(args, flag.ExitOnError)
	if err != nil {
		return err
	}

	cfg := loadCfg(c.fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	res := mediaremote.Run(context.Background(), cfg, p, core.Request{
		Task:   core.TaskAnimateCharacter,
		Door:   "cli:animate-character",
		Input:  c.animate.prompt,
		Image:  c.animate.ref,
		Video:  c.animate.driver,
		Params: buildAnimateParams(c.animate),
	}, c.route, c.remotes)
	emitResult(res, c.asJSON, "", c.compact)
	return nil
}

// animateFlags is the parsed CLI input for `animate-character`, factored out like videoFlags so the command line and the
// param-building are testable without a render.
type animateFlags struct {
	node                     string
	minVRAM                  *float64
	out, ref, driver, prompt string
	motionPrompt, negative   string
	width, height, frames    int
	steps, seed              int
	poseStrength             float64
	refStrength              float64
	reserveVRAM              float64
}

// animateCharacterCLI is one parsed `animate-character` command line.
type animateCharacterCLI struct {
	fs      *flag.FlagSet
	animate animateFlags
	asJSON  bool
	compact bool
	route   string
	remotes []string
}

// parseAnimateCharacter parses the animate-character command line (factored out of runAnimateCharacter, like
// parseGenerateVideo, so the flags can be tested without exiting the process or rendering).
func parseAnimateCharacter(args []string, errorHandling flag.ErrorHandling) (animateCharacterCLI, error) {
	fs := flag.NewFlagSet("animate-character", errorHandling)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	motionPrompt := fs.String("motion-prompt", "", "one-line description of the driver video's motion")
	negative := fs.String("negative", "", "hard exclusions")
	width := fs.Int("width", 0, "working width px (0 = the 482x854 template default)")
	height := fs.Int("height", 0, "working height px")
	frames := fs.Int("frames", 0, "frame count (default 81 — one native chunk; on an animategen_engine sdcpp box the default is its animategen_frames, else 49)")
	steps := fs.Int("steps", 0, "sampler steps (0 = the distilled recipe's 10)")
	seed := fs.Int("seed", 0, "RNG seed for reproducibility")
	poseStrength := fs.Float64("pose-strength", 0, "0-1: how strongly the driver's pose drives the output (0 = builder default 1.0)")
	refStrength := fs.Float64("ref-strength", 0, "0-1: how strongly the reference image pins identity (0 = builder default 1.0)")
	reserveVRAM := fs.Float64("reserve-vram", 0, "VRAM held back for the display (per-workflow override)")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	route := fs.String("route", "auto", "local | auto (here when this machine has the lane, derived from the files its route loads, else a fleet node from delegate_remotes) | remote (a fleet node; the reference image and driver video travel in a hash-checked bundle and the clip is fetched back verified; ADR 0077)")
	remotes := new(repeatedFlag)
	fs.Var(remotes, "remote", "fleet node base URL for this call (repeatable); must be one of delegate_remotes")
	node := fs.String("node", "", nodeFlagUsage)
	minVRAM := fs.String("min-vram-gb", "", minVRAMFlagUsage)

	out, ref, driver, prompt, flagArgs := splitFourArgs(args, map[string]bool{
		"config": true, "motion-prompt": true, "negative": true, "width": true,
		"height": true, "frames": true, "steps": true, "seed": true,
		"pose-strength": true, "ref-strength": true, "reserve-vram": true,
		"route": true, "remote": true, "node": true, "min-vram-gb": true, // animate-character
	})
	if err := fs.Parse(flagArgs); err != nil {
		return animateCharacterCLI{}, err
	}
	floor, err := parseMinVRAMFlag("animate-character", *minVRAM)
	if err != nil {
		return animateCharacterCLI{}, err
	}

	if out == "" || ref == "" || driver == "" || prompt == "" {
		return animateCharacterCLI{}, fmt.Errorf("animate-character requires an output path, a reference image, a driver video, and a prompt: local-offload animate-character <out.mp4> <ref.png> <driver.mp4> \"<prompt>\" [--frames 81]")
	}
	return animateCharacterCLI{
		fs: fs,
		animate: animateFlags{
			out: out, node: *node, minVRAM: floor, ref: ref, driver: driver, prompt: prompt, motionPrompt: *motionPrompt, negative: *negative,
			width: *width, height: *height, frames: *frames, steps: *steps, seed: *seed,
			poseStrength: *poseStrength, refStrength: *refStrength, reserveVRAM: *reserveVRAM,
		},
		asJSON: *asJSON, compact: *compactFlag, route: *route, remotes: *remotes,
	}, nil
}

// buildAnimateParams maps the parsed CLI flags to the pipeline params map for an animate_character request.
func buildAnimateParams(f animateFlags) map[string]any {
	params := map[string]any{"ref": f.ref, "driver": f.driver, "out": f.out}
	laneTargetParams(params, f.node, f.minVRAM)
	if f.motionPrompt != "" {
		params["motion_prompt"] = f.motionPrompt
	}
	if f.negative != "" {
		params["negative"] = f.negative
	}
	for k, v := range map[string]int{"width": f.width, "height": f.height, "frames": f.frames, "steps": f.steps, "seed": f.seed} {
		if v > 0 {
			params[k] = v
		}
	}
	if f.poseStrength > 0 {
		params["pose_strength"] = strconv.FormatFloat(f.poseStrength, 'f', -1, 64)
	}
	if f.refStrength > 0 {
		params["ref_strength"] = strconv.FormatFloat(f.refStrength, 'f', -1, 64)
	}
	if f.reserveVRAM > 0 {
		params["reserve_vram"] = strconv.FormatFloat(f.reserveVRAM, 'f', -1, 64)
	}
	return params
}
