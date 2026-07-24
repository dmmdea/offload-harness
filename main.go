// Command local-offload delegates short-context grunt work (summarize, classify,
// extract, triage) to a free local model, exposed as both a CLI and an MCP
// server. It never calls a cloud model: on failure it returns a structured
// defer so the caller (Claude) handles the task itself.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/calibration"
	"github.com/dmmdea/offload-harness/internal/confhead"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/eval"
	"github.com/dmmdea/offload-harness/internal/exemplars"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/grounding"
	"github.com/dmmdea/offload-harness/internal/health"
	"github.com/dmmdea/offload-harness/internal/judge"
	"github.com/dmmdea/offload-harness/internal/knn"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/mcpserver"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/nimclient"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/report"
	"github.com/dmmdea/offload-harness/internal/router"
	"github.com/dmmdea/offload-harness/internal/shadow"
	"github.com/dmmdea/offload-harness/internal/trajectory"
)

const version = "0.22.19"

// Keep config.example.json in lockstep with config.Default() (LO-17):
//go:generate go run ./cmd/genexample

func main() {
	sub, args, ok := hoistGlobalConfig(os.Args[1:])
	if !ok {
		usage()
		os.Exit(2)
	}
	var err error
	switch sub {
	case "summarize", "classify", "extract", "triage":
		err = runTask(sub, args)
	case "vqa":
		err = runVQA(args)
	case "video-describe":
		err = runVideoDescribe(args)
	case "transcribe":
		err = runTranscribe(args)
	case "generate-image":
		err = runGenerateImage(args)
	case "inpaint-image":
		err = runInpaintImage(args)
	case "generate-svg":
		err = runGenerateSVG(args)
	case "generate-audio":
		err = runGenerateAudio(args)
	case "generate-video":
		err = runGenerateVideo(args)
	case "run-graph":
		err = runRunGraph(args)
	case "edit-image":
		err = runEditImage(args)
	case "media":
		err = runMedia(args)
	case "nim":
		err = runNim(args)
	case "ocr":
		err = runOCR(args)
	case "extract-image":
		err = runExtractImage(args)
	case "assess-image":
		err = runAssessImage(args)
	case "mcp":
		err = runMCP(args)
	case "fleet-serve":
		err = runFleetServe(args)
	case "fleet-measure":
		err = runFleetMeasure(args)
	case "ledger":
		err = runLedger(args)
	case "doctor":
		err = runDoctor(args)
	case "models":
		err = runModels(args)
	case "calibrate":
		err = runCalibrate(args)
	case "health":
		err = runHealth(args)
	case "train-router":
		err = runTrainRouter(args)
	case "shadow-label":
		err = runShadowLabel(args)
	case "agent-trajectory-label":
		err = runAgentTrajectoryLabel(args)
	case "agent-trajectory-gate":
		err = runAgentTrajectoryGate(args)
	case "train-confhead":
		err = runTrainConfHead(args)
	case "confhead-eval":
		err = runConfheadEval(args)
	case "confhead-calibrate":
		err = runConfheadCalibrate(args)
	case "optimize":
		err = runOptimize(args)
	case "audit-sample":
		err = runAuditSample(args)
	case "eval":
		err = runEval(args)
	case "stats":
		err = runStats(args)
	case "version", "--version", "-v":
		fmt.Println("local-offload", version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// hoistGlobalConfig pulls a global "--config <path>" / "--config=<path>" (or the
// single-dash forms) that appears BEFORE the subcommand and re-attaches it to
// the subcommand's args, so the subcommand's own flag parser handles it. Without
// this, `local-offload --config x triage ...` dispatches on "--config" and falls
// through to usage. A --config placed AFTER the subcommand is left untouched
// (already parsed there). Returns ok=false when no subcommand is present.
func hoistGlobalConfig(in []string) (sub string, args []string, ok bool) {
	var cfg string
	i := 0
loop:
	for i < len(in) {
		a := in[i]
		switch {
		case a == "--config" || a == "-config":
			if i+1 < len(in) {
				cfg = in[i+1]
				i += 2
			} else {
				i++ // dangling flag, no value
			}
		case strings.HasPrefix(a, "--config="):
			cfg = strings.TrimPrefix(a, "--config=")
			i++
		case strings.HasPrefix(a, "-config="):
			cfg = strings.TrimPrefix(a, "-config=")
			i++
		default:
			break loop // first non-config token is the subcommand
		}
	}
	rest := in[i:]
	if len(rest) == 0 {
		return "", nil, false
	}
	sub = rest[0]
	args = rest[1:]
	if cfg != "" {
		args = append([]string{"--config", cfg}, args...)
	}
	return sub, args, true
}

func usage() {
	fmt.Fprint(os.Stderr, `local-offload `+version+` — delegate grunt work to a free local model

Usage:
  local-offload summarize <file|-> [--max-points N] [--json]
  local-offload classify  <file|-> --labels a,b,c [--json]
  local-offload extract   <file|-> --schema schema.json [--json]
  local-offload triage    <file|-> --question "..." [--json]
  local-offload vqa       <image-path> --question "..." [--json]
  local-offload video-describe <video-path> --question "..." [--json]
  local-offload transcribe <audio-path> [--language es] [--hq] [--json]
  local-offload ocr       <image-path> [--json]
  local-offload extract-image <image-path> --schema schema.json [--json]
  local-offload assess-image <image-path> [--brief "..."] [--json]
  local-offload generate-audio <out> "<text>" [--kind voice|music] [--voice generalist|finetuned] [--clone ref.wav] [--lang es] [--seconds N] [--seed N]
  local-offload generate-image "<prompt>" [--negative "..."] [--width N] [--height N] [--steps N] [--seed N] [--out path]
  local-offload generate-image --batch jobs.jsonl    N prompts through ONE warm ComfyUI session (checkpoint loads once)
  local-offload inpaint-image <image> --mask m.png --prompt "..."   re-render ONLY the masked region (white=repaint)
  local-offload generate-video <out.mp4> <still.png> "<prompt>" [--model hunyuan|wan] [--frames 49] [--seed N] [--reserve-vram F]
  local-offload run-graph --graph <g.json> [--manifest <m.json>] [--out-dir <d>] [--reserve-vram F] [--json]
  local-offload nim <file|-|"text"> [--model id] [--base url] [--system "..."] [--max-tokens N] [--temp F] [--json]
  local-offload nim --list-models        list a NIM endpoint's model ids (free hosted catalog or self-hosted)
  local-offload mcp                      run as an MCP server (stdio)
  local-offload fleet-serve [--listen ADDR] [--listen-trusted-network] [--node-id NAME]   join the fleet-dispatcher fleet (health/dispatch/jobs on :18811; docs/FLEET-NODE.md)
  local-offload fleet-measure            prime the fleet footprint store: one minimal render per configured task, then print the recorded entries
  local-offload ledger [--since DAYS]    token-savings report
  local-offload doctor                   check endpoint health + config
  local-offload models                   show configured offload model
  local-offload eval [--dir DIR]         code-based quality eval (AURC, deferral-curve AUDC/QNC)
  local-offload confhead-eval            out-of-fold adoption gate (AURC/AUGRC + paired-bootstrap CI)
  local-offload confhead-calibrate       per-task conformal p(correct) escalation thresholds (ADOPT tasks)
  local-offload stats                    observational per-task ledger telemetry
  local-offload version

Global: --config <path> (or $LOCAL_OFFLOAD_CONFIG)

extract --schema expects a JSON Schema object with a "properties" map, e.g.
  {"properties":{"name":{"type":"string"},"amount":{"type":"number"}}}
A bare {"field":"string"} map has no usable properties and is deferred.
`)
}

func loadCfg(fs *flag.FlagSet) config.Config {
	cfg, _ := loadCfgWithSource(fs)
	return cfg
}

// loadCfgWithSource loads the config and DISCLOSES its true source, warning on stderr
// whenever the effective config is built-in defaults — including the two silent-trap
// shapes a review surfaced: an explicit --config/$LOCAL_OFFLOAD_CONFIG path that does not
// exist (Load maps IsNotExist to defaults with a NIL error by design), and a file that
// exists but fails to parse. Silent degradation is the trap (live incident 2026-07-20):
// a box whose real config lived at a non-conventional path served every bare CLI call
// from built-in defaults — empty VisionModel and all — and the only symptom was a
// misleading "no vision model configured" defer in a consumer's pipeline hours later.
func loadCfgWithSource(fs *flag.FlagSet) (config.Config, config.Source) {
	cfg, src := config.LoadWithSource(fs.Lookup("config").Value.String())
	config.WarnOnDefaults(src, os.Stderr)
	return cfg, src
}

// resolveCfgPath picks the config file by precedence: explicit --config flag >
// $LOCAL_OFFLOAD_CONFIG > ./config.json if it exists (LO-4: the README
// quickstart says `cp config.example.json config.json`, which was never read) >
// the conventional ~/.local-offload/config.json if it exists > "" (built-in
// defaults). The conventional-path fallback is the fix for a bare
// `local-offload mcp` spawned by an MCP host that passes neither flag nor
// env (as the registration did): without it the server silently ran on defaults
// with ShadowEnabled=false, so capture never fired and the flywheel starved.
// exists is injected so the precedence is unit-testable without touching disk.
func resolveCfgPath(flagPath, envPath, home string, exists func(string) bool) string {
	return config.ResolvePath(flagPath, envPath, home, exists)
}

func openPipeline(cfg config.Config) (*pipeline.Pipeline, func(), error) {
	if err := cfg.EnsureDirs(); err != nil {
		return nil, nil, err
	}
	client := llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, time.Duration(cfg.RequestTimeoutSec)*time.Second)
	// Cache + ledger are bbolt (single-writer, exclusive file lock). When the
	// long-running MCP server holds the lock, a CLI invocation degrades to
	// cache-less rather than aborting — they speed things up / report savings,
	// they are not required for correctness.
	var ca *cache.Cache
	if cfg.CachePath != "" { // "" = caller opted out of caching (e.g. the confhead A/B, where a shared cache would cross-contaminate arms)
		var err error
		ca, err = cache.Open(cfg.CachePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "note: cache unavailable (held by the MCP server?); continuing without cache")
			ca = nil
		}
	}
	led, err := ledger.Open(cfg.LedgerPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "note: ledger unavailable (held by the MCP server?); continuing without ledger")
		led = nil
	}
	return pipeline.New(cfg, client, ca, led), func() {
		if ca != nil {
			ca.Close()
		}
		if led != nil {
			led.Close()
		}
	}, nil
}

