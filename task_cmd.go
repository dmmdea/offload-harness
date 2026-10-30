// The text and vision task verbs (classify, extract, vqa, ocr, transcribe, video
// watch/describe, nim, forecast) and their shared route help.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/mcpserver"
	"github.com/dmmdea/offload-harness/internal/nimclient"
	"github.com/dmmdea/offload-harness/internal/sttremote"
	"github.com/dmmdea/offload-harness/internal/textremote"
	"github.com/dmmdea/offload-harness/internal/visionremote"
)

// textRouteHelp is the --route flag text of classify and extract (0.154.0): the MCP tools carry the same
// vocabulary (textremote).
const textRouteHelp = "classify / extract only. Where the call runs: local (default), auto (a fleet node advertising the text lane when the local GPU lease is held), remote (force a fleet node; defers when none is eligible)"

// runCLITextTask runs one cascade text task for a CLI verb. classify and extract ride the route lane
// (textremote), as their MCP twins do; with no route that is the in-process pipeline, unchanged.
// summarize and triage have no route on either surface (a fleet node refuses them), so a route given to
// them is an error rather than a silent local run under a route the caller asked for.
func runCLITextTask(ctx context.Context, cfg config.Config, runner textremote.Runner, req core.Request, route string) (core.Result, error) {
	switch req.Task {
	case core.TaskClassify, core.TaskExtract:
		return textremote.Run(ctx, cfg, runner, req, route), nil
	}
	if strings.TrimSpace(route) != "" {
		return core.Result{}, fmt.Errorf("--route applies to classify and extract; %s always runs on this box's cascade (no fleet node serves it)", req.Task)
	}
	return runner.Run(ctx, req), nil
}

// sttRouteHelp is the --route flag text of transcribe (ADR 0072). The CLI default is local, like the
// vision verbs: a script that never passes --route is byte-identical to before the route existed (the
// MCP tool defaults to auto).
const sttRouteHelp = "where the whisper model runs: local (default), auto (a fleet node when a render would keep the local whisper waiting), remote (force a fleet node; defers when none is eligible)"

// visionRouteHelp is the --route flag text shared by vqa / ocr / assess-image
// (0.116.0) — the MCP tools carry the same vocabulary (visionremote).
const visionRouteHelp = "where the vision model runs: local (default), auto (a fleet node when the local GPU lease is held or the local vision seat is busy), remote (force a fleet node; defers when none is eligible)"

// runVQA handles `local-offload vqa <image-path> --question "..." [--route local|auto|remote] [--json]`.
// Unlike the text tasks, the positional argument is an IMAGE PATH (or data URI),
// not stdin text; it is passed through Request.Image and resolved in the pipeline.
func runVQA(args []string) error {
	fs := flag.NewFlagSet("vqa", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	question := fs.String("question", "", "the question to ask about the image")
	route := fs.String("route", "", visionRouteHelp)
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "question": true, "route": true,
	})
	_ = fs.Parse(flagArgs)

	if positional == "" || positional == "-" {
		return fmt.Errorf("vqa requires an image path (not stdin): local-offload vqa <image> --question \"...\"")
	}
	if *question == "" {
		return fmt.Errorf("vqa requires --question \"...\"")
	}

	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	res := visionremote.Run(context.Background(), cfg, p, core.Request{
		Task:   core.TaskVQA,
		Door:   "cli:vqa",
		Image:  positional,
		Params: map[string]any{"question": *question},
	}, *route)
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	printHuman(res)
	return nil
}

