// The generate-svg and generate-audio verbs, and the lane-targeting helpers
// the audio and video verbs share.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mediaremote"
)

func runGenerateSVG(args []string) error {
	fs := flag.NewFlagSet("generate-svg", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	spec := fs.String("spec", "", "component spec as a JSON string")
	specFile := fs.String("spec-file", "", "path to a JSON file with the component spec")
	out := fs.String("out", "", "output .svg path (default under the svg dir)")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "spec": true, "spec-file": true, "out": true,
	})
	_ = fs.Parse(flagArgs)
	if positional == "" || positional == "-" {
		return fmt.Errorf("generate-svg requires a kind: local-offload generate-svg <gauge|comparison-bar|chromatogram|icon> --spec '<json>'")
	}
	specStr := *spec
	if specStr == "" && *specFile != "" {
		b, rerr := os.ReadFile(*specFile)
		if rerr != nil {
			return rerr
		}
		specStr = string(b)
	}
	if specStr == "" {
		specStr = "{}"
	}
	var specObj map[string]any
	if jerr := json.Unmarshal([]byte(specStr), &specObj); jerr != nil {
		return fmt.Errorf("generate-svg: invalid --spec JSON: %w", jerr)
	}
	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	params := map[string]any{"kind": positional, "spec": specObj}
	if *out != "" {
		params["out"] = *out
	}
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateSVG, Door: "cli:generate-svg", Params: params})
	emitResult(res, *asJSON, "", *compactFlag)
	return nil
}

// audioFlags is the parsed CLI input for `generate-audio`. Factored out so the
// param-building (buildAudioParams) is unit-testable without a live render.
type audioFlags struct {
	node        string
	kind        string
	voice       string
	ttsVoice    string // voice=endpoint: server-side voice name
	clone       string
	lang        string
	out         string
	seconds     int
	seed        int
	reserveVRAM float64
}

// buildAudioParams maps the parsed CLI flags to the pipeline params map for a
// generate_audio request. Empty kind defaults to "voice" (matching the pipeline
// default); zero/empty optional flags are omitted so the pipeline applies its own
// defaults. reserve_vram is stringified to match the MCP tool's wire shape.
func buildAudioParams(f audioFlags) map[string]any {
	kind := f.kind
	if kind == "" {
		kind = "voice"
	}
	params := map[string]any{"kind": kind}
	laneTargetParams(params, f.node, nil)
	if f.voice != "" {
		params["voice"] = f.voice
	}
	if f.ttsVoice != "" {
		params["tts_voice"] = f.ttsVoice
	}
	if f.clone != "" {
		params["clone"] = f.clone
	}
	if f.lang != "" {
		params["lang"] = f.lang
	}
	if f.out != "" {
		params["out"] = f.out
	}
	if f.seconds > 0 {
		params["seconds"] = f.seconds
	}
	if f.seed > 0 {
		params["seed"] = f.seed
	}
	if f.reserveVRAM > 0 {
		params["reserve_vram"] = strconv.FormatFloat(f.reserveVRAM, 'f', -1, 64)
	}
	return params
}