func runTask(task string, args []string) error {
	fs := flag.NewFlagSet(task, flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	maxPoints := fs.Int("max-points", 5, "summarize: max bullet points")
	labels := fs.String("labels", "", "classify: comma-separated labels")
	question := fs.String("question", "", "triage: the yes/no/unsure question")
	schemaPath := fs.String("schema", "", "extract: path to a JSON schema file")
	selectFlag := fs.String("select", "", "comma-separated top-level result fields to keep")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	// Go's flag pkg stops at the first positional, so split the input arg out
	// first and parse the remaining flags (allows `summarize <file> --json`).
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "labels": true, "question": true, "schema": true, "max-points": true, "select": true,
	})
	_ = fs.Parse(flagArgs)

	input, err := readInput(positional)
	if err != nil {
		return err
	}
	params := map[string]any{}
	switch core.TaskType(task) {
	case core.TaskSummarize:
		params["max_points"] = *maxPoints
	case core.TaskClassify:
		if *labels == "" {
			return fmt.Errorf("classify requires --labels a,b,c")
		}
		params["labels"] = splitCSV(*labels)
	case core.TaskTriage:
		if *question == "" {
			return fmt.Errorf("triage requires --question")
		}
		params["question"] = *question
	case core.TaskExtract:
		if *schemaPath == "" {
			return fmt.Errorf("extract requires --schema schema.json")
		}
		sch, err := readSchemaFile(*schemaPath)
		if err != nil {
			return err
		}
		params["schema"] = sch
	}

	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	res := p.Run(context.Background(), core.Request{Task: core.TaskType(task), Input: input, Params: params})
	emitResult(res, *asJSON, *selectFlag, *compactFlag)
	return nil
}

// runVQA handles `local-offload vqa <image-path> --question "..." [--json]`.
// Unlike the text tasks, the positional argument is an IMAGE PATH (or data URI),
// not stdin text; it is passed through Request.Image and resolved in the pipeline.
func runVQA(args []string) error {
	fs := flag.NewFlagSet("vqa", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	question := fs.String("question", "", "the question to ask about the image")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "question": true,
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

	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskVQA,
		Image:  positional,
		Params: map[string]any{"question": *question},
	})
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	printHuman(res)
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
		Video:  positional,
		Params: map[string]any{"question": *question},
	})
	emitResult(res, *asJSON, *selectFlag, *compactFlag)
	return nil
}

// runTranscribe handles `local-offload transcribe <audio-path> [--language es]
// [--hq] [--json]`. The positional argument is a LOCAL AUDIO/VIDEO PATH (not
// stdin); the pipeline converts it to 16kHz WAV and runs whisper-server over it.
func runTranscribe(args []string) error {
	fs := flag.NewFlagSet("transcribe", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	language := fs.String("language", "", "force language: en, es, or auto (default auto-detect)")
	hq := fs.Bool("hq", false, "use the configured higher-accuracy STT tier (slower; may return one full-span segment instead of timestamps)")
	selectFlag := fs.String("select", "", "comma-separated top-level result fields to keep (e.g. gist,language,srt_path — drops the verbose segments[])")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "language": true, "select": true,
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
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskTranscribe,
		Audio:  positional,
		Params: params,
	})
	emitResult(res, *asJSON, *selectFlag, *compactFlag)
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
	steps := fs.Int("steps", 0, "sampler steps (default 30)")
	seed := fs.Int("seed", 0, "RNG seed (default random)")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")
	batchFile := fs.String("batch", "", "render a JSONL batch of jobs through ONE warm ComfyUI session (one line per job: {\"prompt\":...,\"out\"?,\"negative\"?,\"width\"?,\"height\"?,\"steps\"?,\"seed\"?})")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "negative": true, "out": true,
		"width": true, "height": true, "steps": true, "seed": true,
		"batch": true,
	})
	_ = fs.Parse(flagArgs)

	if *batchFile != "" {
		raw, rerr := os.ReadFile(*batchFile)
		if rerr != nil {
			return rerr
		}
		var jobs []pipeline.ImageBatchJob
		for n, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var j pipeline.ImageBatchJob
			if jerr := json.Unmarshal([]byte(line), &j); jerr != nil {
				return fmt.Errorf("batch line %d: %w", n+1, jerr)
			}
			jobs = append(jobs, j)
		}
		cfg := loadCfg(fs)
		p, cleanup, err := openPipeline(cfg)
		if err != nil {
			return err
		}
		defer cleanup()
		items, berr := p.RunImageBatch(context.Background(), jobs)
		ok := 0
		for _, it := range items {
			if it.OK {
				ok++
			}
		}
		payload := map[string]any{"count": len(items), "succeeded": ok, "failed": len(items) - ok, "items": items}
		data, _ := json.Marshal(payload)
		res := core.Result{OK: berr == nil && ok == len(items), Data: data}
		if berr != nil {
			res.Reason = berr.Error()
		}
		emitResult(res, *asJSON, "", *compactFlag)
		return berr
	}

	if positional == "" || positional == "-" {
		return fmt.Errorf("generate-image requires a prompt (not stdin): local-offload generate-image \"<prompt>\" [--width 1024]")
	}

	cfg := loadCfg(fs)
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
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateImage,
		Input:  positional,
		Params: params,
	})
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
		Input:  *prompt,
		Params: params,
	})
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
	_ = fs.Parse(args)

	params, err := runGraphParams(args)
	if err != nil {
		return err
	}

	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	res := p.Run(context.Background(), core.Request{Task: core.TaskRunGraph, Params: params})
	emitResult(res, *asJSON, "", *compactFlag)
	return nil
}

// runGraphParams parses the run-graph flags into the pipeline request params map. It is
// the testable validation seam: --graph OR --graph-json is required (else an error), and
// inline JSON is materialized to a temp file whose path is threaded on (the runner reads
// files). Kept independent of runRunGraph's pipeline wiring so the arg contract is unit-
// testable without a ComfyUI/pipeline.
func runGraphParams(args []string) (map[string]any, error) {
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
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *graph == "" && *graphJSON == "" {
		return nil, fmt.Errorf("run-graph: --graph or --graph-json required")
	}
	graphPath, err := materializeArg(*graph, *graphJSON, "run-graph-*.json")
	if err != nil {
		return nil, fmt.Errorf("graph: %w", err)
	}
	manifestPath, err := materializeArg(*manifest, *manifestJSON, "run-graph-manifest-*.json")
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	return map[string]any{
		"graph_path":    graphPath,
		"manifest_path": manifestPath,
		"out_dir":       *outDir,
		"reserve_vram":  *reserve,
	}, nil
}

// materializeArg returns path if set, else writes inline json to a temp file and returns
// that path (the runner reads files, not inline). Empty+empty → ("", nil).
func materializeArg(path, inline, pattern string) (string, error) {
	if path != "" {
		return path, nil
	}
	if inline == "" {
		return "", nil
	}
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(inline); err != nil {
		return "", err
	}
	return f.Name(), nil
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
	res := p.Run(context.Background(), core.Request{Task: core.TaskEditImage, Image: positional, Params: params})
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
	res := p.Run(context.Background(), core.Request{Task: core.TaskMedia, Params: params})
	emitResult(res, *asJSON, "", *compactFlag)
	return nil
}

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
	res := p.Run(context.Background(), core.Request{Task: core.TaskGenerateSVG, Params: params})
	emitResult(res, *asJSON, "", *compactFlag)
	return nil
}

// audioFlags is the parsed CLI input for `generate-audio`. Factored out so the
// param-building (buildAudioParams) is unit-testable without a live render.
type audioFlags struct {
	kind        string
	voice       string
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
	if f.voice != "" {
		params["voice"] = f.voice
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
	fs := flag.NewFlagSet("generate-audio", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	kind := fs.String("kind", "voice", "voice (Chatterbox TTS) | music (ACE-Step)")
	voice := fs.String("voice", "", "voice: generalist | finetuned (finetuned needs this machine's voicegen_ft_* config)")
	clone := fs.String("clone", "", "voice: local path to a reference .wav for zero-shot voice cloning")
	lang := fs.String("lang", "", "voice: language code (default es)")
	seconds := fs.Int("seconds", 0, "music: clip length in seconds")
	seed := fs.Int("seed", 0, "RNG seed for reproducibility")
	reserveVRAM := fs.Float64("reserve-vram", 0, "music: VRAM held back for the display")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")

	// generate-audio takes TWO positionals (out path, text); the rest are flags.
	out, text, flagArgs := splitTwoArgs(args, map[string]bool{
		"config": true, "kind": true, "voice": true, "clone": true, "lang": true,
		"seconds": true, "seed": true, "reserve-vram": true,
	})
	_ = fs.Parse(flagArgs)

	if out == "" || text == "" {
		return fmt.Errorf("generate-audio requires an output path and text: local-offload generate-audio <out.wav> \"<text>\" [--kind voice|music] [--lang es]")
	}

	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	params := buildAudioParams(audioFlags{
		kind: *kind, voice: *voice, clone: *clone, lang: *lang, out: out,
		seconds: *seconds, seed: *seed, reserveVRAM: *reserveVRAM,
	})
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateAudio,
		Input:  text,
		Params: params,
	})
	emitResult(res, *asJSON, "", *compactFlag)
	return nil
}

