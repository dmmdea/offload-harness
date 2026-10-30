// Command local-offload delegates short-context grunt work (summarize, classify,
// extract, triage) to a free local model, exposed as both a CLI and an MCP
// server. It never calls a cloud model: on failure it returns a structured
// defer so the caller (Claude) handles the task itself.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/dmmdea/offload-harness/internal/buildinfo"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/datahome"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/embedmemo"
	"github.com/dmmdea/offload-harness/internal/fleetnode"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/hostsample"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/mcpserver"
	"github.com/dmmdea/offload-harness/internal/mediacap"
	"github.com/dmmdea/offload-harness/internal/netguard"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
	"github.com/dmmdea/offload-harness/internal/research"
	"github.com/dmmdea/offload-harness/internal/rosterprobe"
	"github.com/dmmdea/offload-harness/internal/storesteward"
	"github.com/dmmdea/offload-harness/internal/swapclient"
)

// version aliases buildinfo.Version — the const moved to a shared package in
// 0.81.0 so the pipeline/fleet A1 stampers can read it without plumbing; the
// VERSION-file agreement test in main_test.go still binds through this alias.
const version = buildinfo.Version

// Keep config.example.json in lockstep with config.Default() (LO-17):
//go:generate go run ./cmd/genexample

// docs/tiers/ is rendered from setup/templates/profiles.json so the per-tier pages
// can never drift from the table that drives the installer (LO-17's lesson, applied
// to documentation): regenerate with `go generate ./...`; TestTierDocsAreCurrent
// fails the build if the checked-in tree is stale.
//go:generate go run ./cmd/gentiers .

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
	case "video-watch":
		err = runVideoWatch(args)
	case "transcribe":
		err = runTranscribe(args)
	case "forecast":
		err = runForecastCLI(args)
	case "generate-image":
		err = runGenerateImage(args)
	case "inpaint-image":
		err = runInpaintImage(args)
	case "upscale-image":
		err = runUpscaleImage(args)
	case "generate-svg":
		err = runGenerateSVG(args)
	case "generate-audio":
		err = runGenerateAudio(args)
	case "generate-video":
		err = runGenerateVideo(args)
	case "animate-character":
		err = runAnimateCharacter(args)
	case "run-graph":
		err = runRunGraph(args)
	case "edit-image":
		err = runEditImage(args)
	case "media":
		err = runMedia(args)
	case "compose-video":
		err = runComposeVideo(args)
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
	case "delegate":
		err = runDelegate(args)
	case "research":
		err = runResearch(args)
	case "fleet-serve":
		err = runFleetServe(args)
	case "forecast-serve":
		err = runForecastServe(args)
	case "fleet-measure":
		err = runFleetMeasure(args)
	case "fleet-ui":
		err = runFleetUI(args)
	case "fleet-smoke":
		err = runFleetSmoke(args)
	case "rig":
		err = runRig(args)
	case "top":
		err = runTop(args)
	case "ledger":
		err = runLedgerMode(args)
	// `loupe` is the identity-aware ledger view (memory-frontier Phase 0.1b).
	// Deliberately a SEPARATE command from `stats`: `stats` emits a stable
	// report.Summarize JSON that callers may already script against, and
	// silently changing its shape would break them.
	case "loupe":
		err = runLoupe(args)
	case "doctor":
		err = runDoctorBackends(args)
	case "audit-yaml":
		err = runAuditYAML(args)
	case "audit-config":
		err = runAuditConfig(args)
	case "agent-audit":
		err = runAgentAudit(args)
	case "report":
		err = runReport(args)
	case "acceptance":
		err = runAcceptance(args)
	case "install":
		err = runInstall(args)
	case "data":
		err = runData(args)
	case "models":
		err = runModels(args)
	case "calibrate":
		err = runCalibrate(args)
	case "health":
		err = runHealth(args)
	case "gpu":
		err = runGPU(args)
	case "node-swap":
		err = runNodeSwap(args)
	case "update":
		err = runUpdate(args)
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
	case "compaction-eval":
		err = runCompactionEval(args)
	case "gate":
		err = runGate(args)
	case "recipe-audit":
		err = runRecipeAudit(args)
	case "stats":
		err = runStats(args)
	case "trace-corpus":
		err = runTraceCorpus(args)
	case "version", "--version", "-v":
		fmt.Println("local-offload", version)
	case "--help", "-h", "help":
		// A requested help is not a mistake: the usage goes to stdout and the exit is 0.
		usageTo(os.Stdout)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		code, line := exitStatus(err)
		fmt.Fprintln(os.Stderr, line)
		os.Exit(code)
	}
}

// exitCodeError is a verb that ran to the end and already printed its answer, but whose status says
// the answer is not all good: it ends the process with its own exit code and a plain stderr line
// instead of exit 1 and "error: ...". generate-image --batch uses it for "every job ran and some
// failed" (imagegen.BatchExitJobsFailed, the code render/comfy-generate.mjs --batch ends with).
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }

