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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/local-offload-pp-cli/internal/cache"
	"github.com/dmmdea/local-offload-pp-cli/internal/calibration"
	"github.com/dmmdea/local-offload-pp-cli/internal/config"
	"github.com/dmmdea/local-offload-pp-cli/internal/core"
	"github.com/dmmdea/local-offload-pp-cli/internal/eval"
	"github.com/dmmdea/local-offload-pp-cli/internal/exemplars"
	"github.com/dmmdea/local-offload-pp-cli/internal/health"
	"github.com/dmmdea/local-offload-pp-cli/internal/ledger"
	"github.com/dmmdea/local-offload-pp-cli/internal/llamaclient"
	"github.com/dmmdea/local-offload-pp-cli/internal/mcpserver"
	"github.com/dmmdea/local-offload-pp-cli/internal/pipeline"
	"github.com/dmmdea/local-offload-pp-cli/internal/report"
	"github.com/dmmdea/local-offload-pp-cli/internal/confhead"
	"github.com/dmmdea/local-offload-pp-cli/internal/router"
)

const version = "0.1.0"

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
	case "ocr":
		err = runOCR(args)
	case "extract-image":
		err = runExtractImage(args)
	case "assess-image":
		err = runAssessImage(args)
	case "mcp":
		err = runMCP(args)
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
  local-offload ocr       <image-path> [--json]
  local-offload extract-image <image-path> --schema schema.json [--json]
  local-offload assess-image <image-path> [--brief "..."] [--json]
  local-offload mcp                      run as an MCP server (stdio)
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
	path := fs.Lookup("config").Value.String()
	if path == "" {
		path = os.Getenv("LOCAL_OFFLOAD_CONFIG")
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: config load failed, using defaults:", err)
	}
	return cfg
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
	ca, err := cache.Open(cfg.CachePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "note: cache unavailable (held by the MCP server?); continuing without cache")
		ca = nil
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
	// Go's flag pkg stops at the first positional, so split the input arg out
	// first and parse the remaining flags (allows `summarize <file> --json`).
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "labels": true, "question": true, "schema": true, "max-points": true,
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
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	printHuman(res)
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
	return mcpserver.New(p).Run(context.Background(), version)
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
	b, _ := json.MarshalIndent(s, "", "  ")
	fmt.Println(string(b))
	return nil
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	fs.String("config", "", "config file path")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	fmt.Printf("endpoint:   %s%s\nmodel:      %s\ncache:      %s\nledger:     %s\n",
		cfg.Endpoint, cfg.CompletionPath, cfg.Model, cfg.CachePath, cfg.LedgerPath)
	client := llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Health(ctx); err != nil {
		fmt.Println("health:     DOWN -", err)
		return nil
	}
	fmt.Println("health:     OK")
	return nil
}

func runModels(args []string) error {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	fs.String("config", "", "config file path")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	fmt.Printf("endpoint: %s\n\n", cfg.Endpoint)
	fmt.Println("Gemma-4 QAT family cascade (ascending capability; climbs on quality failure):")
	fmt.Printf("  fast   triage,classify -> %s  (~120 tok/s, entry tier)\n", orDash(cfg.TriageModel))
	fmt.Printf("  work   summarize,extract -> %s  (~83 tok/s, default workhorse)\n", orDash(cfg.Model))
	fmt.Printf("  escal  on validation/low-confidence -> %s  (MoE, ~16 tok/s, near-frontier)\n", orDash(cfg.EscalationModel))
	fmt.Println("  defer  all local tiers fail -> Opus (structured defer; harness never calls cloud)")
	fmt.Println()
	fmt.Println("Per-tier llama-server flags are grammar-reliable (NO MTP; MTP breaks GBNF):")
	fmt.Println("  -ngl 99|48|999(+--cpu-moe) --ctx-size 8192 --flash-attn on \\")
	fmt.Println("  --cache-type-k f16 --cache-type-v f16 --jinja --reasoning off")
	fmt.Println("served by llama-swap on :11436 (see ~/llama-swap/config.yaml)")
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "(unset -> falls back to workhorse)"
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
	rep, err := router.Train(cfg.LedgerPath, dst)
	if err != nil {
		return err
	}
	fmt.Println(rep)
	fmt.Println("wrote", dst)
	return nil
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
		delta, lo, hi := eval.BootstrapDeltaAURC(margins, oofHead, correct, *b, seed)

		verdict := "REJECT"
		if lo > 0 {
			verdict = "ADOPT"
		}
		baseErr := float64(nWrong) / float64(n)
		out[task] = result{
			N: n, BaseError: &baseErr,
			AURCIncumbent: &aurcInc, AURCHeadOOF: &aurcHead, AUGRCHead: &augrc,
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
	}

	raw, err := json.MarshalIndent(thresholds, "", "  ")
	if err != nil {
		return fmt.Errorf("confhead-calibrate: marshal thresholds: %w", err)
	}
	if err := os.WriteFile(dst, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("confhead-calibrate: write %s: %w", dst, err)
	}

	fmt.Print(sb.String())
	fmt.Println("wrote", dst)
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
		AURC         float64 `json:"aurc,omitempty"`  // selective prediction (triage/classify)
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