// videoFlags is the parsed CLI input for `generate-video`. Factored out so the
// param-building (buildVideoParams) is unit-testable without a live render.
type videoFlags struct {
	model       string
	still       string
	negative    string
	out         string
	frames      int
	width       int
	height      int
	steps       int
	seed        int
	reserveVRAM float64
	hero        bool
	upscale     bool
}

// buildVideoParams maps the parsed CLI flags to the pipeline params map for a
// generate_video request. Empty model defaults to "wan" (the PRIMARY I2V path — the
// best open-weight I2V that fits 16GB, and the only one whose files are complete on
// this box; Hunyuan 1.5 is the opt-in). The still path is always carried as "still".
// Zero/empty optional flags are omitted so the pipeline/runner apply their own
// defaults (the workflow builder's settings + the machine's videogen_* config + the
// runner's --reserve-vram). reserve_vram is stringified to match the MCP wire shape.
func buildVideoParams(f videoFlags) map[string]any {
	model := f.model
	if model == "" {
		model = "wan"
	}
	params := map[string]any{"model": model}
	if f.still != "" {
		params["still"] = f.still
	}
	if f.negative != "" {
		params["negative"] = f.negative
	}
	if f.out != "" {
		params["out"] = f.out
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
	if f.hero {
		params["hero"] = true
	}
	if f.upscale {
		params["upscale"] = true
	}
	return params
}

// runGenerateVideo handles `local-offload generate-video <out.mp4> <still.png>
// "<prompt>" [--model hunyuan|wan] [--frames 49] [--width N] [--height N]
// [--steps N] [--seed N] [--negative "..."] [--reserve-vram F] [--json]`. The
// THREE positionals are the output path, the input still (I2V needs an image),
// and the prompt — mirroring the raw `node render/comfy-video.mjs out.mp4 still.png
// "prompt"` CLI. It animates the still into a short clip on the LOCAL ComfyUI for
// free via the same runGenerateVideo pipeline branch the MCP tool uses. Steps,
// shift, and VAE temporal tiling are SETTLED inside the workflow builder and are
// intentionally NOT exposed here so a caller can't regress them.
func runGenerateVideo(args []string) error {
	fs := flag.NewFlagSet("generate-video", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	model := fs.String("model", "wan", "wan (default; Wan 2.2, 4-step LoRA — best 16GB photoreal I2V) | hunyuan (opt-in; needs Hunyuan 1.5 files)")
	negative := fs.String("negative", "", "hard exclusions, e.g. 'blurry, distorted'")
	frames := fs.Int("frames", 0, "frame count (default ~33; realistic ceiling ~49)")
	width := fs.Int("width", 0, "width px")
	height := fs.Int("height", 0, "height px")
	steps := fs.Int("steps", 0, "sampler steps (0 = builder default: wan 4 fast / 20 hero)")
	seed := fs.Int("seed", 0, "RNG seed for reproducibility")
	reserveVRAM := fs.Float64("reserve-vram", 0, "VRAM held back for the display (default per-workflow; ~2.0 for Hunyuan/Wan)")
	hero := fs.Bool("hero", false, "native no-LoRA quality pass (wan; slower, better motion for realistic b-roll)")
	upscale := fs.Bool("upscale", false, "post-decode upscale using this machine's configured upscale model (e.g. 720p->1080p)")
	compactFlag := fs.Bool("compact", false, "compact (minified) JSON output")

	// generate-video takes THREE positionals (out path, still image, prompt); the
	// rest are flags.
	out, still, prompt, flagArgs := splitThreeArgs(args, map[string]bool{
		"config": true, "model": true, "negative": true, "frames": true,
		"width": true, "height": true, "steps": true, "seed": true, "reserve-vram": true,
	})
	_ = fs.Parse(flagArgs)

	if out == "" || prompt == "" {
		return fmt.Errorf("generate-video requires an output path, a still image, and a prompt: local-offload generate-video <out.mp4> <still.png> \"<prompt>\" [--model hunyuan|wan] [--frames 49]")
	}

	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	params := buildVideoParams(videoFlags{
		model: *model, still: still, negative: *negative, out: out,
		frames: *frames, width: *width, height: *height, steps: *steps,
		seed: *seed, reserveVRAM: *reserveVRAM, hero: *hero, upscale: *upscale,
	})
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskGenerateVideo,
		Input:  prompt,
		Image:  still,
		Params: params,
	})
	emitResult(res, *asJSON, "", *compactFlag)
	return nil
}

// splitThreeArgs separates the FIRST THREE positionals (e.g. out path + still +
// prompt) from flags, treating the named value-flags as consuming the following
// token. Used by generate-video, whose CLI shape is `<out> <still> "<prompt>"
// [flags]`. A fourth+ positional is dropped onto flags (harmless; FlagSet ignores
// trailing positionals).
func splitThreeArgs(args []string, valueFlags map[string]bool) (first, second, third string, flags []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if strings.ContainsRune(name, '=') {
				continue
			}
			if valueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		switch {
		case first == "":
			first = a
		case second == "":
			second = a
		case third == "":
			third = a
		default:
			flags = append(flags, a)
		}
	}
	return first, second, third, flags
}

// splitTwoArgs separates the FIRST TWO positionals (e.g. out path + text) from
// flags, treating the named value-flags as consuming the following token. Used by
// generate-audio, whose CLI shape is `<out> "<text>" [flags]`. A third+ positional
// is dropped onto flags (harmless; FlagSet ignores trailing positionals).
func splitTwoArgs(args []string, valueFlags map[string]bool) (first, second string, flags []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if strings.ContainsRune(name, '=') {
				continue
			}
			if valueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		switch {
		case first == "":
			first = a
		case second == "":
			second = a
		default:
			flags = append(flags, a)
		}
	}
	return first, second, flags
}

// runOCR handles `local-offload ocr <image-path> [--json]`. Like vqa the
// positional argument is an IMAGE PATH (or data URI), not stdin text; there is no
// question — the task is fixed (transcribe all text). It returns {text:...}.
func runOCR(args []string) error {
	fs := flag.NewFlagSet("ocr", flag.ExitOnError)
	fs.String("config", "", "config file path")
	asJSON := fs.Bool("json", false, "print full result JSON")
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true,
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

	res := p.Run(context.Background(), core.Request{
		Task:  core.TaskOCR,
		Image: positional,
	})
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
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "brief": true,
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
	res := p.Run(context.Background(), core.Request{
		Task:   core.TaskAssessImage,
		Image:  positional,
		Params: params,
	})
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	printHuman(res)
	return nil
}

func printHuman(res core.Result) {
	if res.Deferred {
		fmt.Printf("DEFERRED (%s) — handle this yourself.\n", res.Reason)
		if res.Partial != "" {
			fmt.Println("partial:", truncate(res.Partial, 400))
		}
		return
	}
	var pretty any
	_ = json.Unmarshal(res.Data, &pretty)
	b, _ := json.MarshalIndent(pretty, "", "  ")
	fmt.Println(string(b))
	fmt.Printf("[%d in / %d out tok, %.0f tok/s, %dms%s]\n",
		res.Meta.TokensIn, res.Meta.TokensOut, res.Meta.TokPerSec, res.Meta.LatencyMs,
		map[bool]string{true: ", cache hit", false: ""}[res.Meta.CacheHit])
}

// emitResult prints a Result, optionally projecting its Data to the --select
// fields and/or minifying with --compact. This is the harness's fastcontext
// citation pattern at the output layer: the caller keeps only the fields it
// needs (e.g. `transcribe --select gist,srt_path` drops the verbose segments[]).
// selectCSV "" / compact false reproduces the prior plain behavior exactly.
func emitResult(res core.Result, asJSON bool, selectCSV string, compact bool) {
	if keys := core.SelectKeys(selectCSV); len(keys) > 0 {
		res.Data = core.ProjectFields(res.Data, keys)
	}
	if asJSON {
		var b []byte
		if compact {
			b, _ = json.Marshal(res)
		} else {
			b, _ = json.MarshalIndent(res, "", "  ")
		}
		fmt.Println(string(b))
		return
	}
	printHuman(res)
}

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	fs.String("config", "", "config file path")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	// A2 hot-reload: the MCP server is long-running, so a nightly retrain that
	// rewrites the self-learning artifacts would otherwise never go live without a
	// manual restart. Start the background poll reloader HERE (only for the MCP
	// server — CLI one-shots never start it, so they stay byte-identical and leak
	// no goroutine) and stop it on cleanup. It hot-swaps changed artifacts within
	// one tick, fail-open, with zero IO/parse added to the request path.
	stopReloader := p.StartReloader(0) // 0 => default interval
	defer stopReloader()
	return mcpserver.New(p).Run(context.Background(), version)
}

// nvidiaSmiMemory shells the global VRAM query (MiB CSV) that feeds both the
// fleet-serve startup probe and the 2s health sampler; fleetnode.ParseSmiMemory
// parses it.
func nvidiaSmiMemory() (string, error) {
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.total,memory.used", "--format=csv,noheader,nounits").Output()
	return string(out), err
}