// exitStatus is the exit code and the stderr line for a verb's error: its own code for an
// exitCodeError, else 1 and "error: <err>".
func exitStatus(err error) (int, string) {
	var ec *exitCodeError
	if errors.As(err, &ec) {
		return ec.code, ec.msg
	}
	return 1, "error: " + err.Error()
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

func usage() { usageTo(os.Stderr) }

// usageTo prints the usage on w: stderr for a mistake (exit 2), stdout for a requested help (exit 0).
func usageTo(w io.Writer) {
	fmt.Fprint(w, `local-offload `+version+` — delegate grunt work to a free local model

Usage:
  local-offload summarize <file|-> [--max-points N] [--json]
  local-offload classify  <file|-> --labels a,b,c [--json]
  local-offload extract   <file|-> --schema schema.json [--json]
  local-offload triage    <file|-> --question "..." [--json]
  local-offload vqa       <image-path> --question "..." [--json]
  local-offload video-describe <video-path> --question "..." [--json]
  local-offload video-watch <video-path> --question "..." [--window-sec 8] [--fps 1] [--max-frames N] [--frame-width N] [--start S] [--end S] [--no-synthesize] [--json]
                                         watch the WHOLE file window by window (video-describe sees only its first frames)
  local-offload transcribe <audio-path> [--language es] [--hq] [--json]
  local-offload forecast  (--series JSON | --series-path FILE.csv|.json) --prediction-length N [--target-columns A,B] [--past-covariate-columns ..] [--known-covariate-columns ..] [--quantiles 0.1,0.5,0.9] [--out-path F] [--route local|auto|remote] [--select a,b] [--compact]
  local-offload ocr       <image-path> [--json]
  local-offload extract-image <image-path> --schema schema.json [--json]
  local-offload assess-image <image-path> [--brief "..."] [--json]
  local-offload generate-audio <out> "<text>" [--kind voice|music] [--voice generalist|finetuned|endpoint] [--tts-voice NAME] [--clone ref.wav] [--lang es] [--seconds N] [--seed N] [--node ID|URL]
                                         a bare -- ends the flags, so text that starts with a dash is kept: generate-audio out.wav -- "--- Intro ---"
                                         (the same for generate-video, animate-character and the verbs that take one file, text or prompt)
  local-offload generate-image "<prompt>" [--negative "..."] [--width N] [--height N] [--steps N] [--seed N] [--out path] [--refine=false] [--family NAME] [--transparent]
  local-offload generate-image --batch jobs.jsonl [--family F] [--route auto|local|remote] [--results r.jsonl] [--resume] [--jobs N]
                                         N prompts: ONE warm ComfyUI session when this machine's lane is free, else each job a ticket on the
                                         first matching lane that frees (fleet nodes included); a verdict per job; --resume re-renders only
                                         what is missing or does not prove itself; rows go to r.jsonl.partial and over r.jsonl when the batch
                                         ends. exit 0 every job has its picture (or a busy lane deferred the whole warm batch: rows queued),
                                         4 some job failed (the JSON and the results file name them), 1 it stopped, errored or was interrupted
  local-offload inpaint-image <image> --mask m.png --prompt "..."   re-render ONLY the masked region (white=repaint)
  local-offload upscale-image <image> [--scale F] [--width N --height N] [--method lanczos] [--model name] [--out path]   ESRGAN enlarge (this machine's upscale_model)
  local-offload generate-video <out.mp4> <still.png> "<prompt>" [--model hunyuan|wan] [--frames 49] [--seed N] [--reserve-vram F] [--fast] [--upscale] [--node ID|URL] [--min-vram-gb N]
  local-offload animate-character <out.mp4> <ref.png> <driver.mp4> "<prompt>" [--motion-prompt "..."] [--frames 81] [--seed N] [--node ID|URL] [--min-vram-gb N]
                                         --node names the ONE fleet node that runs a video, animation or audio call (its node_id, or a delegate_remotes URL): no other node
                                         and not this machine is tried, and a node that cannot take the call is refused by a word before anything is sent (--json prints
                                         it as data.refusal; without it the DEFERRED line carries the reason); --min-vram-gb N
                                         asks for a node whose largest card is at least N GB
  local-offload run-graph --graph <g.json> [--manifest <m.json>] [--out-dir <d>] [--reserve-vram F] [--min-vram-gb N] [--model-family wan22] [--route local|auto|remote] [--remote URL]... [--json]
                                         with a fleet, a whole-node graph goes to an idle node with ONE card that holds the files it names; --min-vram-gb N asks for a node whose largest card is at least N GB
                                         (without it, --model-family wan22 and a height of 720 or more get a 16 GB floor chosen from the film owner's stated constraint, not measured; 0 lifts it)
  local-offload compose-video --template <name> [--variables '<json>'] | --html <file> | --project-dir <dir>
                              [--format mp4|webm|mov|png-sequence|gif] [--fps N] [--quality draft|standard|high]
                              [--resolution landscape|...] [--workers N] [--strict=false] [--snapshots 1,2.5] [--out path] [--json]
                                         HTML/CSS motion graphics -> video with HyperFrames (CPU-class, no GPU lock; ADR 0059)
  local-offload nim <file|-|"text"> [--model id] [--base url] [--system "..."] [--max-tokens N] [--temp F] [--json]
  local-offload nim --list-models        list a NIM endpoint's model ids (free hosted catalog or self-hosted)
  local-offload mcp                      run as an MCP server (stdio)
  local-offload delegate --contract file.json [--route auto|spread|local|remote] [--read-root DIR] [--remote http://node:18811]...   place delegation-contract subtasks on this box or tailnet fleet nodes (needs agent_delegation_enabled; remotes default to delegate_remotes)
  local-offload fleet-serve [--listen ADDR] [--listen-trusted-network] [--node-id NAME]   join the fleet-dispatcher fleet (health/dispatch/jobs on :18811; docs/FLEET-NODE.md)
  local-offload forecast-serve --forecast-dir DIR --model DIR --port N [--host 127.0.0.1] [--device cuda] [--max-row-patches N]   run the Timestral forecast server a forecast seat of llama-swap launches (python child bound to this process; loopback only)
  local-offload install forecast [--home DIR] [--models DIR] [--ref SHA] [--torch-backend auto] [--json]   build the forecast lane: venv, torch, the newest timestral package, the newest weights checked against the publisher's digest, and the server file (uv required; never loads the model)
  local-offload fleet-measure            prime the fleet footprint store: one minimal render per configured task, then print the recorded entries
  local-offload fleet-ui [--listen 127.0.0.1:18813] [--listen-trusted-network] [--interval 5s] [--remote URL]...   live overview page: node cards (GPU/VRAM/CPU/RAM graphs, seat, served models), cluster jobs + errors feed (docs/systems/fleet-overview.md)
  local-offload fleet-smoke [--remote URL]... [--timeout 120] [--json]   send one grounded one-step contract to EVERY node and print where each landed (node, seat, placement, wall, verdict); non-zero unless all PASS
  local-offload top [--ui http://127.0.0.1:18813] [--interval 5s]   terminal view of the fleet overview (for headless boxes; reads a running fleet-ui)
  local-offload rig --seat <alias> [--since 7d] [--node ID] [--out report.json] [--json]   the seat rigger (ADR 0036 P3a): classify this box's delegation-log failures for a seat onto ONE axis each (published precedence), weights over eligible rows, evidence job ids, the pre-authored remedy per axis — proposes and applies nothing
  local-offload ledger [--since DAYS] [--failures]    token-savings report; --failures prints the failure-class register instead (docs/systems/failure-register.md)
  local-offload doctor [--config FILE] [--probe-completion]   check endpoint health + config, then the engine answering each lane and seat (--probe-completion sends a 1-token completion and can load one idle seat on an idle box)
  local-offload audit-yaml [--against-render] [--other-box] FILE...
                                         check live llama-swap config(s) against the operator rules (INV-1/INV-2: no -ngl 0, no empty CUDA_VISIBLE_DEVICES, ttl 300 everywhere, no persistent group, no preload).
                                         --against-render also re-derives each file from THIS binary's tier seeds and reports MATCH / STALE(keys) / UNSTAMPED / HAND-EDITED.
                                         The CUDA_VISIBLE_DEVICES pin rules compare a file with THIS box's cards (nvidia-smi); --other-box says the files belong to another machine and skips them and the display-cap-unseen note.
                                         A file with a proxy: seat or a CUDA_VISIBLE_DEVICES pin also gets ONE note (display-cap-unseen) naming the entries that may sit on the display card (a proxy: seat, an unpinned entry, an entry pinned onto it): their use of the display card is not judged against gpu_display_cap_pct (the yaml states no VRAM footprint); gpu status reads the card itself. The card table is read only for such a file, so a file with neither is not read and gets no note, its unpinned entries included. The note is silent on a box with one card or with no flagged display card. A note is never a violation and never changes the exit status.
  local-offload audit-config [--config PATH] [--tier NAME] [--json] [--all]
                                         compare this node's live config.json against the seed its tier would install; lists every seed-owned key that is DIFFERENT, LIVE-ONLY (hand-wired, never written back to profiles.json) or SEED-ONLY. Exit 1 on drift.
                                         --ram-tier defaults to THIS machine's detected RAM tier (the header names it, GB included); for another node's config (--config/--goos/--home) pass --ram-tier min|low|mid|high|none. A RAM probe that reads 0 GB is refused, never read as min.
                                         Exit 1 on a violation, a STALE config or a HAND-EDITED one; UNSTAMPED prints as a finding and does not fail. Flags come BEFORE the files.
  local-offload agent-audit verify [--file PATH] [--json]
                                         verify the hash chain of every agent run on the broker audit trail (audit_chain, register SF-08); exit 1 naming the run and seq of an edited, removed or reordered row. Open runs (no run_end) and unchained rows are reported, not failed.
  local-offload report [--out FILE]      READ-ONLY capability report for this machine (tier, serving, media routes) — Markdown, safe to send
  local-offload acceptance [--json]      the gate: EXERCISE every bound capability as this identity (lease writable, interpreters runnable, aliases live). Non-zero when a node must not be handed work.
  local-offload install detect [--json]  classify this machine into a hardware tier (works on every OS)
  local-offload install plan [--json]    the tier + the media bindings an install would seed here
  local-offload install render [--profile ID] [--llama-bin DIR] [--llama-bin-cpu DIR] [--llama-bin-eg2 DIR] [--models DIR] [--out FILE]   render this tier's llama-swap serving config (templates embedded); --llama-bin-cpu adds the tier's CPU seat family (tier must declare alt_backends), --llama-bin-eg2 runs ONLY the embeddinggemma2 entry from a second llama.cpp build (b11452 or newer) for a node whose main build is older
  local-offload install volumes [--json] [--min-free-gb N] [--allow-os-volume] [--data]   where should this machine install? (most free space, never the OS drive by default; --data picks the volume for harness DATA and also skips cloud-synced virtual drives and FAT volumes)
  local-offload data status [--json]     where this node keeps its data, and whether any of it is on the OS drive (C: holds Windows and program installs, never data). Exit 1 when it is and a data volume qualifies.
  local-offload data migrate [--from DIR] [--to DIR] [--apply] [--stopped] [--json]   COPY the data tree to a data drive (never a move, never a junction, source untouched; dry run unless --apply; bbolt stores only with --stopped), then print the "home" line to set
  local-offload models                   show configured offload model
  local-offload eval [--dir DIR]         code-based quality eval (AURC, deferral-curve AUDC/QNC)
  local-offload compaction-eval <harvest|run|freeze|check|ab|kvbench> --corpus C | contextbench   compaction ladder eval: trace→corpus harvest (redacting), ratio/retention report, tokens ratchet, gated A/B, KV-reuse bench, ContextBench tasks
  local-offload gate <freeze|check|verdict|selftest|schema> [flags]   the one measurement gate: freeze a pre-registration, check it, judge a candidate against the incumbent (ADOPT/BLOCK/INCONCLUSIVE, paired interval over tasks), write a model judge's self-test receipt (selftest); exit 0 a verdict was produced, 1 non-ADOPT under --require-adopt or a scorer that failed its self-test, 2 a refusal (docs/systems/measurement-gate.md)
  local-offload recipe-audit --recipe ID --seat ALIAS --seat-tag TAG --contracts TASKS.json --baseline BASELINE.json --engine-build STR [--store DIR] [--attempts N] [--input-out FILE]
                                         run the contracts with and without a recipe (ADR 0036 amendment) on one seat UNDER a GPU lease, judged by the acceptance DSL; publish the recipe only on a gate ADOPT with steps no worse, else demote it; exit 2 without a lease (run it under gpu reserve), 0 once the store records the result
  local-offload confhead-eval            out-of-fold adoption gate (AURC/AUGRC + paired-bootstrap CI)
  local-offload confhead-calibrate       per-task conformal p(correct) escalation thresholds (ADOPT tasks)
  local-offload stats                    observational per-task ledger telemetry
  local-offload trace-corpus <harvest|readout|freeze|check>   recorded agent runs: trace-bearing count, pager re-fetch rate vs its gate, compaction fire rate, prefill tokens (UNRECORDED reads null, never 0)
  local-offload loupe [--since N] [--json]  call-identity ledger view: duplicate rate,
                                         prefix reuse, exemplar histogram, artifact A/B arms
  local-offload update [--component llama|llama-swap|whisper|sdcpp|hyperframes|all] [--channel nightly|release] [--dry-run] [--json] [--resolve COMPONENT] [--rollback COMPONENT] [--repoint YAML --component llama]
                                         resolve each harness component's newest complete build from its publisher, verify the publisher's digest, stage, promote as a generation behind the live path (junction/symlink), keep the previous one, roll back on failure; nothing new upstream is one conditional request (docs/systems/update.md)
  local-offload node-swap --staged NEW.exe --target LIVE.exe --sha256 HEX [--restart-task NAME | --restart-command "..."] [--health-url URL] [--render-tarball t.tar.gz --render-dir DIR] [--dry-run] [--result out.json] [--log out.log] [--json]
                                         the reusable Windows fleet-node binary swap: verify hash -> wait idle -> backup+rename -> restart -> verify PID+hash+health -> auto-rollback on any failure (docs/systems/node-swap.md; launch detached via setup/windows-node-swap-launch.ps1 so an SSH drop cannot orphan it)
  local-offload version

Global: --config <path> (or $LOCAL_OFFLOAD_CONFIG)

extract --schema expects a JSON Schema object with a "properties" map, e.g.
  {"properties":{"name":{"type":"string"},"amount":{"type":"number"}}}
A bare {"field":"string"} map has no usable properties and is deferred.
`)
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
	route := fs.String("route", "", textRouteHelp)
	// Go's flag pkg stops at the first positional, so split the input arg out
	// first and parse the remaining flags (allows `summarize <file> --json`).
	positional, flagArgs := splitArgs(args, map[string]bool{
		"config": true, "labels": true, "question": true, "schema": true, "max-points": true, "select": true, "route": true,
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

	res, err := runCLITextTask(context.Background(), cfg, p, core.Request{Task: core.TaskType(task), Door: "cli:" + task, Input: input, Params: params}, *route)
	if err != nil {
		return err
	}
	emitResult(res, *asJSON, *selectFlag, *compactFlag)
	return nil
}

func runMCP(args []string) error {
	// D-36: take the identity of the image this process started from BEFORE anything else runs. The capture hashes the
	// executable at the path os.Executable names, and on Windows that path keeps its name after a rename-swap, so a
	// deploy landing during openPipeline would otherwise be hashed as the image this process runs. (A short CLI call
	// never captures and keeps the lazy hash.)
	buildinfo.CaptureStartup()
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	fs.String("config", "", "config file path")
	_ = fs.Parse(args)
	// The MCP server deliberately STARTS on a config that failed validation. A
	// server that exits removes every offload_* tool from every session with no
	// message on any surface the operator reads — this repo has been bitten by
	// that shape before. It starts and says so instead: offload_status carries
	// config_error and every other tool defers by name (mcpserver.WithConfigError).
	cfg, src := loadCfgWithSource(fs)
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

	// The MCP server is the ONLY long-running process that accumulates embed-memo
	// hits, and deferred functions do not run on SIGINT/SIGTERM — so without a
	// signal context its `defer cleanup()` (the sole caller of the memo's Flush)
	// never fired, and weeks of hit counters vanished on every stop. fleet-serve
	// already installs one; this did not.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// A signal handler still loses everything since the last flush on a kill -9,
	// and this process can run for weeks. A periodic flush bounds that loss to one
	// interval instead of one process lifetime. It is cheap: a no-op when no
	// lookups happened, and one small bbolt transaction when they did.
	flushDone := make(chan struct{})
	go func() {
		defer close(flushDone)
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := embedmemo.FlushShared(); err != nil {
					fmt.Fprintln(os.Stderr, "note: periodic embed-memo flush failed:", err)
				}
			}
		}
	}()

	srv := mcpserver.New(p).WithConfigError(src.LoadErr)
	if src.Loaded() {
		// The server never reloads its config, so the ticket queue re-reads delegate_remotes from this file when it changes
		// (ADR 0083): a node added to the file joins the running session.
		srv = srv.WithConfigSource(src.Path)
	}
	err = srv.Run(ctx, version)
	stopSignals()
	<-flushDone
	// A cancelled context IS the clean shutdown here, not a failure. Returning it
	// made main print "error: context canceled" and exit 1 on every SIGTERM, which
	// a supervisor reads as a crash. runFleetServe — the precedent this signal
	// handling was copied from — returns nil for exactly this reason.
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// runDelegate is the CLI surface onto delegate.Run (multi-node delegation,
// Task 6): same intake, engine, and response JSON as the MCP agent_delegate
// tool, for testing and scripts. Exit code contract: 0 even when subtasks
// DEFER (a defer is a success shape — the result JSON says what happened);
// non-zero only for transport/config errors (bad flags, disabled role,
// unreadable contract file, failed subtasks).
func runDelegate(args []string) error {
	fs := flag.NewFlagSet("delegate", flag.ExitOnError)
	fs.String("config", "", "config file path")
	contractPath := fs.String("contract", "", "path to the delegation contract JSON: one subtask object or an array of up to 8 (fields: goal, context, context_paths, output_schema, acceptance, profile, max_steps, timeout_sec)")
	route := fs.String("route", "auto", "placement route: auto (idle-local wins) | spread (across the local seat AND every eligible fleet node, concurrently; subtask 0 stays local unless it names a layer this box does not declare, the remote slots are placed by the goal's shape, no model call: reasoning-shaped goals take the roomiest eligible seat, mechanical ones the seat expected to finish first, and the window decides only a tie or a fleet that publishes no rate) | local | remote (a PIN only with --pin-reason: local runs on this box's seat and nowhere else, remote on a fleet node and nowhere else, and a remote pin's failed-verification retry goes to another node and never to the local seat; without one they are hints that placement may override, local placed as auto places it and remote fleet-first with the local seat as the fallback). A box with no agent seat (a delegation client) is never a placement: auto and spread go to the fleet")
	pinReason := fs.String("pin-reason", "", "why this call is pinned to --route local or remote: "+strings.Join(delegate.PinReasons(), " | ")+" (privacy and locality take --route local only; measurement and operator take either). Without it local and remote are hints. Refused with any other route")
	readRoot := fs.String("read-root", "", "directory context_paths may be read from (default: the current dir)")
	var remotes repeatedFlag
	fs.Var(&remotes, "remote", "remote fleet node base URL, tailnet-only (repeatable)")
	priority := fs.Int("priority", 0, "scheduling band for every subtask: 0 (default, production) | -1 (sheddable: measurement/gate traffic — takes idle fleet capacity only, never queues before or behind production work, shed when no node is idle) | 1 (urgent)")
	tenant := fs.String("tenant", "", "tenant id the fleet round-robins across (default: host-pid-start, or LOCAL_OFFLOAD_TENANT)")
	_ = fs.Parse(args)
	if err := leftoverArgErr(fs, "delegate"); err != nil {
		return err
	}
	// Before the config is loaded or a contract is read (ADR 0078): the engine checks again.
	if err := delegate.CheckPinReason(*route, *pinReason); err != nil {
		return err
	}
	if *tenant == "" {
		*tenant = delegate.DefaultTenant()
	}
	// loadCfgWithSource says when this config's own layers shadow its agent_model
	// (C-95), before any run defers on a seat the operator never wrote.
	cfg, _ := loadCfgWithSource(fs)
	// Same switch that gates the MCP tool's registration (roast delta 13): a
	// box is a DELEGATOR only by explicit opt-in.
	if !cfg.AgentDelegationEnabled {
		return fmt.Errorf("agent delegation is disabled on this box — set \"agent_delegation_enabled\": true in the config")
	}
	if *contractPath == "" {
		return fmt.Errorf("--contract required (a subtask JSON file)")
	}
	specs, err := parseContractFile(*contractPath)
	if err != nil {
		return err
	}
	// The subtask cap is read off what the file holds, before anything is prepared: nine subtasks with one bad one are
	// refused for their number exactly as nine good ones are (register D-111).
	if err := delegate.CheckSubtaskCount(len(specs)); err != nil {
		return err
	}
	// Each subtask is prepared on its own (register D-111): one that fails intake no longer rejects the valid ones.
	// The BOX's cap (ADR 0052): a composite box admits a contract sized for its long seats; a plain box keeps the
	// 256 KiB transport cap.
	batch, lints, err := cliIntake(specs, *readRoot, cfg.AgentContextCapBytes(), "cli:delegate", "subtask", nil)
	if err != nil {
		return err
	}
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	results, sum, err := delegate.RunWith(context.Background(), cfg, p.RunAgentContract, batch.Contracts, *route, remotes,
		cliPinOptions(*priority, *tenant, p.RescueRepack, *pinReason))
	if err != nil {
		return cliEngineErr(err, batch, "subtask", nil)
	}
	// The refused subtasks go back among the results at their own positions: one result per subtask in the file, in
	// order, each a contract-class defer (the file's mistake, not a broken stack), which delegateExitErr leaves at exit 0.
	results, sum = batch.Splice(results, sum, "subtask", nil)
	out, err := json.MarshalIndent(delegate.WireResponse(results, sum, lints), "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return delegateExitErr(sum)
}

// startVRAMSampler starts the ongoing health sampler for the resolved provider: the routing
// decision (chooseSamplerKind) and the sampler it implies, in one place so the tested seam is
// the shipped one. drmRoot is /sys/class/drm in production and a temp tree in tests; uma is the
// memory model the amdgpu probe composes (APU: VRAM + GTT; discrete: VRAM alone).
//
// A linux-amdgpu node whose device probe cannot read at startup, though the gate probe just did,
// keeps the single-value sampler and says so: gpu_devices[] omitted is today's behaviour, and a
// node must never lose its memory reading to the per-device path.
func startVRAMSampler(ctx context.Context, interval time.Duration, prov fleetnode.ResolvedProvider, uma bool, drmRoot, primaryGPUUUID string) *fleetnode.Sampler {
	switch chooseSamplerKind(prov.Source) {
	case samplerKindDevice:
		return fleetnode.StartDeviceProbeSampler(ctx, interval, fleetnode.SmiDeviceProbe(nvidiaSmiMemoryDevices), primaryGPUUUID)
	case samplerKindAmdgpuDevice:
		probe := fleetnode.AmdgpuSysfsDeviceProbe(drmRoot, uma)
		if _, err := probe(); err != nil {
			fmt.Fprintf(os.Stderr, "[fleet-serve] warning: %s per-device read failed (%v); publishing the single memory reading and no gpu_devices[]\n", prov.Source, err)
			return fleetnode.StartProbeSampler(ctx, interval, prov.Probe)
		}
		return fleetnode.StartDeviceProbeSampler(ctx, interval, probe, primaryGPUUUID)
	default:
		return fleetnode.StartProbeSampler(ctx, interval, prov.Probe)
	}
}

// runFleetServe joins this box to the fleet-dispatcher fleet (CONTRACT.md v2:
// /fleet/health, /fleet/dispatch, /fleet/jobs/{id}) on the same pipeline the
// MCP server drives. Refuses to start without a working GPU MEMORY SOURCE —
// nvidia-smi or the windows-generic WDDM provider (J3, ADR 0014); a zero-VRAM
// node is a contract violation the dispatcher treats as broken. SIGINT drains
// in-flight jobs for up to 30s before exiting.
func runFleetServe(args []string) error {
	// D-36, as runMCP: take the identity of the image this process started from BEFORE anything else runs. A node is as
	// long-lived as an MCP server and stamps harness_build_sha256 on its agent answers, so a deploy that lands after it
	// started must not be hashed, at the first answer, as the image it runs.
	buildinfo.CaptureStartup()
	fs := flag.NewFlagSet("fleet-serve", flag.ExitOnError)
	fs.String("config", "", "config file path")
	listenFlag := fs.String("listen", "", "listen address (default: config fleet_listen, 127.0.0.1:18811)")
	trusted := fs.Bool("listen-trusted-network", false, "allow --listen to bind beyond loopback (the Tailscale address). Every lane then requires the fleet bearer, and the node will not start without fleet_auth_token set; use this ONLY on a network you trust — and NEVER bind 0.0.0.0.")
	nodeIDFlag := fs.String("node-id", "", "node id advertised in /fleet/health, stamped on this node's results and used in its queue claims (default: config fleet_node_id, else the hostname)")
	_ = fs.Parse(args)
	cfg, src := loadCfgWithSource(fs)
	// Before ANY state is touched: a node that cannot prove its own config must
	// not join the fleet (see fleetServeConfigGate).
	if err := fleetServeConfigGate(src); err != nil {
		return err
	}

	// Sweep orphaned pipeline-job dirs BEFORE anything else starts: jobs are
	// in-memory, so a pipeline-jobs/<id> dir of fleet-serve's own that is still
	// on disk at this instant — before this process has accepted a single
	// dispatch — belongs to a job an earlier, ungracefully-stopped instance
	// never finished. Left in place it would permanently refuse every future
	// dispatch reusing that job_spec.id (buildPipelineJob's exclusive job-dir
	// Mkdir guard can't tell "orphaned" from "still running"). Not every dir in
	// that root is fleet-serve's: a delegator process (the MCP server, the
	// delegate and research commands) keeps its local runs' dirs there too,
	// marked with its process id, and outlives this restart — the sweep keeps
	// those while their owner is alive (register C-78), says how many, and says
	// why whenever it removes one for its age alone.
	// A sweep failure is a warning, not fatal — one stuck directory blocking
	// one id is far cheaper than refusing to serve.
	swept, kept, serr := fleetnode.SweepOrphanedPipelineJobs(cfg)
	reportPipelineJobsSweep(os.Stderr, swept, kept, serr)
	// Extracted compose-project trees (ADR 0071) a crash left behind; a warning, never fatal.
	if n, perr := fleetnode.SweepOrphanedProjectDirs(cfg, time.Now()); perr != nil {
		fmt.Fprintf(os.Stderr, "[fleet-serve] WARNING: sweeping compose-project dirs: %v\n", perr)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "[fleet-serve] swept %d orphaned compose-project dir(s)\n", n)
	}
	// Extracted media-job inputs (ADR 0077) a crash left behind; a warning, never fatal.
	if n, perr := fleetnode.SweepOrphanedInputDirs(cfg, time.Now()); perr != nil {
		fmt.Fprintf(os.Stderr, "[fleet-serve] WARNING: sweeping media-job input dirs: %v\n", perr)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "[fleet-serve] swept %d orphaned media-job input dir(s)\n", n)
	}
	// Audio files the stt upload door (ADR 0072) wrote and a crash left behind; a warning, never fatal.
	if n, perr := fleetnode.SweepOrphanedSTTUploads(cfg, time.Now()); perr != nil {
		fmt.Fprintf(os.Stderr, "[fleet-serve] WARNING: sweeping stt upload files: %v\n", perr)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "[fleet-serve] swept %d orphaned stt upload file(s)\n", n)
	}
	// Transcripts of stt upload jobs (ADR 0072) older than their retention, which a stopped node did
	// not get to sweep; the running node sweeps them on the job store's janitor tick. A warning, never fatal.
	if n, perr := fleetnode.SweepSTTTranscripts(cfg, time.Now()); perr != nil {
		fmt.Fprintf(os.Stderr, "[fleet-serve] WARNING: sweeping stt transcripts: %v\n", perr)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "[fleet-serve] swept %d expired stt transcript file(s)\n", n)
	}

	listen, nodeID, err := fleetServeParams(*listenFlag, *nodeIDFlag, *trusted, cfg, src, os.Hostname)
	if err != nil {
		return err
	}
	// The pipeline stamps cfg.FleetNodeID on every result envelope, and /fleet/health and the queue claim take nodeID: one
	// name for the node in all three, whichever way the id was given (the installers pass --node-id and write no fleet_node_id).
	cfg = fleetServeConfig(cfg, nodeID)
	// Which lanes are open, said plainly, on every start (not only with --listen-trusted-network).
	fmt.Fprintf(os.Stderr, "[fleet-serve] %s\n", fleetServeAuthNote(listen, cfg))

	// Startup GPU probe (J3: provider resolution, not an nvidia-smi assumption).
	// nvidia-smi is tried first — a working NVIDIA node behaves exactly as
	// before; else the windows-generic WDDM source (registry capacity + PDH
	// adapter usage) serves AMD/Intel boxes, with vendor/arch read from the
	// installer's manifest and the UMA memory model taken from the profile.
	// Only when NO memory source works does the node refuse — that is the fact
	// the contract cares about (vram_total_gb must be > 0), not the GPU brand.
	info, _ := fleetnode.ReadInstalledInfo(fleetnode.InstalledJSONPath()) // absent manifest = zero info, handled below
	uma, umaKnown := fleetnode.UMAFromProfile(info.Profile)
	umaSrc := "profile"
	if !umaKnown {
		umaSrc = "n/a"
		// No profile signal (pre-installer box): a small dedicated pool reads as a
		// UMA carve-out. LAST-RESORT heuristic — it misfires on a manifest-less
		// small DISCRETE card (ADR 0014 documents this); the serve log below marks
		// heuristic-guessed UMA so a wrong guess is visible to the operator.
		if ded, derr := fleetnode.DedicatedVramTotalGiB(); derr == nil && ded < 6 {
			uma = true
			umaSrc = "heuristic"
		}
	}
	// The generic source is per-OS and per-tier (genericMemProvider): the WDDM
	// registry+PDH provider on Windows, the amdgpu sysfs provider on Linux
	// (vram_linux_amdgpu.go — the seam ADR 0014 left open; without it an AMD APU on
	// Linux with a measured tier could not fleet-serve at all, <node-f> 2026-09-20),
	// and /proc/meminfo less the operator's reserve for an SoC with no VRAM at all.
	generic := genericMemProvider(runtime.GOOS, info.Profile, uma, cfg.UMAReserveGiB, "/")
	prov, perr := fleetnode.ResolveProviderNamed(
		fleetnode.SmiProbe(nvidiaSmiMemory),
		generic,
		func() (string, string) { return "nvidia", gpuArchFromName(nvidiaSmiGPUName()) },
		info,
	)
	if perr != nil {
		return fmt.Errorf("fleet-serve: %v — refusing to start: a node without a working GPU memory source cannot honor the fleet contract (vram_total_gb must be > 0)", perr)
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

	// Multi-GPU fix: on a working nvidia-smi node, the ONGOING health sampler
	// runs the per-device query (nvidiaSmiMemoryDevices) so /fleet/health can
	// report every card (gpu_devices[], ALWAYS present for nvidia-smi — including
	// a single-GPU box, with one entry; no single-GPU special case) and headline
	// either the operator-pinned primary_gpu_uuid card (cfg.PrimaryGPUUUID — the
	// CMP tier notes' canonical "pin by UUID, never index" guidance) or, when
	// unset/unmatched, the LARGEST one (fleetnode.HeadlineDevice) — never
	// trusting nvidia-smi's PCI-bus-order "index 0". See vram.go's
	// SelectHeadlineDevice/HeadlineDevice doc comments for why that distinction
	// matters. A linux-amdgpu node publishes gpu_devices[] the same way from the
	// driver's sysfs (no UUID, so the largest card heads). windows-generic has no
	// per-device signal (vram_windows.go) and linux-meminfo is one RAM pool with no
	// cards, so both stay on the single-value sampler (gpu_devices[] omitted), and
	// PrimaryGPUUUID has no effect there. startVRAMSampler is the tested seam
	// for this routing decision (fleet_verbs_test.go) — this is its only
	// caller, so the tested logic is the shipped logic.
	sampler := startVRAMSampler(ctx, 2*time.Second, prov, uma, "/sys/class/drm", cfg.PrimaryGPUUUID)
	// total for the startup banner below: prefer the live sampler's headline
	// reading (multi-device-aware) over prov.TotalGiB (the gate probe's
	// single-value, first-line reading) so the banner never disagrees with
	// what /fleet/health is about to advertise. Falls back to the gate
	// reading only in the implausible case the sampler's own first sample
	// (taken synchronously above) somehow isn't loadable yet.
	total := prov.TotalGiB
	if snap, ok := sampler.Load(); ok {
		total = snap.TotalGiB
	}
	jobs := newFleetJobs(cfg)
	// The lease in force is not always the one written (default, raised to the
	// floor, off): say which, once, so a substituted value is not silent.
	fmt.Fprintf(os.Stderr, "[fleet-serve] %s\n", pollLeaseNote(cfg))
	// The ingress guard defaults to warn, which serves what it would refuse: say which mode runs.
	fmt.Fprintf(os.Stderr, "[fleet-serve] %s\n", ingressGuardNote(cfg))
	// Reclaimable VRAM is sampled in the background (never from the health handler,
	// which must not block on llama-swap) — see fleet_reclaim.go for why the idle
	// baseline, not free or total, is the right denominator for a shared card.
	reclaim := startReclaimTracking(ctx, cfg, sampler.Load, 5*time.Second)
	// Host CPU/RAM, same background-sampler rule: health only ever Loads.
	host := hostsample.Start(ctx, 5*time.Second)
	// The display layer's post-admission guard (ADR 0075): while a twin of the layer is loaded, the
	// layer's presence and display_floor guards are asked again every display_watch_sec (10 s), and the
	// twin is unloaded when either refuses. Inert on a box with no display layer; stopped before
	// fleet-serve returns. See fleet_displaywatch.go.
	stopDisplayWatch := startDisplayWatch(ctx, cfg, sampler)
	defer stopDisplayWatch()
	// PAIR seat activity (0.133.0): direct traffic on this box's vLLM seats
	// becomes PAIR cards. Stopped and flushed before fleet-serve returns, so
	// an open card is closed rather than left running.
	if cfg.PairSeatActivityEnabled {
		watcher := pairworkloads.NewSeatWatcher(pairworkloads.New(pairworkloads.SeatWatchConfig(cfg)), cfg)
		wctx, wcancel := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		go func() { watcher.Run(wctx); close(watchDone) }()
		defer func() { wcancel(); <-watchDone }()
		pairworkloads.LogSeatWatch(cfg)
	}
	// PAIR orphan sweep (0.133.1): a harness process killed between a card's
	// running frame and its terminal frame leaves the card "Running" in PAIR
	// until PAIR restarts. The long-lived node closes such cards every
	// OrphanSweepInterval (every other emitting process sweeps once itself).
	if cfg.PairWorkloadsEnabled || cfg.PairSeatActivityEnabled {
		sctx, scancel := context.WithCancel(ctx)
		sweepDone := make(chan struct{})
		go func() {
			pairworkloads.RunOrphanSweeper(sctx, pairworkloads.New(pairworkloads.OrphanSweepConfig(cfg)), pairworkloads.OrphanSweepInterval)
			close(sweepDone)
		}()
		defer func() { scancel(); <-sweepDone }()
	}
	// One resolved answer, used by BOTH the server and the startup banner: the
	// agent lane's advertisement keys on it (fleetnode.AgentLaneAdmissible), so
	// a banner computing it separately from the config could print a task list
	// health does not serve.
	loopbackListener := netguard.LoopbackAddr(listen)
	// Store steward (0.113.16): when this node owns a KV page store on disk,
	// keep it under budget between turns — every fleet_store_prune_every_jobs
	// completed jobs, on any health poll that finds it above the high mark, and
	// once at start. A missing root is loud here, never a silent no-op per tick.
	var storeStatus func() storesteward.Status
	if root := strings.TrimSpace(cfg.FleetStoreRoot); root != "" {
		if err := storesteward.Validate(root); err != nil {
			return fmt.Errorf("fleet-serve: fleet_store_root %q: %w", root, err)
		}
		steward := storesteward.New(root, cfg.FleetStoreCapGB, cfg.FleetStorePruneEveryJobs, nil)
		steward.SetLogf(log.Printf) // one journal line per removed page: a prune stays forensically recoverable
		jobs.OnFinish(func() { steward.JobDone() })
		storeStatus = steward.Status
		go steward.Tick()
		every := time.Duration(cfg.FleetStorePruneEverySec) * time.Second
		if cfg.FleetStorePruneEverySec == 0 {
			every = 60 * time.Second
		}
		if every > 0 {
			go storesteward.Every(ctx, every, func() { steward.Tick() })
		}
		log.Printf("fleet-serve: store steward on %s (cap %.0f GB, tick every %d jobs and every %s)", root, cfg.FleetStoreCapGB, cfg.FleetStorePruneEveryJobs, every)
	}
	// The node's own GPU lease, advertised in health and enforced at dispatch
	// (0.113.16). Read through THE one resolver (gpulease.LeaseDir) so the
	// advertised lease is the same one every acquirer contends on.
	leaseRead := fleetLeaseReader(cfg)
	nodePair := pairworkloads.New(pairworkloads.FromConfig(cfg))
	// The driver fault summary (gpu_faults on health): read in the background every 10 minutes; this first call starts
	// the first read now rather than at the first health request.
	gpuFaults := gpuFaultsHealthReporter(cfg.StateDir)
	gpuFaults()
	srv := fleetnode.New(p, jobs, fleetnode.Options{
		NodeID:  nodeID,
		Version: version,
		// The card of a job whose asker will not card it itself (D7): this node's own emitter, so
		// an in-flight card of a killed fleet-serve still gets an orphan marker and is closed.
		Pair:     nodePair,
		Reclaim:  reclaim,
		Snapshot: sampler.Load,
		Lease:    leaseRead,
		// What the lease is doing beside the fact that it is held (plan P8): orphaned,
		// overdue and stalled ride /fleet/health so a delegator can tell an abandoned
		// lease from a working one.
		LeaseStanding: fleetLeaseStanding(cfg),
		Store:         storeStatus,
		// The rendered serving config's provenance (K-02): NEW KEYS on the
		// existing /fleet/health payload, never a new listener. nil when
		// serving_config_path is unset, which omits both fields.
		ServingConfig: servingConfigReporter(cfg.ServingConfigPath),
		// Whether the boot-time GPU power profile took (register A-103): the doctor's power row as a health field,
		// read from the state file LOCAL_OFFLOAD_POWER_PROFILE_STATE names. "unknown" with a reason, never an error,
		// on a node that applies no profile.
		PowerProfile: powerProfileHealthReporter(),
		GpuFaults:    gpuFaults,
		Footprints: func() []fleetnode.FootprintEntry {
			if st := p.FootprintStore(); st != nil {
				// Pick up records written by OTHER processes (fleet-measure while
				// serving was the live-found case) — mtime-gated, max-merged.
				st.ReloadIfChanged()
				return st.Entries()
			}
			return nil
		},
		GpuVendor: prov.Vendor,
		GpuArch:   prov.Arch,
		// ADR 0054: the serving backends this install rendered, primary first — a
		// dual-route node (Vulkan iGPU + CPU family) advertises both so a caller can
		// pick the route by seat id. nil (no manifest backend) omits the field.
		Backends: info.Backends(),
		// ADR 0024: the manifest's additive-device list, advertised so
		// a delegator can route NPU-owned work here. A hand-built node has no
		// installed.json (<node-c>, verified) and would never list its device;
		// the config's own list is the fallback then (Coral D6). The manifest
		// wins when it lists anything, so the installer path is unchanged.
		// A local-only device (register E-08) is dropped either way.
		Accelerators: fleetAccelerators(info.Accelerators, cfg.Accelerators),
		// The agent lane's tokenless-listener refusal keys on where the bind
		// actually landed (the resolved listen address), not on the
		// --listen-trusted-network permission flag — a trusted-network flag on
		// a loopback bind is still loopback.
		LoopbackListener: loopbackListener,
		Cfg:              cfg,
		KVSlotDir:        filepath.Join(filepath.Dir(fleetnode.InstalledJSONPath()), "kvslots"),
		KVSlotCapGiB:     cfg.KVSlotCapGiB,
		Host:             host.Load,
	})

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("fleet-serve: listen %s: %w", listen, err)
	}
	umaLabel := ""
	switch prov.Source {
	case "windows-generic":
		umaLabel = fmt.Sprintf(", uma=%v(%s)", uma, umaSrc)
	case fleetnode.MeminfoSource:
		umaLabel = fmt.Sprintf(", unified memory, %.1f GiB held back for the host", cfg.UMAReserveGiB)
	}
	devicesLabel := ""
	if snap, ok := sampler.Load(); ok && len(snap.Devices) > 1 {
		devicesLabel = fmt.Sprintf(", %d GPUs (headlining the largest)", len(snap.Devices))
	}
	fmt.Fprintf(os.Stderr, "[fleet-serve] node %q serving /fleet on %s (%.1f GiB VRAM via %s, vendor=%s arch=%s%s%s; tasks: %s)\n",
		nodeID, listen, total, prov.Source, prov.Vendor, prov.Arch, umaLabel, devicesLabel, strings.Join(fleetnode.SupportedTasksFor(cfg, loopbackListener), ", "))

	errCh := make(chan error, 1)
	// Option B pull queue (ADR 0030) — both halves DARK unless configured:
	// this node HOSTS the durable queue when fleet_queue_host is set, and PULLS
	// work from the configured holder when fleet_queue_holder+fleet_queue_claim
	// are. An unconfigured box runs byte-identically to pre-0.109.0.
	if cfg.FleetQueueHost {
		root, rerr := gpulease.ResolveStateRoot(cfg.StateDir)
		if rerr != nil {
			return fmt.Errorf("fleet_queue_host: no state root for the queue store: %w", rerr)
		}
		q, qerr := srv.EnableQueueHost(filepath.Join(root, "fleet-queue.db"))
		if qerr != nil {
			return fmt.Errorf("fleet_queue_host: %w", qerr)
		}
		defer q.Close()
		log.Printf("fleet-serve: hosting the consolidated queue at %s", filepath.Join(root, "fleet-queue.db"))
	}
	if cfg.FleetQueueHolder != "" && cfg.FleetQueueClaim {
		go srv.StartClaimLoop(ctx, cfg)
	}
	go func() { errCh <- srv.Serve(ln) }()
	return fleetServeAwait(ctx, errCh, ln, jobs, nodePair, 30*time.Second)
}