// runVideoWatch handles `local-offload video-watch <video-path> --question "..."
// [--window-sec 8] [--fps 1] [--max-frames N] [--frame-width N] [--start S]
// [--end S] [--no-synthesize] [--json]`: the whole-file sweep (video_watch).
func runVideoWatch(args []string) error {
	fs := flag.NewFlagSet("video-watch", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	question := fs.String("question", "", "the question to answer about the whole video")
	windowSec := fs.Float64("window-sec", 0, "seconds per window (default 8)")
	fps := fs.Float64("fps", 0, "frames per second inside each window (default 1)")
	maxFrames := fs.Int("max-frames", 0, "cap on frames per window (default: config video_max_frames)")
	frameWidth := fs.Int("frame-width", 0, "frame width px (default: config video_frame_width)")
	startS := fs.Float64("start", 0, "start second")
	endS := fs.Float64("end", 0, "end second (0 = end of file)")
	noSynth := fs.Bool("no-synthesize", false, "return the per-window notes only (skip the text-seat answer)")
	selectFlag := fs.String("select", "", "comma-separated top-level result fields to keep")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "question": true, "select": true, "window-sec": true, "fps": true,
		"max-frames": true, "frame-width": true, "start": true, "end": true,
	})
	_ = fs.Parse(flagArgs)
	if positional == "" || positional == "-" {
		return fmt.Errorf("video-watch requires a video path (not stdin): local-offload video-watch <video> --question \"...\"")
	}
	if *question == "" {
		return fmt.Errorf("video-watch requires --question \"...\"")
	}
	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	res := p.Run(context.Background(), core.Request{
		Task:  core.TaskVideoWatch,
		Door:  "cli:video-watch",
		Video: positional,
		Params: map[string]any{
			"question": *question, "window_sec": *windowSec, "fps": *fps, "max_frames": *maxFrames,
			"frame_width": *frameWidth, "start": *startS, "end": *endS, "synthesize": !*noSynth,
		},
	})
	emitResult(res, *asJSON, *selectFlag, *compactFlag)
	return nil
}

// runVideoDescribe handles `local-offload video-describe <video-path> --question
// "..." [--json]`. The positional argument is a LOCAL VIDEO PATH (not stdin);
// the pipeline samples frames from it and runs the vision tier over them.
func runVideoDescribe(args []string) error {
	fs := flag.NewFlagSet("video-describe", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	question := fs.String("question", "", "the question to ask about the video")
	selectFlag := fs.String("select", "", "comma-separated top-level result fields to keep")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "question": true, "select": true,
	})
	_ = fs.Parse(flagArgs)

	if positional == "" || positional == "-" {
		return fmt.Errorf("video-describe requires a video path (not stdin): local-offload video-describe <video> --question \"...\"")
	}
	if *question == "" {
		return fmt.Errorf("video-describe requires --question \"...\"")
	}

	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskVideoDescribe,
		Door:   "cli:video-describe",
		Video:  positional,
		Params: map[string]any{"question": *question},
	})
	emitResult(res, *asJSON, *selectFlag, *compactFlag)
	return nil
}

// runTranscribe handles `local-offload transcribe <audio-path> [--language es]
// [--hq] [--route local|auto|remote] [--json]`. The positional argument is a LOCAL
// AUDIO/VIDEO PATH (not stdin); the pipeline converts it to 16kHz WAV and runs
// whisper-server over it, or the route sends it to a fleet node (sttremote).
func runTranscribe(args []string) error {
	fs := flag.NewFlagSet("transcribe", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	language := fs.String("language", "", "force language: en, es, or auto (default auto-detect)")
	hq := fs.Bool("hq", false, "use the configured higher-accuracy STT tier (slower; may return one full-span segment instead of timestamps)")
	selectFlag := fs.String("select", "", "comma-separated top-level result fields to keep (e.g. gist,language,srt_path — drops the verbose segments[])")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	route := fs.String("route", "", sttRouteHelp)
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "language": true, "select": true, "route": true,
	})
	_ = fs.Parse(flagArgs)

	if positional == "" || positional == "-" {
		return fmt.Errorf("transcribe requires an audio path (not stdin): local-offload transcribe <audio> [--language es]")
	}

	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	params := map[string]any{}
	if *language != "" {
		params["language"] = *language
	}
	if *hq {
		params["hq"] = true
	}
	res := sttremote.Run(context.Background(), cfg, p, core.Request{
		Task:   core.TaskTranscribe,
		Door:   "cli:transcribe",
		Audio:  positional,
		Params: params,
	}, *route)
	emitResult(res, *asJSON, *selectFlag, *compactFlag)
	return nil
}