// gpuArchFromName maps an nvidia-smi product name to the architecture CLASS
// the health payload advertises as gpu_arch — the dispatcher routes on arch
// classes, not product names ("NVIDIA GeForce RTX 3070 Laptop GPU" is not a
// schedulable fact; "ampere" is). Mirrors setup/detect.ps1's Get-GpuArch:
// note "RTX PRO" must match in its own right BEFORE any 50xx logic — the PRO
// branding ("RTX PRO 5000") breaks the "RTX 50" substring, and that branding
// IS the Blackwell pro generation (pre-Blackwell pro cards were "RTX A6000" /
// "RTX 6000 Ada Generation", which match neither rule). Unrecognized products
// (and an empty name from a failed query) fall back to the lowercase vendor
// "nvidia" — an honest "NVIDIA, generation unknown", never "".
func gpuArchFromName(name string) string {
	n := strings.ToUpper(name)
	switch {
	case strings.Contains(n, "RTX PRO"), strings.Contains(n, "BLACKWELL"), strings.Contains(n, "RTX 50"):
		return "blackwell"
	case strings.Contains(n, "RTX 40"):
		return "ada"
	case strings.Contains(n, "RTX 30"):
		return "ampere"
	case strings.Contains(n, "RTX 20"), strings.Contains(n, "GTX 16"):
		return "turing"
	case strings.Contains(n, "V100"):
		return "volta"
	default:
		return "nvidia"
	}
}

// nvidiaSmiGPUName returns the GPU product name feeding gpuArchFromName
// (best-effort: "" when the query fails — the probe already proved the GPU
// works, and the arch mapper turns "" into the vendor fallback).
func nvidiaSmiGPUName() string {
	out, err := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader").Output()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(out))
	if i := strings.IndexByte(name, '\n'); i >= 0 {
		name = strings.TrimSpace(name[:i])
	}
	return name
}

// fleetServeParams resolves + validates fleet-serve's identity/binding args —
// the testable seam (mirrors runGraphParams): flag > config for the listen
// address and node id, hostname fallback for an empty id, and the shared
// netguard loopback refusal unless --listen-trusted-network.
func fleetServeParams(listenFlag, nodeIDFlag string, trusted bool, cfg config.Config, hostname func() (string, error)) (listen, nodeID string, err error) {
	listen = listenFlag
	if listen == "" {
		listen = cfg.FleetListen
	}
	if listen == "" {
		listen = "127.0.0.1:18811" // config.Default's value; belt-and-suspenders for a zeroed config
	}
	if err := netguard.Validate(listen, trusted); err != nil {
		return "", "", err
	}
	nodeID = nodeIDFlag
	if nodeID == "" {
		nodeID = cfg.FleetNodeID
	}
	if nodeID == "" {
		if h, herr := hostname(); herr == nil && h != "" {
			nodeID = h
		} else {
			nodeID = "fleet-node"
		}
	}
	return listen, nodeID, nil
}

// runFleetServe joins this box to the fleet-dispatcher fleet (CONTRACT.md v2:
// /fleet/health, /fleet/dispatch, /fleet/jobs/{id}) on the same pipeline the
// MCP server drives. Refuses to start without a working NVIDIA GPU (a
// zero-VRAM node is a contract violation the dispatcher treats as broken);
// SIGINT drains in-flight jobs for up to 30s before exiting.
func runFleetServe(args []string) error {
	fs := flag.NewFlagSet("fleet-serve", flag.ExitOnError)
	fs.String("config", "", "config file path")
	listenFlag := fs.String("listen", "", "listen address (default: config fleet_listen, 127.0.0.1:18811)")
	trusted := fs.Bool("listen-trusted-network", false, "allow --listen to bind beyond loopback (the Tailscale address). The fleet endpoints are UNAUTHENTICATED; set this ONLY on a network you fully trust — and NEVER bind 0.0.0.0.")
	nodeIDFlag := fs.String("node-id", "", "node id advertised in /fleet/health (default: config fleet_node_id, else the hostname)")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)

	listen, nodeID, err := fleetServeParams(*listenFlag, *nodeIDFlag, *trusted, cfg, os.Hostname)
	if err != nil {
		return err
	}
	if *trusted {
		fmt.Fprintf(os.Stderr, "[fleet-serve] WARNING: --listen-trusted-network set — the UNAUTHENTICATED fleet "+
			"endpoints are exposed beyond loopback on %q. Anyone who can reach them can run renders on this GPU.\n", listen)
	}

	// Startup GPU probe: one nvidia-smi exec. Error or total <= 0 → refuse
	// loudly (ParseSmiMemory already rejects a non-positive total).
	probeOut, perr := nvidiaSmiMemory()
	if perr != nil {
		return fmt.Errorf("fleet-serve: GPU probe failed (nvidia-smi: %v) — refusing to start: a node without a working NVIDIA GPU cannot honor the fleet contract (vram_total_gb must be > 0)", perr)
	}
	total, _, perr := fleetnode.ParseSmiMemory(probeOut)
	if perr != nil {
		return fmt.Errorf("fleet-serve: GPU probe returned no usable VRAM (%v) — refusing to start: advertising a zero-VRAM node would make the dispatcher treat this box as broken", perr)
	}

	// Same pipeline construction as the mcp verb, incl. the hot-reloader (this
	// is a long-running server; nightly retrains must go live without a restart).
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	stopReloader := p.StartReloader(0)
	defer stopReloader()

	ctx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSig()

	sampler := fleetnode.StartGlobalSampler(ctx, 2*time.Second, nvidiaSmiMemory)
	jobs := fleetnode.NewJobs(time.Hour)
	srv := fleetnode.New(p, jobs, fleetnode.Options{
		NodeID:   nodeID,
		Snapshot: sampler.Load,
		Footprints: func() []fleetnode.FootprintEntry {
			if st := p.FootprintStore(); st != nil {
				// Pick up records written by OTHER processes (fleet-measure while
				// serving was the live-found case) — mtime-gated, max-merged.
				st.ReloadIfChanged()
				return st.Entries()
			}
			return nil
		},
		GpuVendor: "nvidia",
		GpuArch:   gpuArchFromName(nvidiaSmiGPUName()),
		Cfg:       cfg,
	})

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("fleet-serve: listen %s: %w", listen, err)
	}
	fmt.Fprintf(os.Stderr, "[fleet-serve] node %q serving /fleet on %s (%.1f GiB VRAM; tasks: %s)\n",
		nodeID, listen, total, strings.Join(fleetnode.SupportedTasks(cfg), ", "))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// Drain BEFORE closing the listener: new dispatches already 503, but
		// pollers can still read states while in-flight renders finish.
		fmt.Fprintln(os.Stderr, "[fleet-serve] interrupt — draining jobs (up to 30s); survivors are marked error:\"interrupted\"")
		jobs.DrainAndStop(30 * time.Second)
		ln.Close()
		<-errCh
		return nil
	}
}

// runFleetMeasure primes an empty footprint store: one minimal render per
// configured task through the NORMAL pipeline (so the passive gpugen hook
// records exactly what fleet jobs will), then prints the store's on-disk
// records as JSON. Tasks without a cheap universal probe (voice, run-graph)
// are skipped — their footprints accumulate passively during normal use.
func runFleetMeasure(args []string) error {
	fs := flag.NewFlagSet("fleet-measure", flag.ExitOnError)
	fs.String("config", "", "config file path")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx := context.Background()
	note := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "[fleet-measure] "+format+"\n", a...) }

	// image-gen: a small fast render (512² at 8 steps).
	var stillPath string
	if cfg.ImageGenScript != "" {
		note("image-gen: rendering 512x512 at 8 steps...")
		res := p.Run(ctx, core.Request{
			Task:   core.TaskGenerateImage,
			Input:  "fleet-measure probe: a plain gray sphere on a white background",
			Params: map[string]any{"width": 512, "height": 512, "steps": 8},
		})
		if res.OK {
			var out struct {
				ImagePath string `json:"image_path"`
			}
			_ = json.Unmarshal(res.Data, &out)
			stillPath = out.ImagePath
			note("image-gen: done (%s)", out.ImagePath)
		} else {
			note("image-gen: deferred: %s", res.Reason)
		}
	} else {
		note("image-gen: skipped (no imagegen_script configured)")
	}

	// video-gen: the FAST (distilled) recipe at the smallest frame count — the
	// slow native recipe is not a measurement tool. Reuses the probe image as
	// the I2V still when the image step produced one.
	if cfg.VideoGenScript != "" {
		note("video-gen: rendering the fast recipe at 9 frames...")
		params := map[string]any{"fast": true, "frames": 9}
		req := core.Request{Task: core.TaskGenerateVideo, Input: "fleet-measure probe: slow gentle camera pan", Params: params}
		if stillPath != "" {
			req.Image = stillPath
		}
		if res := p.Run(ctx, req); res.OK {
			note("video-gen: done")
		} else {
			note("video-gen: deferred: %s", res.Reason)
		}
	} else {
		note("video-gen: skipped (no videogen_script configured)")
	}

	// audio-gen: 5s of music (the ComfyUI ACE-Step path — the GPU-heavy one).
	if cfg.MusicGenScript != "" {
		note("audio-gen: rendering 5s of music...")
		res := p.Run(ctx, core.Request{
			Task:   core.TaskGenerateAudio,
			Input:  "fleet-measure probe: soft ambient pad, slow tempo",
			Params: map[string]any{"kind": "music", "seconds": 5},
		})
		if res.OK {
			note("audio-gen: done")
		} else {
			note("audio-gen: deferred: %s", res.Reason)
		}
	} else {
		note("audio-gen: skipped (no musicgen_script configured)")
	}
	note("audio-gen (voice): skipped — voice footprints accumulate passively during normal TTS use")
	note("run-graph: skipped — no universal probe graph; footprints accumulate passively per model_family")

	// Print the on-disk records (vram_peak_gb + observed_peak_gb/samples — the
	// observed value is what the Afterburner validation procedure compares).
	st := p.FootprintStore()
	if st == nil {
		fmt.Println("[]")
		return nil
	}
	raw, rerr := os.ReadFile(st.Path())
	if rerr != nil {
		fmt.Println("[]")
		return nil
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return nil
}