// runGenerateAudio handles `local-offload generate-audio <out> "<text>"
// [--kind voice|music] [--clone ref.wav] [--lang es] [--seconds N] [--seed N]
// [--reserve-vram F] [--json]`. The TWO positionals are the output path and the
// text (narration for voice, style prompt for music) — mirroring the raw
// `node render/tts.mjs out.wav "text"` CLI. It synthesizes on the LOCAL GPU for
// free via the same runGenerateAudio pipeline branch the MCP tool uses.
func runGenerateAudio(args []string) error {
	c, err := parseGenerateAudio(args, flag.ExitOnError)
	if err != nil {
		return err
	}

	cfg := loadCfg(c.fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	params := buildAudioParams(c.audio)
	res := mediaremote.Run(context.Background(), cfg, p, core.Request{
		Task:   core.TaskGenerateAudio,
		Door:   "cli:generate-audio",
		Input:  c.text,
		Params: params,
	}, c.route, c.remotes)
	emitResult(res, c.asJSON, "", c.compact)
	return nil
}

// generateAudioCLI is one parsed `generate-audio` command line.
type generateAudioCLI struct {
	fs      *flag.FlagSet
	audio   audioFlags
	text    string
	asJSON  bool
	compact bool
	route   string
	remotes []string
}

// parseGenerateAudio parses the generate-audio command line (factored out of runGenerateAudio, like
// parseGenerateVideo, so the positionals can be tested without a render).
func parseGenerateAudio(args []string, errorHandling flag.ErrorHandling) (generateAudioCLI, error) {
	fs := flag.NewFlagSet("generate-audio", errorHandling)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	kind := fs.String("kind", "voice", "voice (Chatterbox TTS) | music (ACE-Step)")
	voice := fs.String("voice", "", "voice: generalist | finetuned | endpoint (finetuned needs this machine's voicegen_ft_* config; endpoint renders through tts_endpoint, the configured OpenAI-compatible speech server — the default on a box with no voicegen_script)")
	ttsVoice := fs.String("tts-voice", "", "voice=endpoint: the server-side voice/profile name (default: this box's tts_voice, else the server's default)")
	clone := fs.String("clone", "", "voice: local path to a reference .wav for zero-shot voice cloning")
	lang := fs.String("lang", "", "voice: language code (default es)")
	seconds := fs.Int("seconds", 0, "music: clip length in seconds")
	seed := fs.Int("seed", 0, "RNG seed for reproducibility")
	reserveVRAM := fs.Float64("reserve-vram", 0, "music: VRAM held back for the display")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	route := fs.String("route", "auto", "local | auto (here when this machine has the lane, derived from the files its route loads, else a fleet node from delegate_remotes) | remote (a fleet node; input files travel in a hash-checked bundle and the output is fetched back verified; ADR 0077)")
	remotes := new(repeatedFlag)
	fs.Var(remotes, "remote", "fleet node base URL for this call (repeatable); must be one of delegate_remotes")
	node := fs.String("node", "", nodeFlagUsage)

	// generate-audio takes TWO positionals (out path, text); the rest are flags.
	out, text, flagArgs := splitTwoArgs(args, map[string]bool{
		"config": true, "kind": true, "voice": true, "clone": true, "lang": true,
		"seconds": true, "seed": true, "reserve-vram": true, "tts-voice": true,
		"route": true, "remote": true, "node": true, // generate-audio
	})
	if err := fs.Parse(flagArgs); err != nil {
		return generateAudioCLI{}, err
	}

	if out == "" || text == "" {
		return generateAudioCLI{}, fmt.Errorf("generate-audio requires an output path and text: local-offload generate-audio <out.wav> \"<text>\" [--kind voice|music] [--lang es]")
	}
	return generateAudioCLI{
		fs: fs,
		audio: audioFlags{
			node: *node, kind: *kind, voice: *voice, ttsVoice: *ttsVoice, clone: *clone, lang: *lang, out: out,
			seconds: *seconds, seed: *seed, reserveVRAM: *reserveVRAM,
		},
		text: text, asJSON: *asJSON, compact: *compactFlag,
		route: *route, remotes: *remotes,
	}, nil
}

// laneTargetParams puts the --node and --min-vram-gb of a lane verb into the request's params, only when they were given (an
// explicit 0 is a number the caller gave). generate-audio has no VRAM floor and passes nil.
func laneTargetParams(params map[string]any, node string, minVRAM *float64) {
	if n := strings.TrimSpace(node); n != "" {
		params["node"] = n
	}
	if minVRAM != nil {
		params["min_vram_gb"] = *minVRAM
	}
}

// parseMinVRAMFlag reads --min-vram-gb of a verb that takes it: nil when the flag was not given, else a number of GB, 0 or more
// (run-graph reads its own spelling of the same flag in runGraphParams).
func parseMinVRAMFlag(verb, s string) (*float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil, fmt.Errorf("%s: --min-vram-gb must be a number of GB, 0 or more; got %q", verb, s)
	}
	return &v, nil
}