// runForecastCLI handles `local-offload forecast (--series JSON | --series-path FILE.csv|.json)
// --prediction-length N [--target-columns A,B] [--past-covariate-columns ..] [--known-covariate-columns ..]
// [--quantiles 0.1,0.5,0.9] [--out-path F] [--route local|auto|remote] [--select a,b] [--compact]`. It is the
// offload_forecast door on the command line: the arguments mean what the MCP tool's mean, the answer is the
// result JSON. A path may also be the one positional argument.
func runForecastCLI(args []string) error {
	fs := flag.NewFlagSet("forecast", flag.ExitOnError)
	fs.String("config", "", "config file path")
	series := fs.String("series", "", "the series inline as JSON: [1,2,3], [[..],[..]] or [[[..]]]; null marks a missing value")
	seriesPath := fs.String("series-path", "", "a LOCAL .csv or .json series file (or give it as the one positional argument)")
	targets := fs.String("target-columns", "", "CSV: comma-separated header names to forecast (default: every numeric column that is not a covariate)")
	pastCols := fs.String("past-covariate-columns", "", "CSV: comma-separated header names known over the history only")
	knownCols := fs.String("known-covariate-columns", "", "CSV: comma-separated header names known over the history and the horizon")
	horizon := fs.Int("prediction-length", 0, "steps to forecast ahead (required; a CSV's trailing all-empty-target rows must number this)")
	quantiles := fs.String("quantiles", "", "comma-separated quantile levels among the model's 21 (default 0.1,0.5,0.9)")
	past := fs.String("past-covariates", "", "inline past covariates as JSON, (C,T) or (B,C,T)")
	known := fs.String("known-covariates", "", "inline known covariates as JSON, (C,T+prediction-length) or (B,C,T+prediction-length)")
	outPath := fs.String("out-path", "", "where to write a result of more than 2,000 numbers (default <media_dir>/forecast-<sha8>.json)")
	selectFlag := fs.String("select", "", "comma-separated top-level result fields to keep (e.g. shape,result_path)")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	route := fs.String("route", "", "where the forecast runs: auto (default; this box's model, else a fleet node), local, remote (force a fleet node)")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "series": true, "series-path": true, "target-columns": true, "past-covariate-columns": true,
		"known-covariate-columns": true, "prediction-length": true, "quantiles": true, "past-covariates": true,
		"known-covariates": true, "out-path": true, "select": true, "route": true,
	})
	_ = fs.Parse(flagArgs)

	in := mcpserver.ForecastArgs{
		SeriesPath:            *seriesPath,
		TargetColumns:         splitCSV(*targets),
		PastCovariateColumns:  splitCSV(*pastCols),
		KnownCovariateColumns: splitCSV(*knownCols),
		PredictionLength:      *horizon,
		OutPath:               *outPath,
		Route:                 *route,
		Select:                core.SelectKeys(*selectFlag),
	}
	if in.SeriesPath == "" {
		in.SeriesPath = positional
	}
	if *series != "" {
		in.Series = json.RawMessage(*series)
	}
	if *past != "" {
		in.PastCovariates = json.RawMessage(*past)
	}
	if *known != "" {
		in.KnownCovariates = json.RawMessage(*known)
	}
	for _, q := range splitCSV(*quantiles) {
		v, err := strconv.ParseFloat(q, 64)
		if err != nil {
			return fmt.Errorf("forecast: --quantiles %q: %q is not a number", *quantiles, q)
		}
		in.Quantiles = append(in.Quantiles, v)
	}

	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	res := mcpserver.RunForecast(context.Background(), cfg, p, nil, in, "cli:forecast")
	emitResult(res, true, "", *compactFlag)
	return nil
}