func runLedger(args []string) error {
	fs := flag.NewFlagSet("ledger", flag.ExitOnError)
	fs.String("config", "", "config file path")
	since := fs.Int("since", 0, "only count entries from the last N days")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	var sinceTS int64
	if *since > 0 {
		sinceTS = time.Now().AddDate(0, 0, -*since).Unix()
	}
	// Lock-free read: works even while the MCP server is appending to the ledger.
	s, err := ledger.SummarizeFile(cfg.LedgerPath, sinceTS, cfg.OpusInputPricePerMTok)
	if err != nil {
		return err
	}
	// LO-8: surface WHY calls deferred — top defer reasons over the last 7 days
	// (fixed window regardless of --since, so the recent-failure signal is
	// always visible). Best-effort: a read error must not sink the summary.
	reasons, rerr := ledger.TopDeferReasons(cfg.LedgerPath, time.Now().AddDate(0, 0, -7).Unix(), 5)
	if rerr != nil {
		fmt.Fprintln(os.Stderr, "note: defer-reason aggregation failed:", rerr)
	}
	out := struct {
		ledger.Summary
		TopDeferReasons7d []ledger.ReasonCount `json:"top_defer_reasons_7d,omitempty"`
	}{s, reasons}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	// LO-12: honest claim — this is the est. Opus-input VALUE of tokens kept
	// local, not literal billed savings. Math unchanged.
	fmt.Printf("tokens kept local (est.): %d (~$%.2f Opus-input value — an estimate, not billed savings)\n",
		s.TokensSaved, s.EstValueKeptLocal)
	return nil
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	fs.String("config", "", "config file path")
	_ = fs.Parse(args)
	cfg, src := loadCfgWithSource(fs)
	// Disclose the config source FIRST — and truthfully: SourceLine never credits a file
	// that was not actually read (not-found and failed-to-load both disclose defaults).
	// Every binding verdict below is only as good as the file that produced it.
	fmt.Fprintln(os.Stdout, config.SourceLine(src))
	return doctorRun(cfg, os.Stdout)
}

// aliasCheck pairs a config key with its configured llama-swap model alias.
type aliasCheck struct{ Key, Alias string }

// modelAliases enumerates EVERY llama-swap model alias the config routes to,
// with its config key. NIMModel is deliberately absent — it is a REMOTE
// endpoint's model id, never served by the local llama-swap.
func modelAliases(cfg config.Config) []aliasCheck {
	return []aliasCheck{
		{"model", cfg.Model},
		{"triage_model", cfg.TriageModel},
		{"escalation_model", cfg.EscalationModel},
		{"reasoning_model", cfg.ReasoningModel},
		{"vision_model", cfg.VisionModel},
		{"stt_model", cfg.STTModel},
		{"stt_model_hq", cfg.STTModelHQ},
	}
}

// doctorRun checks endpoint health, then diffs the LIVE /v1/models roster
// against every configured model alias (LO-11: doctor used to GET only
// /health, so a renamed/removed llama-swap alias passed doctor yet every call
// deferred). Any missing alias prints FAIL and the command exits non-zero.
func doctorRun(cfg config.Config, w io.Writer) error {
	fmt.Fprintf(w, "endpoint:   %s%s\nmodel:      %s\ncache:      %s\nledger:     %s\n",
		cfg.Endpoint, cfg.CompletionPath, cfg.Model, cfg.CachePath, cfg.LedgerPath)
	client := llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Health(ctx); err != nil {
		fmt.Fprintln(w, "health:     DOWN -", err)
		return fmt.Errorf("endpoint down: %w", err)
	}
	fmt.Fprintln(w, "health:     OK")
	roster, err := fetchModelRoster(ctx, cfg.Endpoint)
	if err != nil {
		fmt.Fprintln(w, "roster:     FAIL - cannot list /v1/models:", err)
		return err
	}
	missing := 0
	for _, a := range modelAliases(cfg) {
		switch {
		case a.Alias == "":
			fmt.Fprintf(w, "%-18s (unset)\n", a.Key+":")
		case roster[a.Alias]:
			fmt.Fprintf(w, "%-18s OK    %s\n", a.Key+":", a.Alias)
		default:
			fmt.Fprintf(w, "%-18s FAIL  %s — not in the live /v1/models roster\n", a.Key+":", a.Alias)
			missing++
		}
	}
	if missing > 0 {
		return fmt.Errorf("%d configured model alias(es) missing from %s/v1/models", missing, cfg.Endpoint)
	}
	return nil
}

// fetchModelRoster GETs <endpoint>/v1/models and returns the served model ids.
func fetchModelRoster(ctx context.Context, endpoint string) (map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/models: %s", resp.Status)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(body.Data))
	for _, m := range body.Data {
		ids[m.ID] = true
	}
	return ids, nil
}

func runModels(args []string) error {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	fs.String("config", "", "config file path")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	fmt.Print(modelsReport(cfg))
	return nil
}

// modelsReport renders the CURRENT configured model routes, built from live
// config values (LO-11: the old text hardcoded a stale tier roster — the
// prose still said gemma4-26b-a4b long after the default escalation moved to
// the escalation model — and printed serving flags this binary does not control).
func modelsReport(cfg config.Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "endpoint: %s\n\n", cfg.Endpoint)
	b.WriteString("Configured model routes (ascending cascade; verify the live roster with `local-offload doctor`):\n")
	fmt.Fprintf(&b, "  entry   triage,classify          -> %s  (triage_model)\n", orDash(cfg.TriageModel))
	fmt.Fprintf(&b, "  work    summarize,extract        -> %s  (model)\n", orDash(cfg.Model))
	fmt.Fprintf(&b, "  escal   on quality failure       -> %s  (escalation_model)\n", orDash(cfg.EscalationModel))
	fmt.Fprintf(&b, "  reason  grammar defers, pre-Opus -> %s  (reasoning_model)\n", orDash(cfg.ReasoningModel))
	fmt.Fprintf(&b, "  vision  vqa,ocr,assess_image     -> %s  (vision_model)\n", orUnset(cfg.VisionModel))
	fmt.Fprintf(&b, "  stt     transcribe               -> %s  (stt_model); hq: %s  (stt_model_hq)\n", orUnset(cfg.STTModel), orUnset(cfg.STTModelHQ))
	b.WriteString("  defer   all local tiers fail     -> structured defer to the caller (harness never calls cloud)\n")
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "(unset -> falls back to workhorse)"
	}
	return s
}

// orUnset annotates an empty non-cascade route (vision/stt), where unset means
// the task defers rather than falling back to the workhorse.
func orUnset(s string) string {
	if s == "" {
		return "(unset -> task defers)"
	}
	return s
}

// --- self-learning offline subcommands (run by offload-dream.ps1 or by hand) ---

func runCalibrate(args []string) error {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	fs.String("config", "", "config file path")
	out := fs.String("out", "", "output thresholds.json (default: config thresholds_path)")
	alpha := fs.Float64("alpha", 0.10, "default target error rate (per-task overrides via config target_error_rate)")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	_ = cfg.EnsureDirs()
	dst := *out
	if dst == "" {
		dst = cfg.ThresholdsPath
	}
	_, report, err := calibration.Run(cfg.LedgerPath, *alpha, cfg.TargetErrorRate, dst)
	if err != nil {
		return err
	}
	fmt.Println(report)
	fmt.Println("wrote", dst)
	return nil
}

func runHealth(args []string) error {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	fs.String("config", "", "config file path")
	out := fs.String("out", "", "output tier_overrides.json (default: config tier_overrides_path)")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	dst := *out
	if dst == "" {
		dst = cfg.TierOverridesPath
	}
	rep, err := health.Run(cfg.LedgerPath, dst)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(b))
	fmt.Println("wrote", dst)
	return nil
}

func runTrainRouter(args []string) error {
	fs := flag.NewFlagSet("train-router", flag.ExitOnError)
	fs.String("config", "", "config file path")
	out := fs.String("out", "", "output router-weights.json (default: config router_weights_path)")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	dst := *out
	if dst == "" {
		dst = cfg.RouterWeightsPath
	}
	rep, err := router.Train(cfg.LedgerPath, cfg.RouterLabelsPath, dst, cfg.TriageModel)
	if err != nil {
		return err
	}
	fmt.Println(rep)
	fmt.Println("wrote", dst)
	return nil
}