// doctorRunChecked is doctorRun plus the data-volume audit (register C-92). data is
// nil when the caller did not audit, which is every test that predates the audit and
// every host with no OS drive letter.
func doctorRunChecked(cfg config.Config, routes []mediacap.Route, w io.Writer, data *datahome.Report) error {
	fmt.Fprintf(w, "endpoint:   %s%s\nmodel:      %s\ncache:      %s\nledger:     %s\n",
		cfg.Endpoint, cfg.CompletionPath, cfg.Model, cfg.CachePath, cfg.LedgerPath)
	// Media first, and independent of everything below: these verdicts are pure
	// config + filesystem, so an endpoint that happens to be down must never hide
	// them — a doctor that is red for llama-swap and silent about a broken media
	// binding is the same blind spot in a different disguise.
	mediaMissing := writeMediaSection(w, routes)
	// sdcpp's Vulkan device pin (register: <node-e> remediation 2026-09-23): a box
	// with an enabled integrated GPU enumerates it as Vulkan0, ahead of the
	// discrete card, and the render script used to pin device 0 whenever the
	// environment left it unset — every sdcpp render then ran on the iGPU at
	// 100x the discrete card's speed with no error, nothing red anywhere. The
	// render script now auto-picks the discrete adapter itself; this just tells
	// the operator, without guessing, which device is actually about to be used
	// and flags it loudly when that is still an iGPU (an explicit env override,
	// or a box with no discrete adapter at all). Informational only — it never
	// changes doctor's exit code, because a working-but-slow iGPU render is not
	// the same class of failure as a missing binding.
	writeSdcppDeviceSection(w, cfg)
	// Gap 1 (videogen_text_encoder etc. were not family-scoped): report, by name,
	// what EACH video family actually resolves to on this box — an explicit
	// videogen_families[name] entry when one is bound, else the box's flat
	// videogen_* keys (which may intentionally belong to a DIFFERENT family, the
	// back-compat fallback — see ResolveVideoFamilyBinding). Informational only,
	// like the sdcpp device line above: an operator can now SEE, by name, exactly
	// which files a `model` override would reach for each family on this box,
	// instead of having to read the config and reason through the precedence.
	writeVideoFamilyBindingsSection(w, mediacap.VideoFamilyBindingRows(cfg))
	// The model files behind the ComfyUI routes (register F-31): a name the graph
	// will ask ComfyUI to load must sit in the class directory the loader node
	// opens; a missing or misplaced file used to surface only as a graph
	// rejection at render time.
	bindingBroken := writeModelBindingsSection(w, mediacap.ModelBindings(cfg))
	// Same reasoning, same place in the order: the cache-server verdicts are pure
	// config too, so a serving layer that happens to be down must never hide them.
	unbound := writeCacheServerSection(w, cfg)
	// Third in the same band, for the third time the same reason: a configuration
	// finding is pure config, so an endpoint that happens to be down must not hide
	// the value that EXPLAINS what the operator is looking at.
	findings := writeConfigFindingsSection(w, cfg)
	// The tailnet zones the guard admits dotted hostnames under: pure config as well, and
	// the first thing to read when a roster entry by name is refused.
	writeTailnetZonesSection(w, cfg)
	// The ingress guard mode: pure config, and the default (warn) serves what it would refuse.
	writeIngressGuardSection(w, cfg)
	// The workers this box publishes against the slots its agent seat serves, read from the live serving
	// config: pure config plus one file, informational like the skew and token rows, and silent without an
	// agent seat.
	writeFleetCapacitySection(w, cfg)
	// The host RAM a vLLM agent seat holds, against the figure the node declares for it (agent_seat_host_ram_gib): pure
	// config, informational, and silent on a box with no vLLM seat.
	writeSeatHostRAMSection(w, cfg)
	// The flat comfy_cuda_device and comfy_dynamic_vram a multicard seat does not apply: pure config, informational (a NOTE never changes the exit code), and
	// silent on a config with no multicard seat. Config load used to warn of them on every command.
	writeMulticardNotesSection(w, cfg)
	// Whether the GPU power limit and clock lock a box applies at boot were applied this boot, from the state record the applier
	// writes: one file, informational (a WARN or UNKNOWN never changes the exit code), and one line where nothing is recorded.
	writePowerProfileRow(w)
	// Endpoints that name a box by a private-range IP literal instead of a tailnet name: pure config, a
	// warning only (never an exit code), and silent on a config with none.
	writeLANIPEndpointsSection(w, cfg)
	// Fourth in the band, for the same reason: where the data lives is config plus a
	// volume list, so a serving layer that is down must not hide it (register C-92).
	dataOnOS := writeDataHomeSection(w, data)
	client := llamaclient.New(cfg.Endpoint, cfg.CompletionPath, cfg.Model, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !hasLocalModel(cfg) {
		// A delegation client (install client) has no local endpoint to be healthy.
		fmt.Fprintln(w, "health:     SKIP - no local model is configured (a delegation client); the fleet serves every model")
	} else if err := client.Health(ctx); err != nil {
		fmt.Fprintln(w, "health:     DOWN -", err)
		return fmt.Errorf("endpoint down: %w", err)
	} else {
		fmt.Fprintln(w, "health:     OK")
	}
	// Fleet version skew (security standard L0, register R-06): informational,
	// never an exit-code change — a node on another release is a parity finding
	// for the operator, not a broken local box.
	//
	// The roster is read ONCE, concurrently, through the same admission and cache the single-shot
	// lanes use (internal/rosterprobe): doctor used to read it serially at 5 s a remote, so a
	// roster with dead members made the one verb an operator runs to find out what is wrong slow
	// in proportion to what was wrong. The version rows and the token rows below read that one answer.
	fleetRoster := rosterprobe.Probe(context.Background(), cfg.DelegateRemotes, cfg.FleetAuthToken, doctorFleetProbeTimeout)
	writeFleetSkewSection(w, cfg, buildinfo.Version, rosterVersionReader(fleetRoster))
	// Whether the nodes accept THIS box's fleet token: health ignores the bearer, so a wrong or
	// missing token reads healthy everywhere above and first shows as a 401 on dispatch.
	writeFleetTokenSection(w, cfg, fleetRoster, func(base string) (int, string, error) {
		tctx, tcancel := context.WithTimeout(context.Background(), doctorFleetProbeTimeout)
		defer tcancel()
		return rosterprobe.CheckToken(tctx, base, cfg.FleetAuthToken)
	})
	roster, err := swapclient.FetchRoster(ctx, cfg.Endpoint, 10*time.Second)
	if err != nil {
		fmt.Fprintln(w, "roster:     FAIL - cannot list /v1/models:", err)
		return err
	}
	missing := 0
	for _, a := range modelAliases(cfg) {
		switch {
		case a.Alias == "":
			fmt.Fprintf(w, "%-18s (unset)\n", a.Key+":")
		case roster.Serves(a.Alias):
			fmt.Fprintf(w, "%-18s OK    %s\n", a.Key+":", a.Alias)
		default:
			fmt.Fprintf(w, "%-18s FAIL  %s — not in the live /v1/models roster\n", a.Key+":", a.Alias)
			missing++
		}
	}
	// The escalation head is one semantic verdict, not just an alias row: a cascade whose escalation_model is
	// empty or the workhorse itself has nowhere to escalate to, so the confidence gate is inert. Informational
	// for UNAVAILABLE (a deliberate single-model box is not a broken one); a BROKEN alias is already counted above.
	switch {
	case cfg.EscalationModel == "" || cfg.EscalationModel == cfg.Model:
		fmt.Fprintf(w, "%-18s UNAVAILABLE  single-model cascade - the confidence gate is inert\n", "escalation:")
	case !roster.Serves(cfg.EscalationModel):
		fmt.Fprintf(w, "%-18s BROKEN  %s - not in the live /v1/models roster\n", "escalation:", cfg.EscalationModel)
	default:
		fmt.Fprintf(w, "%-18s OK    %s\n", "escalation:", cfg.EscalationModel)
	}
	if missing > 0 {
		return fmt.Errorf("%d configured model alias(es) missing from %s/v1/models", missing, cfg.Endpoint)
	}
	if mediaMissing > 0 {
		return fmt.Errorf("%d media route(s) bound to something that is not on this machine (a script, model file, custom node class or TTS python/weights)", mediaMissing)
	}
	if bindingBroken > 0 {
		return fmt.Errorf("%d ComfyUI model binding(s) missing or misplaced under %s", bindingBroken, filepath.Join(cfg.ComfyDir, "models"))
	}
	if unbound > 0 {
		return fmt.Errorf("%d vLLM seat(s) with no kv_cache_server binding", unbound)
	}
	if findings > 0 {
		return fmt.Errorf("%d configuration finding(s) — see the config findings section above", findings)
	}
	if dataOnOS > 0 {
		return fmt.Errorf("%d data location(s) on the OS drive %s while %s qualifies to hold them — see the data volume section above", dataOnOS, data.OSDrive, data.Target.Volume.Root)
	}
	return nil
}

// runResearch — `local-offload research`: the CLI face of offload_research.
// URLs in, fetched delegator-side under the public-web guard, one grounded
// digest contract per page, the same delegate.Run path as `delegate`.
func runResearch(args []string) error {
	fs := flag.NewFlagSet("research", flag.ExitOnError)
	fs.String("config", "", "config file path")
	goal := fs.String("goal", "", "what to extract from EVERY page (self-contained; the seat sees only this + the page text)")
	var urls repeatedFlag
	fs.Var(&urls, "url", "public http(s) URL to fetch (repeatable, max 12)")
	var questions repeatedFlag
	fs.Var(&questions, "question", "extra ask appended to the goal for every page (repeatable)")
	var accept repeatedFlag
	fs.Var(&accept, "accept", "extra acceptance check appended to the grounded default (repeatable)")
	schemaPath := fs.String("schema", "", "path to a JSON Schema for the per-page digest (default: key_facts/numbers/quotes/verdict)")
	route := fs.String("route", "spread", "placement: spread (default) | auto | local | remote (local and remote are a PIN only with --pin-reason; without one they are hints that placement may override)")
	pinReason := fs.String("pin-reason", "", "why this call is pinned to --route local or remote: "+strings.Join(delegate.PinReasons(), " | ")+" (research takes measurement or operator only; both take either route). Without it local and remote are hints. Refused with any other route")
	timeout := fs.Int("timeout", 0, "per-page contract budget in seconds (default 300, cap 900)")
	fetchTimeout := fs.Int("fetch-timeout", 0, "per-page fetch timeout in seconds (default 30)")
	_ = fs.Parse(args)
	if err := leftoverArgErr(fs, "research"); err != nil {
		return err
	}
	// Before the config is loaded or a page is fetched (ADR 0078): the engine checks again.
	if err := delegate.CheckPinReason(*route, *pinReason); err != nil {
		return err
	}
	if err := delegate.CheckResearchPinReason(*pinReason); err != nil {
		return err
	}
	cfg := loadCfg(fs)
	if !cfg.AgentDelegationEnabled {
		return fmt.Errorf("agent delegation is disabled on this box — set \"agent_delegation_enabled\": true in the config")
	}
	if strings.TrimSpace(*goal) == "" || len(urls) == 0 {
		return fmt.Errorf("--goal and at least one --url are required")
	}
	if len(urls) > research.MaxURLsPerCall {
		return fmt.Errorf("at most %d --url per call (got %d)", research.MaxURLsPerCall, len(urls))
	}
	var schema json.RawMessage
	if *schemaPath != "" {
		b, err := os.ReadFile(*schemaPath)
		if err != nil {
			return fmt.Errorf("--schema: %w", err)
		}
		schema = json.RawMessage(b)
	}
	opt := research.Options{}
	if *fetchTimeout > 0 {
		opt.Timeout = time.Duration(*fetchTimeout) * time.Second
	}
	fetched := research.FetchAll(context.Background(), urls, opt, 4)
	specs, sources := research.Build(research.Request{
		Goal: *goal, URLs: urls, Questions: questions, OutputSchema: schema, Acceptance: accept, TimeoutSec: *timeout,
	}, fetched)
	if len(specs) == 0 {
		out, _ := json.MarshalIndent(map[string]any{"deferred": true, "reason": "no usable source: every fetch failed or was refused", "sources": sources}, "", "  ")
		fmt.Println(string(out))
		return fmt.Errorf("no usable source")
	}
	// Per page, the rule the delegate verb follows per subtask (register D-111): a page whose contract fails intake is a
	// contract-class defer at its own place in the results and the pages that are fine are digested; only a call with no
	// valid page is refused, naming every page it lost, each by its place in the url list.
	resultSources := research.ResultSources(sources)
	batch, lints, err := cliIntake(specs, "", cfg.AgentContextCapBytes(), "cli:research", "source", resultSources)
	if err != nil {
		return err
	}
	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	results, sum, err := delegate.RunBatched(context.Background(), cfg, p.RunAgentContract, batch.Contracts, *route, nil,
		cliPinOptions(0, "", p.RescueRepack, *pinReason))
	if err != nil && len(results) == 0 {
		return cliEngineErr(err, batch, "source", resultSources)
	}
	results, sum = batch.Splice(results, sum, "source", resultSources)
	// A chunk error after some pages finished: render the finished pages and
	// name the error (partial), never drop the work (same rule as the MCP tool).
	partialErr := ""
	if err != nil {
		partialErr = err.Error()
	}
	lints = lints[:len(results)]
	wire := delegate.WireResponse(results, sum, lints)
	// The same quote check as the MCP door (register F4); the results map to the pages that produced a digest.
	research.AnnotateWire(&wire, resultSources, fetched)
	out, err := json.MarshalIndent(struct {
		Summary any               `json:"summary"`
		Sources []research.Source `json:"sources"`
		Results any               `json:"results"`
		Partial bool              `json:"partial,omitempty"`
		Error   string            `json:"error,omitempty"`
	}{wire.Summary, sources, wire.Results, partialErr != "", partialErr}, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	if partialErr != "" {
		return fmt.Errorf("research: partial — %d of %d pages ran before: %s", len(results), len(specs), partialErr)
	}
	return delegateExitErr(sum)
}