// runNim handles `local-offload nim <file|-|"literal text"> [--model id] [--base url]
// [--system "..."] [--max-tokens N] [--temp F] [--json]` and `local-offload nim
// --list-models`. It is the EXPLICIT remote tool: it calls an OpenAI-compatible
// NVIDIA NIM endpoint — NVIDIA's hosted build.nvidia.com API by default, or a
// self-hosted NIM container via --base. The key comes from $NVIDIA_API_KEY /
// $NGC_API_KEY (never config). NIM calls never touch the savings ledger (they are
// deliberate experiments/escalations, not defer-avoidance), and the local cascade
// and its GBNF grammar path are untouched.
func runNim(args []string) error {
	fs := flag.NewFlagSet("nim", flag.ExitOnError)
	fs.String("config", "", "config file path")
	model := fs.String("model", "", "model id (default from config; browse with --list-models)")
	base := fs.String("base", "", "OpenAI-compatible base URL incl. /v1 (default from config)")
	system := fs.String("system", "", "optional system prompt")
	maxTokens := fs.Int("max-tokens", 0, "max completion tokens (0 = config default)")
	temp := fs.Float64("temp", 0, "sampling temperature")
	listModels := fs.Bool("list-models", false, "list available model ids and exit")
	asJSON := fs.Bool("json", false, "print full result JSON")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "model": true, "base": true, "system": true, "max-tokens": true, "temp": true,
	})
	_ = fs.Parse(flagArgs)

	cfg := loadCfg(fs)
	baseURL := *base
	if baseURL == "" {
		baseURL = cfg.NIMEndpoint
	}
	key := nimclient.KeyForBase(baseURL) // env key only for NVIDIA hosts; never transmitted to a non-NVIDIA --base
	if key == "" && nimclient.IsHostedNVIDIA(baseURL) {
		return fmt.Errorf("nim: NVIDIA_API_KEY (or NGC_API_KEY) is not set — required for the hosted endpoint %s.\n"+
			"Get a free key at build.nvidia.com, then: export NVIDIA_API_KEY=nvapi-...\n"+
			"(A self-hosted NIM via --base http://host:8000/v1 needs no key.)", baseURL)
	}
	timeout := time.Duration(cfg.NIMTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	client := nimclient.New(baseURL, key, timeout)
	ctx := context.Background()

	if *listModels {
		ids, err := client.ListModels(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			b, _ := json.MarshalIndent(ids, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		for _, id := range ids {
			fmt.Println(id)
		}
		fmt.Fprintf(os.Stderr, "%d models at %s\n", len(ids), baseURL)
		return nil
	}

	user, err := readNimInput(positional)
	if err != nil {
		return err
	}
	if strings.TrimSpace(user) == "" {
		return fmt.Errorf("nim requires input: a file path, \"-\" for stdin, or literal text (or use --list-models)")
	}

	mdl := *model
	if mdl == "" {
		mdl = cfg.NIMModel
	}
	mt := *maxTokens
	if mt == 0 {
		mt = cfg.NIMMaxTokens
	}
	res, err := client.Chat(ctx, mdl, *system, user, mt, *temp)
	if err != nil {
		return err
	}
	if *asJSON {
		out := map[string]any{
			"model":             res.Model,
			"content":           res.Content,
			"reasoning_content": res.ReasoningContent,
			"tokens_in":         res.TokensIn,
			"tokens_out":        res.TokensOut,
			"truncated":         res.Truncated,
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	switch {
	case res.Content != "":
		fmt.Println(res.Content)
	case res.ReasoningContent != "":
		fmt.Println(res.ReasoningContent)
		fmt.Fprintln(os.Stderr, "note: answer (content) was empty; printed reasoning_content instead — raise --max-tokens")
	default:
		fmt.Fprintln(os.Stderr, "note: model returned no content (a stop/content-filter, or --max-tokens too low to reach an answer)")
	}
	if res.Truncated {
		fmt.Fprintln(os.Stderr, "note: output truncated at max-tokens; raise --max-tokens for a full answer")
	}
	fmt.Fprintf(os.Stderr, "[%s] tokens in=%d out=%d\n", res.Model, res.TokensIn, res.TokensOut)
	// Don't report a blank success: an empty content+reasoning response exits non-zero.
	if res.Content == "" && res.ReasoningContent == "" {
		return fmt.Errorf("nim: empty response from %s (raise --max-tokens or try another model)", res.Model)
	}
	return nil
}

// readNimInput reads the nim user content: "-"/"" = stdin, an existing file path =
// its contents, anything else = the literal text itself (so a quick test prompt
// needs no file). This file-or-literal convenience is unique to the nim tool.
func readNimInput(arg string) (string, error) {
	if arg == "" || arg == "-" {
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	if info, statErr := os.Stat(arg); statErr == nil && !info.IsDir() {
		b, err := os.ReadFile(arg)
		return string(b), err
	}
	return arg, nil // literal prompt text
}

// runOCR handles `local-offload ocr <image-path> [--json]`. Like vqa the
// positional argument is an IMAGE PATH (or data URI), not stdin text; there is no
// question — the task is fixed (transcribe all text). It returns {text:...}.
func runOCR(args []string) error {
	fs := flag.NewFlagSet("ocr", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	route := fs.String("route", "", visionRouteHelp)
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "route": true,
	})
	_ = fs.Parse(flagArgs)

	if positional == "" || positional == "-" {
		return fmt.Errorf("ocr requires an image path (not stdin): local-offload ocr <image>")
	}

	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	res := visionremote.Run(context.Background(), cfg, p, core.Request{
		Task:  core.TaskOCR,
		Door:  "cli:ocr",
		Image: positional,
	}, *route)
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	printHuman(res)
	return nil
}

// readSchemaFile reads + parses a --schema file into the {properties:{...}} JSON
// Schema map the extract task expects. Shared by the `extract` and `extract-image`
// subcommands so they parse the schema identically.
func readSchemaFile(path string) (map[string]any, error) {
	sb, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sch map[string]any
	if err := json.Unmarshal(sb, &sch); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	return sch, nil
}

// runExtractImage handles `local-offload extract-image <image-path> --schema f.json
// [--json]`. The positional argument is an IMAGE PATH (or data URI), not stdin
// text; the schema is read exactly like the `extract` subcommand. The pipeline
// composes OCR -> the existing text-extract over the OCR text.
func runExtractImage(args []string) error {
	fs := flag.NewFlagSet("extract-image", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	schemaPath := fs.String("schema", "", "path to a JSON schema file")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "schema": true,
	})
	_ = fs.Parse(flagArgs)

	if positional == "" || positional == "-" {
		return fmt.Errorf("extract-image requires an image path (not stdin): local-offload extract-image <image> --schema schema.json")
	}
	if *schemaPath == "" {
		return fmt.Errorf("extract-image requires --schema schema.json")
	}
	sch, err := readSchemaFile(*schemaPath)
	if err != nil {
		return err
	}

	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskExtractImage,
		Door:   "cli:extract-image",
		Image:  positional,
		Params: map[string]any{"schema": sch},
	})
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	printHuman(res)
	return nil
}

// runAssessImage handles `local-offload assess-image <image-path> [--brief "..."]
// [--json]`. The positional argument is an IMAGE PATH (or data URI); the optional
// --brief is woven into the prompt. The pipeline runs a grammar-constrained QA and
// returns {has_people, has_text, matches_brief, notes}.
func runAssessImage(args []string) error {
	fs := flag.NewFlagSet("assess-image", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	brief := fs.String("brief", "", "optional description the image should match")
	route := fs.String("route", "", visionRouteHelp)
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "brief": true, "route": true,
	})
	_ = fs.Parse(flagArgs)

	if positional == "" || positional == "-" {
		return fmt.Errorf("assess-image requires an image path (not stdin): local-offload assess-image <image> [--brief \"...\"]")
	}

	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	params := map[string]any{}
	if *brief != "" {
		params["brief"] = *brief
	}
	res := visionremote.Run(context.Background(), cfg, p, core.Request{
		Task:   core.TaskAssessImage,
		Door:   "cli:assess-image",
		Image:  positional,
		Params: params,
	}, *route)
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	printHuman(res)
	return nil
}