// runShadowLabel drains the shadow queue, runs the escalation tier on each item
// (counterfactual), judges agreement vs the stored entry output, and appends a
// confhead label row to cfg.ConfHeadLabelsPath. It caps processing at --n items
// (default 200). Only confhead-labels.jsonl is written; the savings ledger is
// never touched.
func runShadowLabel(args []string) error {
	fs := flag.NewFlagSet("shadow-label", flag.ExitOnError)
	fs.String("config", "", "config file path")
	n := fs.Int("n", 200, "max items to label this run")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)

	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return fmt.Errorf("shadow-label: open pipeline: %w", err)
	}
	defer cleanup()

	items, err := shadow.Drain(cfg.ShadowQueuePath)
	if err != nil {
		return fmt.Errorf("shadow-label: drain queue: %w", err)
	}
	if len(items) == 0 {
		fmt.Println("shadow-label: queue empty")
		return nil
	}

	emb := judge.NewEmbedder(cfg.Endpoint, cfg.EmbedModel(), 30*time.Second)
	deps := shadow.LabelDeps{
		Escalation:            cfg.EscalationModel,
		E2B:                   cfg.TriageModel,
		RunTier:               p.RunTier,
		AnswersAgree:          pipeline.AnswersAgree,
		Ground:                grounding.Check,
		Similar:               emb.Similar,
		SummarizeSimThreshold: cfg.SummarizeSimThreshold,
		AppendLabel:           ledger.AppendLabel,
		LabelsPath:            cfg.ConfHeadLabelsPath,
		RouterLabelsPath:      cfg.RouterLabelsPath,
	}
	// meta-router v2: build the kNN entry-tier substrate during the drain, only
	// when the feature is enabled (one flag controls the whole feature).
	if cfg.KNNPreFilterEnabled {
		deps.Embed = emb.Embed
		deps.AppendKNN = func(task string, vec []float64, accept bool) error {
			return knn.Append(cfg.KNNIndexPath, knn.Row{Task: task, Vec: vec, Accept: accept})
		}
	}
	routerBefore := countJSONLLines(cfg.RouterLabelsPath)
	w := shadow.LabelQueue(context.Background(), items, *n, deps)
	routerWritten := countJSONLLines(cfg.RouterLabelsPath) - routerBefore
	fmt.Printf("shadow-label: drained %d items, wrote %d confhead labels -> %s\n", len(items), w, cfg.ConfHeadLabelsPath)
	fmt.Printf("shadow-label: wrote %d router labels -> %s\n", routerWritten, cfg.RouterLabelsPath)
	if cfg.KNNPreFilterEnabled {
		fmt.Printf("shadow-label: kNN substrate now %d rows -> %s\n", countJSONLLines(cfg.KNNIndexPath), cfg.KNNIndexPath)
	}
	return nil
}

// runAgentTrajectoryLabel drains the P6 agent-trajectory capture queue, judges each
// trajectory for GOAL SATISFACTION (a local triage — the raw StopReason "done" only
// means the model stopped calling tools, not that it reached the goal), and appends
// a label per item to the trajectory-label sidecar. The judge runs on a RECORDLESS
// pipeline (nil cache + nil ledger): the savings ledger is never opened or written —
// only the sidecar grows. A single un-judgeable item is skipped, never aborts.
func runAgentTrajectoryLabel(args []string) error {
	fs := flag.NewFlagSet("agent-trajectory-label", flag.ExitOnError)
	fs.String("config", "", "config file path")
	n := fs.Int("n", 500, "max trajectories to label this run (<=0 = no limit)")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)

	items, err := trajectory.Drain(cfg.AgentTrajectoryQueuePath)
	if err != nil {
		return fmt.Errorf("agent-trajectory-label: drain queue: %w", err)
	}
	if len(items) == 0 {
		fmt.Println("agent-trajectory-label: trajectory queue empty")
		return nil
	}
	// A recordless pipeline (nil cache + nil ledger): the judge's RunTier calls write
	// NOTHING and never open the savings ledger — labels go to the sidecar only. The
	// single shared constructor keeps the nil-store invariant from drifting.
	ap := pipeline.NewRecordlessPipeline(cfg, time.Duration(cfg.RequestTimeoutSec)*time.Second)
	judgeModel := cfg.EscalationModel
	if judgeModel == "" {
		judgeModel = cfg.Model
	}
	w := trajectory.LabelQueue(context.Background(), items, *n, trajectory.LabelDeps{
		RunTier:     ap.RunTier,
		JudgeModel:  judgeModel,
		AppendLabel: trajectory.AppendLabel,
		LabelsPath:  cfg.AgentTrajectoryLabelsPath,
	})
	fmt.Printf("agent-trajectory-label: drained %d trajectories, wrote %d goal-satisfaction labels -> %s\n", len(items), w, cfg.AgentTrajectoryLabelsPath)
	return nil
}

// runAgentTrajectoryGate is the P6 flywheel adoption-gate DRY RUN: it re-runs each
// labeled READ-ONLY trajectory under a CANDIDATE planner prompt in a side-effect-
// free replay (a read-only agent.Build — no write/fetch/shell tools registered —
// with the candidate prompt), judges goal satisfaction, and reports the paired
// bootstrap delta (candidate − incumbent) + verdict (ADOPT-eligible iff ci_lo>0,
// BLOCK iff ci_hi<0). It ADOPTS NOTHING and is ledger-pristine (recordless
// pipelines throughout). Effectful trajectories (write/shell/fetch) are excluded —
// a side-effect-free replay cannot fairly re-run them.
func runAgentTrajectoryGate(args []string) error {
	fs := flag.NewFlagSet("agent-trajectory-gate", flag.ExitOnError)
	fs.String("config", "", "config file path")
	candidatePath := fs.String("candidate-prompt", "", "path to a candidate planner system-prompt file to evaluate")
	root := fs.String("root", ".", "read root the replay agent may read (read-only)")
	n := fs.Int("n", 200, "max labeled trajectories to replay (<=0 = all)")
	_ = fs.Parse(args)
	if *candidatePath == "" {
		return fmt.Errorf("agent-trajectory-gate requires --candidate-prompt <file>")
	}
	candidate, err := os.ReadFile(*candidatePath)
	if err != nil {
		return fmt.Errorf("read candidate prompt: %w", err)
	}
	cfg := loadCfg(fs)

	labels, err := trajectory.ReadLabels(cfg.AgentTrajectoryLabelsPath)
	if err != nil {
		return fmt.Errorf("read trajectory labels (%s): %w", cfg.AgentTrajectoryLabelsPath, err)
	}
	// Only READ-ONLY trajectories are fairly replayable side-effect-free.
	var gateable []trajectory.Label
	for _, l := range labels {
		if !hasEffectfulCap(l.Envelope) {
			gateable = append(gateable, l)
		}
	}
	if *n > 0 && len(gateable) > *n {
		gateable = gateable[:*n]
	}
	if len(gateable) < 2 {
		fmt.Printf("agent-trajectory-gate: need >=2 read-only labeled trajectories to gate; have %d\n", len(gateable))
		return nil
	}

	timeout := time.Duration(cfg.RequestTimeoutSec) * time.Second
	plannerModel := cfg.Model
	judgeModel := cfg.EscalationModel
	if judgeModel == "" {
		judgeModel = cfg.Model
	}
	// The replay agent: READ-ONLY (no write/fetch/shell tools => side-effect-free)
	// with the CANDIDATE prompt. Offload + judge run on recordless (nil-store)
	// pipelines, so the whole dry run is ledger-pristine.
	absRoot, _ := filepath.Abs(*root)
	built, err := agent.Build(agent.BuildConfig{
		PlannerBase:          cfg.Endpoint,
		Model:                plannerModel,
		Timeout:              timeout,
		ReadRoot:             absRoot,
		Offload:              pipeline.NewRecordlessOffload(cfg, plannerModel, timeout),
		SystemPromptOverride: string(candidate),
	})
	if err != nil {
		return fmt.Errorf("build replay agent: %w", err)
	}
	judgePipe := pipeline.NewRecordlessPipeline(cfg, timeout)

	var inc, cand []float64
	incReached, candReached := 0, 0
	ctx := context.Background()
	for _, lab := range gateable {
		res, rerr := built.Loop.Run(ctx, lab.Goal)
		if rerr != nil {
			continue // a replay failure drops that pair, never aborts the gate
		}
		decision, _, ok := trajectory.JudgeGoalReached(ctx, judgePipe.RunTier, judgeModel, lab.Goal, res.Output)
		if !ok {
			continue
		}
		reached := decision == "yes"
		inc = append(inc, b2f(lab.GoalReached))
		cand = append(cand, b2f(reached))
		if lab.GoalReached {
			incReached++
		}
		if reached {
			candReached++
		}
	}
	m := len(inc)
	if m < 2 {
		fmt.Printf("agent-trajectory-gate: only %d comparable pairs after replay; need >=2\n", m)
		return nil
	}
	delta, lo, hi := eval.BootstrapDeltaMean(inc, cand, 2000, 42)
	verdict := "INCONCLUSIVE (95% CI spans 0 — do not adopt)"
	switch {
	case lo > 0:
		verdict = "ADOPT-ELIGIBLE (candidate strictly better; ci_lo>0)"
	case hi < 0:
		verdict = "BLOCK (candidate regresses; ci_hi<0)"
	}
	fmt.Printf("agent-trajectory-gate DRY RUN (nothing adopted) — %d read-only trajectories replayed under %s\n", m, *candidatePath)
	fmt.Printf("  incumbent goal-reached: %d/%d (%.0f%%) | candidate: %d/%d (%.0f%%)\n",
		incReached, m, 100*float64(incReached)/float64(m), candReached, m, 100*float64(candReached)/float64(m))
	fmt.Printf("  delta (candidate - incumbent) = %+.3f   95%% CI [%+.3f, %+.3f]\n", delta, lo, hi)
	fmt.Printf("  VERDICT: %s\n", verdict)
	fmt.Println("  caveats: incumbent = label-time verdict (not re-judged); only read-only trajectories gated; small-N CI is wide")
	fmt.Println("  (dry run — no prompt adopted; live adoption is P6c, operator-gated)")
	return nil
}

// hasEffectfulCap reports whether a captured envelope granted a side-effecting
// capability (write/fetch/shell) — such trajectories are excluded from the gate
// because a side-effect-free replay cannot fairly re-run them.
func hasEffectfulCap(envelope []string) bool {
	for _, c := range envelope {
		if c == "write" || c == "fetch" || c == "shell" {
			return true
		}
	}
	return false
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// countJSONLLines returns the number of newline-terminated lines in a JSONL file
// (0 if absent/unreadable). Used to report how many router-sidecar labels a
// shadow-label run wrote, since LabelQueue's return value counts only confhead labels.
func countJSONLLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

func runTrainConfHead(args []string) error {
	fs := flag.NewFlagSet("train-confhead", flag.ExitOnError)
	fs.String("config", "", "config file path")
	out := fs.String("out", "", "output confhead-weights.json (default: config confhead_path)")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	dst := *out
	if dst == "" {
		dst = cfg.ConfHeadPath
	}
	rep, err := confhead.Train(cfg.LedgerPath, cfg.ConfHeadLabelsPath, dst)
	if err != nil {
		return err
	}
	fmt.Println(rep)
	fmt.Println("wrote", dst)
	return nil
}

// confheadVerdict returns "ADOPT" when the bootstrap CI lower bound strictly
// exceeds zero (the head provably lowers AURC), otherwise "REJECT".
// AUGRC and ECE are DIAGNOSTIC only and must NEVER be passed to or read by
// this function — the verdict depends solely on lo.
func confheadVerdict(lo float64) string {
	if lo > 0 {
		return "ADOPT"
	}
	return "REJECT"
}

// runConfheadEval is the Phase 2 confhead ADOPTION GATE: a rigorous, leakage-free
// validation that decides whether the per-task correctness head is good enough to
// adopt. For each task with enough labeled rows it computes out-of-fold p(correct),
// compares the head's AURC against the incumbent confidence's AURC with a paired
// bootstrap CI, and verdicts ADOPT only if the CI lower bound excludes zero (the
// head PROVABLY lowers AURC). Tasks with too few labels are reported as skipped.
func runConfheadEval(args []string) error {
	fs := flag.NewFlagSet("confhead-eval", flag.ExitOnError)
	fs.String("config", "", "config file path")
	k := fs.Int("k", 5, "out-of-fold folds")
	b := fs.Int("bootstrap", 2000, "bootstrap resamples")
	minFold := fs.Int("fold-min-rows", 40, "min rows to train a head within a fold")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)

	rows, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil {
		return fmt.Errorf("confhead-eval: read ledger: %w", err)
	}
	labels, err := ledger.ReadLabelFile(cfg.ConfHeadLabelsPath)
	if err != nil {
		return fmt.Errorf("confhead-eval: read labels: %w", err)
	}
	all := append(rows, labels...)

	// Group LABELED, non-cache-hit rows per task.
	byTask := map[string][]ledger.Entry{}
	for _, e := range all {
		if e.CacheHit {
			continue
		}
		if _, ok := confhead.Label(e); !ok {
			continue
		}
		task := strings.ToLower(e.Task)
		byTask[task] = append(byTask[task], e)
	}

	const seed = 1
	usable := 2 * (*minFold) // need at least this many labeled rows to attempt the gate

	type result struct {
		N             int      `json:"n"`
		BaseError     *float64 `json:"base_error,omitempty"`
		AURCIncumbent *float64 `json:"aurc_incumbent,omitempty"`
		AURCHeadOOF   *float64 `json:"aurc_head_oof,omitempty"`
		AUGRCHead     *float64 `json:"augrc_head,omitempty"`
		ECEHead       *float64 `json:"ece_head,omitempty"` // diagnostic only — never read by verdict
		Delta         *float64 `json:"delta,omitempty"`
		CILo          *float64 `json:"ci_lo,omitempty"`
		CIHi          *float64 `json:"ci_hi,omitempty"`
		Verdict       string   `json:"verdict,omitempty"`
		Note          string   `json:"note,omitempty"`
	}

	out := map[string]result{}
	tasks := make([]string, 0, len(byTask))
	for t := range byTask {
		tasks = append(tasks, t)
	}
	sort.Strings(tasks)

	for _, task := range tasks {
		subset := byTask[task]
		n := len(subset)
		if n < usable {
			out[task] = result{N: n, Note: "insufficient labels"}
			continue
		}

		// Per-row labels and incumbent confidence (Margin; 0 for summarize/extract
		// => no-skill baseline). Captured up front so OOF scoring and the bootstrap
		// share the same aligned arrays.
		correct := make([]bool, n)
		margins := make([]float64, n)
		nWrong := 0
		for i, e := range subset {
			y, _ := confhead.Label(e)
			correct[i] = y == 1
			if !correct[i] {
				nWrong++
			}
			margins[i] = e.Margin
		}

		// Out-of-fold p(correct): for each fold, fit a head on the TRAINING subset
		// and score the held-out rows. A Predict sentinel (-1: no head for this fold)
		// becomes 0.5 = no-skill, so a fold that fails to train can't fake a signal.
		oofHead := eval.KFoldOOF(n, *k, seed,
			func(train []int) *confhead.Model {
				trainSubset := make([]ledger.Entry, 0, len(train))
				for _, ti := range train {
					trainSubset = append(trainSubset, subset[ti])
				}
				return confhead.FitWithMinRows(trainSubset, *minFold)
			},
			func(m *confhead.Model, i int) float64 {
				p := m.Predict(task, confhead.FeatureRow(subset[i]))
				if p < 0 {
					return 0.5
				}
				return p
			},
		)

		incPts := make([]eval.RCPoint, n)
		headPts := make([]eval.RCPoint, n)
		for i := 0; i < n; i++ {
			incPts[i] = eval.RCPoint{Confidence: margins[i], Correct: correct[i]}
			headPts[i] = eval.RCPoint{Confidence: oofHead[i], Correct: correct[i]}
		}
		_, _, aurcInc, _ := eval.RiskCoverage(incPts)
		_, _, aurcHead, _ := eval.RiskCoverage(headPts)
		augrc := eval.AUGRC(headPts)
		ece := eval.ECE(headPts, 10) // diagnostic: Expected Calibration Error (10 bins)
		delta, lo, hi := eval.BootstrapDeltaAURC(margins, oofHead, correct, *b, seed)

		// Verdict is driven solely by the CI lower bound — augrc and ece are
		// diagnostic outputs and are NEVER passed to confheadVerdict.
		verdict := confheadVerdict(lo)
		baseErr := float64(nWrong) / float64(n)
		out[task] = result{
			N: n, BaseError: &baseErr,
			AURCIncumbent: &aurcInc, AURCHeadOOF: &aurcHead, AUGRCHead: &augrc, ECEHead: &ece,
			Delta: &delta, CILo: &lo, CIHi: &hi, Verdict: verdict,
		}
	}

	enc, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(enc))
	return nil
}

// runConfheadCalibrate is the Phase 2 Task 3 threshold calibrator. For each task
// with enough labeled rows it computes OUT-OF-FOLD p(correct) (so the chosen
// threshold is not optimistic), then selects the smallest conformal threshold tau
// on p(correct) whose ACCEPTED-set error meets the per-task target. At runtime
// (Task 4, separate) the pipeline escalates a summarize call to the larger tier
// when p(correct) < tau. This job is OFFLINE only: it writes a thresholds file and
// does not touch the live pipeline. Tasks with too few labels are omitted.
func runConfheadCalibrate(args []string) error {
	fs := flag.NewFlagSet("confhead-calibrate", flag.ExitOnError)
	fs.String("config", "", "config file path")
	k := fs.Int("k", 5, "out-of-fold folds")
	minFold := fs.Int("fold-min-rows", 40, "min rows to train a head within a fold")
	out := fs.String("out", "", "output confhead-thresholds.json (default: config confhead_thresholds_path)")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	_ = cfg.EnsureDirs()
	dst := *out
	if dst == "" {
		dst = cfg.ConfHeadThresholdsPath
	}

	rows, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil {
		return fmt.Errorf("confhead-calibrate: read ledger: %w", err)
	}
	labels, err := ledger.ReadLabelFile(cfg.ConfHeadLabelsPath)
	if err != nil {
		return fmt.Errorf("confhead-calibrate: read labels: %w", err)
	}
	all := append(rows, labels...)

	// Group LABELED, non-cache-hit rows per task.
	byTask := map[string][]ledger.Entry{}
	for _, e := range all {
		if e.CacheHit {
			continue
		}
		if _, ok := confhead.Label(e); !ok {
			continue
		}
		task := strings.ToLower(e.Task)
		byTask[task] = append(byTask[task], e)
	}

	const (
		seed             = 1
		defaultTargetErr = 0.15 // per-task overrides via config target_error_rate
	)
	usable := 2 * (*minFold) // need at least this many labeled rows to attempt calibration

	thresholds := map[string]float64{}
	tasks := make([]string, 0, len(byTask))
	for t := range byTask {
		tasks = append(tasks, t)
	}
	sort.Strings(tasks)

	type calibDiag struct {
		N                   int     `json:"n"`
		Tau                 float64 `json:"tau"`
		Target              float64 `json:"target"`
		RealizedAcceptedErr float64 `json:"realized_accepted_err"` // diagnostic: OOF accepted-set error at tau
		BaseErr             float64 `json:"base_err"`
		EscalationRate      float64 `json:"escalation_rate"`
	}
	diags := map[string]calibDiag{}

	var sb strings.Builder
	sb.WriteString("confhead threshold calibration (out-of-fold)\n")
	for _, task := range tasks {
		subset := byTask[task]
		n := len(subset)
		if n < usable {
			fmt.Fprintf(&sb, "  %-12s  n=%4d  <%d labeled — skipped (omitted from thresholds)\n", task, n, usable)
			continue
		}

		// Per-row correctness label.
		correct := make([]bool, n)
		nWrong := 0
		for i, e := range subset {
			y, _ := confhead.Label(e)
			correct[i] = y == 1
			if !correct[i] {
				nWrong++
			}
		}

		// Out-of-fold p(correct): for each fold, fit on the TRAINING subset and
		// score the held-out rows. A Predict sentinel (-1: no head for this fold)
		// becomes 0.5 = no-skill, so a fold that fails to train can't fake a signal.
		oof := eval.KFoldOOF(n, *k, seed,
			func(train []int) *confhead.Model {
				trainSubset := make([]ledger.Entry, 0, len(train))
				for _, ti := range train {
					trainSubset = append(trainSubset, subset[ti])
				}
				return confhead.FitWithMinRows(trainSubset, *minFold)
			},
			func(m *confhead.Model, i int) float64 {
				p := m.Predict(task, confhead.FeatureRow(subset[i]))
				if p < 0 {
					return 0.5
				}
				return p
			},
		)

		target := defaultTargetErr
		if a, ok := cfg.TargetErrorRate[task]; ok {
			target = a
		}
		tau := confhead.SelectThreshold(oof, correct, target)
		thresholds[task] = tau

		// Resulting accepted-error and escalation-rate at tau on the OOF scores.
		var nAccepted, nAccWrong int
		for i, s := range oof {
			if s >= tau {
				nAccepted++
				if !correct[i] {
					nAccWrong++
				}
			}
		}
		baseErr := float64(nWrong) / float64(n)
		accErr := 0.0
		if nAccepted > 0 {
			accErr = float64(nAccWrong) / float64(nAccepted)
		}
		escRate := float64(n-nAccepted) / float64(n)
		fmt.Fprintf(&sb, "  %-12s  n=%4d  base_err=%.4f  target=%.3f  tau=%.4f  accepted_err=%.4f  escalation_rate=%.4f\n",
			task, n, baseErr, target, tau, accErr, escRate)
		diags[task] = calibDiag{N: n, Tau: tau, Target: target, RealizedAcceptedErr: accErr, BaseErr: baseErr, EscalationRate: escRate}
	}

	raw, err := json.MarshalIndent(thresholds, "", "  ")
	if err != nil {
		return fmt.Errorf("confhead-calibrate: marshal thresholds: %w", err)
	}
	// Atomic write (P4): the long-running MCP server polls confhead-thresholds.json
	// and must only ever read a COMPLETE file. tmp+rename (atomic on the same
	// filesystem) so the reloader never sees a half-written threshold map.
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("confhead-calibrate: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("confhead-calibrate: rename %s: %w", dst, err)
	}

	fmt.Print(sb.String())
	fmt.Println("wrote", dst)
	// Print JSON diagnostics (realized_accepted_err vs target_error_rate per task).
	// This is a diagnostic-only report; the threshold file written above is unchanged.
	if len(diags) > 0 {
		enc, _ := json.MarshalIndent(diags, "", "  ")
		fmt.Println(string(enc))
	}
	return nil
}

func runOptimize(args []string) error {
	fs := flag.NewFlagSet("optimize", flag.ExitOnError)
	fs.String("config", "", "config file path")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	for _, t := range []string{"summarize", "classify", "extract", "triage"} {
		rep, err := exemplars.Select(cfg.ExemplarsDir, t)
		if err != nil {
			fmt.Fprintln(os.Stderr, t+":", err)
			continue
		}
		fmt.Println(rep)
	}
	return nil
}

func runAuditSample(args []string) error {
	fs := flag.NewFlagSet("audit-sample", flag.ExitOnError)
	fs.String("config", "", "config file path")
	n := fs.Int("n", 20, "number of cases to surface")
	hard := fs.Bool("hard", false, "only hard cases (deferred / ungrounded / low margin)")
	asJSON := fs.Bool("json", false, "emit JSON (for codex/Opus review)")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	entries, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil {
		return err
	}
	var picked []ledger.Entry
	for _, e := range entries {
		if e.CacheHit {
			continue
		}
		isHard := e.Deferred || (e.Grounded != nil && !*e.Grounded) || (e.Margin > 0 && e.Margin < cfg.ConfidenceMarginThreshold)
		if !*hard || isHard {
			picked = append(picked, e)
		}
	}
	if len(picked) > *n {
		picked = picked[len(picked)-*n:]
	}
	if *asJSON {
		b, _ := json.MarshalIndent(picked, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	for _, e := range picked {
		g := "-"
		if e.Grounded != nil {
			g = fmt.Sprintf("%v", *e.Grounded)
		}
		fmt.Printf("[%-9s] tier=%-14s margin=%.2f deferred=%v grounded=%s err=%s\n", e.Task, e.ModelTier, e.Margin, e.Deferred, g, e.ErrClass)
	}
	fmt.Printf("(%d cases)\n", len(picked))
	return nil
}

func readInput(arg string) (string, error) {
	if arg == "" || arg == "-" {
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	b, err := os.ReadFile(arg)
	return string(b), err
}

// splitArgs separates the first positional (input path) from flags, treating
// the named value-flags as consuming the following token. This lets the input
// path appear before flags on the command line.
func splitArgs(args []string, valueFlags map[string]bool) (positional string, flags []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-" { // stdin indicator is a positional, not a flag
			if positional == "" {
				positional = a
			} else {
				flags = append(flags, a)
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if strings.ContainsRune(name, '=') {
				continue
			}
			if valueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if positional == "" {
			positional = a
		} else {
			flags = append(flags, a)
		}
	}
	return positional, flags
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func runEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	fs.String("config", "", "config file path")
	dir := fs.String("dir", "testdata/eval", "gold-set dir of <task>.jsonl files")
	confheadAB := fs.Bool("confhead-ab", false, "A1 decision-gate: paired confhead ON-vs-OFF frontier A/B (read-only; never enables the flag)")
	stagedDir := fs.String("staged-dir", filepath.FromSlash(".testrun/bootstrap"), "confhead-ab: dir holding staged confhead-weights.json + confhead-thresholds.json")
	eps := fs.Float64("eps", 0.0, "confhead-ab: max tolerated selective-acc drop for a frontier win")
	costBudget := fs.Float64("cost-budget", 1.0, "confhead-ab: max ON/OFF avg-cost ratio for a frontier win (1.0 = no increase)")
	calibTarget := fs.Float64("calib-target", 0.15, "confhead-ab: per-task target error for the calibrated-margin baseline threshold")
	gate1Adopt := fs.Bool("gate1-adopt", false, "confhead-ab: gate-1 (confhead-eval) ADOPT outcome; ENABLE requires this AND all tasks frontier_win")
	abForceOffOn := fs.Bool("ab-force-off-on", false, "confhead-ab TEST seam: force the ON arm confhead-OFF (OFF/OFF determinism check)")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)

	var cases []eval.Case
	for _, t := range []string{"summarize", "classify", "extract", "triage"} {
		cs, err := eval.LoadCases(filepath.Join(*dir, t+".jsonl"))
		if err != nil {
			return err
		}
		cases = append(cases, cs...)
	}
	if len(cases) == 0 {
		return fmt.Errorf("no gold cases under %s", *dir)
	}

	if *confheadAB {
		return runConfheadAB(cfg, cases, *stagedDir, *eps, *costBudget, *calibTarget, *gate1Adopt, *abForceOffOn)
	}

	run := func(c config.Config) []eval.Outcome {
		p, cleanup, err := openPipeline(c)
		if err != nil {
			return nil
		}
		defer cleanup()
		return eval.Run(context.Background(), p, cases)
	}

	// Operating point 1: entry-tier only (escalation off) — the cheap point.
	entryCfg := cfg
	entryCfg.EscalationModel = ""
	entryOut := run(entryCfg)
	// Operating point 2: full cascade — the expensive point.
	fullOut := run(cfg)
	// Answer-always (escalation off + confidence gates off) so every triage/
	// classify case yields a (Margin, correct) pair for the AURC ranking.
	ansCfg := cfg
	ansCfg.EscalationModel = ""
	ansCfg.ConfidenceMarginThreshold = 0
	ansCfg.ClassifyMinConfidence = 0
	ansOut := run(ansCfg)

	entryRep := eval.Aggregate(entryOut)
	fullRep := eval.Aggregate(fullOut)
	avgCost := func(r eval.Report) float64 {
		if r.N == 0 {
			return 0
		}
		return float64(r.TokensOut) / float64(r.N)
	}

	type taskMetrics struct {
		Coverage     float64 `json:"coverage"`      // 1 - defer_rate (full cascade)
		SelectiveAcc float64 `json:"selective_acc"` // accuracy among answered (full)
		AUDC         float64 `json:"audc"`          // cascade cost-quality area
		QNC          float64 `json:"qnc"`           // min norm cost to match peak quality
		PeakQuality  float64 `json:"peak_quality"`
		AURC         float64 `json:"aurc,omitempty"` // selective prediction (triage/classify)
		EAURC        float64 `json:"eaurc,omitempty"`
		AURCApplies  bool    `json:"aurc_applies"`
	}

	out := map[string]taskMetrics{}
	for _, task := range eval.SortedTasks(fullRep) {
		fr, er := fullRep[task], entryRep[task]
		audc, qnc, peak := eval.DeferralCurve([]eval.OpPoint{
			{Label: "entry", Cost: avgCost(er), Quality: er.AccuracyAccepted},
			{Label: "full", Cost: avgCost(fr), Quality: fr.AccuracyAccepted},
		})
		tm := taskMetrics{
			Coverage: 1 - fr.DeferRate, SelectiveAcc: fr.AccuracyAccepted,
			AUDC: audc, QNC: qnc, PeakQuality: peak,
		}
		if task == string(core.TaskTriage) || task == string(core.TaskClassify) {
			var rc []eval.RCPoint
			for _, o := range ansOut {
				if o.Case.Task == task && o.Accepted && o.Margin > 0 {
					rc = append(rc, eval.RCPoint{Confidence: o.Margin, Correct: o.Correct})
				}
			}
			if len(rc) > 0 {
				_, _, aurc, eaurc := eval.RiskCoverage(rc)
				tm.AURC, tm.EAURC, tm.AURCApplies = aurc, eaurc, true
			}
		}
		out[task] = tm
	}

	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return nil
}

func runStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	fs.String("config", "", "config file path")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	entries, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil {
		return err
	}
	m := report.Summarize(entries, cfg.ConfidenceMarginThreshold)
	b, _ := json.MarshalIndent(m, "", "  ")
	fmt.Println(string(b))
	return nil
}
