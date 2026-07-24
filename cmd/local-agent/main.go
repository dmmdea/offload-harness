// Command local-agent is the local Agent-loop CLI: an agent that plans with a
// local model and acts through tools confined to the workspace. P0/P1 are
// read-only (list_dir, read_file, in-process offload_*) with NO network. Write
// (write_file/delete_file, P2), web_fetch (P3), and run_shell in an OS sandbox
// (P4.6) are OPT-IN (--allow-write / --allow-fetch / --allow-shell) and gated by
// one deny→ask→allow policy broker (single chokepoint
// + audit trail). Offload calls go through the harness pipeline's RunTier
// (record=false) so they never touch the savings ledger / cache / shadow queue.
// The planner backend is any OpenAI-compatible endpoint (local llama-swap default).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/sandbox"
)

// splitObjective separates the first bare positional (the objective) from flags,
// so the objective may appear before OR after flags — Go's flag package otherwise
// stops parsing at the first positional, silently dropping trailing flags.
// valueFlags names the flags that consume a following token.
func splitObjective(args []string, valueFlags map[string]bool) (objective string, flags []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(name, "=") && valueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if objective == "" {
			objective = a
		} else {
			flags = append(flags, a)
		}
	}
	return objective, flags
}

// validateFlagCombo rejects mutually-exclusive flag combinations. --profile and
// --two-tier conflict: two-tier builds its own architect/editor loops and sets
// their role-appropriate toolsets itself, so a non-default --profile would be
// silently ignored (redundant/conflicting). A default profile ("" or "general")
// is a no-op and coexists fine. Returns nil when the combination is allowed.
func validateFlagCombo(twoTier bool, profile string) error {
	if twoTier && profile != "" && profile != "general" {
		return fmt.Errorf("--profile and --two-tier are mutually exclusive: two-tier sets the architect/editor toolsets itself")
	}
	return nil
}

// orCfg returns flag if the user set it, else the config fallback — so a model flag
// left at its empty default resolves to this machine's configured model, never a
// hardcoded alias. Keeps the CLI hardware/model-agnostic.
func orCfg(flag, fallback string) string {
	if flag != "" {
		return flag
	}
	return fallback
}

// multiFlag is a repeatable string flag (each --egress-host appends one value).
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func main() {
	// If this process was re-exec'd as the OS-sandbox worker (P4.6 run_shell), apply
	// the cage and exec the command — this NEVER returns. It must be the very first
	// thing main does, before any normal startup. A no-op on a normal invocation.
	sandbox.RunWorkerFromEnv()

	fs := flag.NewFlagSet("local-agent", flag.ExitOnError)
	cfgPath := fs.String("config", "", "harness config file path")
	root := fs.String("root", ".", "workspace root the agent may read (tools are confined here)")
	model := fs.String("model", "", "planner model id (must support tool-calling; default: the harness's configured model)")
	base := fs.String("base", "", "OpenAI-compatible planner endpoint (default: harness endpoint)")
	maxSteps := fs.Int("max-steps", 12, "hard step budget (owned in code, not the prompt)")
	maxTokens := fs.Int("max-tokens", 4096, "planner max tokens per completion — must be large enough for the biggest tool-call argument (e.g. a full file's content) or the model's JSON gets cut off mid-string and the call fails")
	ctxTokens := fs.Int("ctx-tokens", 0, "served model context window (tokens) that transcript compaction budgets against. Default 0 = AUTO: probe the serving endpoint's live n_ctx (/props, llama-swap per-model passthrough) and fall back to 8192 when unanswerable — an assumed window killed real runs with exceed_context_size 400s (measured 2026-07-24). An explicit value overrides the probe (warned when it exceeds the served window). The derived INPUT budget is ctx-tokens - max-tokens - 512.")
	skeletonPrune := fs.Bool("skeleton-prune", true, "compaction: before eliding older tool results to bare size markers, first reduce them to signal-preserving skeletons (head/tail + error/failure lines kept, the rest elided with counted markers). Deterministic and local — no model call. Default ON (measured: flip decision 2026-07-24 — retention never worse, no outcome cost).")
	gcfCompact := fs.Bool("gcf-compact", true, "compaction: LOSSLESS first rung — older tool results that are JSON arrays of flat objects are re-encoded columnar (keys stated once), round-trip proven, before any lossy rung runs. Deterministic and local. Default ON (measured: flip decision 2026-07-24 — lossless, fail-closed, no-op where ineligible).")
	maxSameTool := fs.Int("max-same-tool", 3, "cap on calls to any one tool name per run — the circuit breaker for a model that loops instead of progressing (e.g. repeated/reworded web_search calls). Negative disables the cap; 0 falls back to the built-in default (3).")
	timeoutSec := fs.Int("timeout", 180, "per model call timeout (seconds)")
	asJSON := fs.Bool("json", false, "print the full result JSON (transcript + telemetry)")
	useMem := fs.Bool("memory", false, "enable mem0: recall (agent namespace + optional shared namespace) before planning, persist the run outcome after (evidence-tier, agent namespace only)")
	memBase := fs.String("mem-base", "http://127.0.0.1:18791", "mem0 server base URL")
	memUser := fs.String("mem-user", "local-agent", "the agent's mem0 WRITE namespace (isolated; the server blocks canonical regardless)")
	memShared := fs.String("mem-shared-namespace", os.Getenv("MEM0_SHARED_NAMESPACE"), "optional operator/shared mem0 namespace to ALSO recall from (in addition to --mem-user). Empty = agent namespace only (operator-neutral default). Set this, or MEM0_SHARED_NAMESPACE, to recall your own knowledge namespace.")
	allowWrite := fs.Bool("allow-write", false, "P2: enable write_file/delete_file (worktree-scoped + policy-gated). Default off (read-only).")
	allowOverwrite := fs.Bool("allow-overwrite", false, "open-write: allow overwriting existing files + edit_file in the worktree (requires --allow-write). Default off.")
	allowDelete := fs.Bool("allow-delete", false, "open-write: allow deleting files in the worktree (requires --allow-write). Default off.")
	worktree := fs.String("worktree", "", "writable worktree for write_file/delete_file (default: --root)")
	auditPath := fs.String("audit", "", "policy audit log path (default: ~/.local-offload/agent-audit.jsonl)")
	allowFetch := fs.Bool("allow-fetch", false, "P3: enable web_fetch (egress-allowlist gated). Default off (no network).")
	var egressHosts multiFlag
	fs.Var(&egressHosts, "egress-host", "allowlisted egress host for web_fetch (repeatable); bare host or *.host. Default: none (deny-all).")
	allowShell := fs.Bool("allow-shell", false, "P4.6: enable run_shell inside the OS sandbox (Linux only; no network, FS-confined, syscall-limited). Default off.")
	allowRun := fs.Bool("allow-run", false, "C7b: enable `run` — an allowlisted program run DIRECTLY (no shell) inside the OS sandbox (Linux AND Windows). The executable allowlist is the primary control. Default off.")
	allowSearch := fs.Bool("allow-search", false, "enable web_search (DuckDuckGo, keyless; auto-allowlists the search host). Default off.")
	allowGitHub := fs.Bool("allow-github", false, "enable GitHub tools (github_api/create_repo/upload_file). Token from $GITHUB_TOKEN, default repo from $GITHUB_REPO. Default off.")
	queuePath := fs.String("queue", "", "P5b standalone: drain a JSONL goal queue UNATTENDED (the capability flags become the pre-authorization envelope) instead of a single objective. No resume — a re-run reprocesses the whole queue.")
	askQueuePath := fs.String("ask-queue", "", "standalone: file where asks deferred on the unattended run are parked for review (default: ~/.local-offload/agent-asks.jsonl)")
	tracesDir := fs.String("traces", "", "standalone: directory for per-goal trace JSON (default: ~/.local-offload/agent-traces)")
	goalTimeoutSec := fs.Int("goal-timeout", 300, "standalone: per-goal wall-clock budget in seconds")
	totalTimeoutSec := fs.Int("total-timeout", 0, "standalone: optional cumulative wall-clock budget for the WHOLE drain in seconds (0 = unbounded; --goal-timeout still bounds each goal)")
	resume := fs.Bool("resume", false, "standalone: skip goals already completed in the checkpoint (and record completions) so a re-run RESUMES instead of reprocessing. Give each goal an explicit id (bare goals get positional ids that shift if the queue is reordered). Resume is goal-granular, not transactional: an interrupted goal re-runs in full over any partial side effects.")
	checkpointPath := fs.String("checkpoint", "", "standalone: --resume checkpoint file (default: <traces>/_checkpoint.jsonl)")
	profile := fs.String("profile", "general", "per-task tool profile: general (all tools, default) | edit | build | research | github. A profile NARROWS the advertised tools to a curated subset (better small-model tool selection) and adds a tuned prompt + few-shot exemplars; it can only narrow, never grant a tool the --allow-* flags didn't enable.")
	twoTier := fs.Bool("two-tier", false, "architect/editor two-tier mode (aider one-shot handoff): a planning model drafts a complete plan, then a separate edit model executes it as its sole instruction. One cold model swap. Uses --architect-model / --editor-model. --allow-* flags gate the EDITOR's write tools.")
	architectModel := fs.String("architect-model", "", "two-tier: planning model id (read/search tools, no write; default: the configured escalation model)")
	editorModel := fs.String("editor-model", "", "two-tier: edit model id (executes the plan with the write tools your --allow-* flags granted; default: the configured model)")
	serve := fs.Bool("serve", false, "run as an OpenAI-compatible HTTP server (for OpenWebUI etc.) instead of a single objective; each chat request runs the full agent loop")
	listen := fs.String("listen", "127.0.0.1:18800", "address for --serve")
	listenTrusted := fs.Bool("listen-trusted-network", false, "allow --listen to bind beyond loopback. The --serve endpoint is UNAUTHENTICATED and drives the agent's write/GitHub tools, so it is loopback-only by default; set this ONLY on a network you fully trust.")
	objective, flags := splitObjective(os.Args[1:], map[string]bool{
		"config": true, "root": true, "model": true, "base": true, "max-steps": true, "max-tokens": true, "ctx-tokens": true, "max-same-tool": true, "timeout": true,
		"mem-base": true, "mem-user": true, "worktree": true, "audit": true, "egress-host": true,
		"queue": true, "ask-queue": true, "traces": true, "goal-timeout": true, "total-timeout": true, "checkpoint": true,
		"listen": true, "profile": true,
		"architect-model": true, "editor-model": true,
	})
	_ = fs.Parse(flags)

	if objective == "" && *queuePath == "" && !*serve {
		fmt.Fprintln(os.Stderr, `usage: local-agent "<objective>" [flags]   |   local-agent --queue goals.jsonl [flags]   |   local-agent --serve [flags]`)
		os.Exit(2)
	}

	// Reject mutually-exclusive flags before doing any work (I-3).
	if err := validateFlagCombo(*twoTier, *profile); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	// Same discovery + loud-defaults contract as the main CLI (a review found this
	// binary silently ran on built-in defaults AND ignored the conventional config).
	cfg, cfgSrc := config.LoadWithSource(*cfgPath)
	config.WarnOnDefaults(cfgSrc, os.Stderr)
	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: bad --root:", err)
		os.Exit(1)
	}
	plannerBase := *base
	if plannerBase == "" {
		plannerBase = cfg.Endpoint
	}
	// Resolve model flags against this machine's config (never a hardcoded alias) —
	// an empty flag falls back to the configured model, so the CLI is model-agnostic.
	plannerModel := orCfg(*model, cfg.Model)
	archModel := orCfg(*architectModel, cfg.EscalationModel)
	edModel := orCfg(*editorModel, cfg.Model)
	timeout := time.Duration(*timeoutSec) * time.Second

	// In-process offload (record=false, nil cache+ledger) — the SINGLE shared
	// constructor, so every drive mode's ledger-pristine guarantee is identical.
	offload := pipeline.NewRecordlessOffload(cfg, plannerModel, timeout)

	// The broker audit trail must live OUTSIDE any worktree; resolve a default
	// only when a mutating capability is enabled.
	auditP := *auditPath
	if auditP == "" && (*allowWrite || *allowFetch || *allowShell || *allowRun) {
		if home, e := os.UserHomeDir(); e == nil {
			auditP = filepath.Join(home, ".local-offload", "agent-audit.jsonl")
		}
	}

	// Standalone (--queue): default the ask-queue + traces dir alongside the audit.
	askQ, tracesD := *askQueuePath, *tracesDir
	if *queuePath != "" {
		if home, e := os.UserHomeDir(); e == nil {
			if askQ == "" {
				askQ = filepath.Join(home, ".local-offload", "agent-asks.jsonl")
			}
			if tracesD == "" {
				tracesD = filepath.Join(home, ".local-offload", "agent-traces")
			}
		}
	}

	// Optional mem0 memory (opt-in via --memory). Keep it a nil INTERFACE when off
	// (a typed-nil *MemoryClient would make the loop call persist() on nil). Reads
	// the agent's own namespace plus an optional operator/shared namespace (empty by
	// default — operator-neutral); writes only evidence-tier records under the agent
	// namespace (the server blocks canonical regardless).
	var mem agent.Memory
	if *useMem {
		if key := agent.Mem0KeyFromEnvOrFile(); key != "" {
			mem = agent.NewMemoryClient(*memBase, key, agent.ReadUsers(*memUser, *memShared), *memUser, "local-agent", timeout)
		} else {
			// The on-disk key fallback (~/.mem0/api-key) only resolves inside WSL/Linux
			// (the agent's target home); on Windows export MEM0_API_KEY instead.
			fmt.Fprintln(os.Stderr, "note: --memory set but no mem0 key found — set MEM0_API_KEY "+
				"(the ~/.mem0/api-key fallback only resolves inside WSL/Linux). Running WITHOUT memory.")
		}
	}

	// Build the loop + tools + broker via the SHARED builder — identical across the
	// CLI, the MCP front door, and standalone (parity by construction).
	built, err := agent.Build(agent.BuildConfig{
		PlannerBase:  plannerBase,
		Model:        plannerModel,
		Timeout:      timeout,
		MaxSteps:     *maxSteps,
		MaxTokens:    *maxTokens,
		MaxSameTool:  *maxSameTool,
		ReadRoot:     absRoot,
		Offload:      offload,
		Unattended:   true, // non-interactive CLI: ask → deny-and-queue
		AuditPath:    auditP,
		AskQueuePath: askQ,
		AllowWrite:     *allowWrite,
		AllowOverwrite: *allowOverwrite,
		AllowDelete:    *allowDelete,
		AllowFetch:     *allowFetch,
		AllowShell:     *allowShell,
		AllowRun:       *allowRun,
		AllowSearch:    *allowSearch,
		AllowGitHub:    *allowGitHub,
		GitHubToken:    os.Getenv("GITHUB_TOKEN"),
		GitHubRepo:     os.Getenv("GITHUB_REPO"),
		Worktree:     *worktree,
		EgressHosts:  egressHosts,
		Memory:       mem,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	for _, n := range built.Notes {
		fmt.Fprintln(os.Stderr, "[local-agent] "+n)
	}
	if auditP != "" {
		fmt.Fprintln(os.Stderr, "[local-agent] audit="+auditP)
	}
	loop := built.Loop
	// Resolve the profile BEFORE any network work: a typo'd --profile must be
	// a fast usage error, not one paid for after a possible cold model start.
	prof, err := agent.LookupProfile(*profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	// Ctrl-C cancels cleanly — created HERE (before the served-window probe,
	// which may cold-start a model) so every network step below is cancellable.
	// The loop checks ctx.Err() each iteration.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	// Durable working memory (Task C5): AGENT.md workspace facts + .agent/plan.md
	// scratchpad, rooted at the resolved worktree. built.Worktree is "" when no
	// write/shell/github capability is enabled, in which case WithWorktree is a
	// no-op (nothing loaded, update_plan not registered).
	loop.WithWorktree(built.Worktree)
	// Budget transcript compaction against the ACTUAL served window: probe the
	// endpoint for the planner model's live n_ctx (--ctx-tokens 0 = auto; an
	// explicit flag overrides, warned when it exceeds the probe). Applied to the
	// shared loop, so it takes effect identically across the one-shot, --serve,
	// and --queue drive modes.
	probed, probeOK := agent.ProbeServedWindow(ctx, plannerBase, plannerModel)
	effCtx, ctxNote := agent.ResolveContextTokens(*ctxTokens, probed, probeOK)
	if ctxNote != "" {
		fmt.Fprintln(os.Stderr, "[local-agent] "+ctxNote)
	}
	loop.WithContextTokens(effCtx)
	loop.WithSkeletonPrune(*skeletonPrune)
	loop.WithGCFCompact(*gcfCompact)

	// Per-task tool profile (Task C6): applied AFTER WithWorktree so a
	// worktree-registered tool (update_plan) is present for the edit/build/github
	// subsets to keep. A profile can only NARROW the enabled tool set (safety
	// invariant in WithProfile) + add a tuned prompt and few-shot exemplars.
	loop.WithProfile(prof)
	if prof.Name != "general" {
		fmt.Fprintf(os.Stderr, "[local-agent] profile=%s (tools narrowed to %d; %d exemplars)\n", prof.Name, len(prof.Tools), len(prof.Exemplars))
	}

	// Server drive mode: expose the loop as an OpenAI-compatible HTTP endpoint so a
	// chat GUI (OpenWebUI, etc.) can drive it. Blocks until killed.
	if *serve {
		// Loopback guard: the endpoint is unauthenticated, so refuse a non-loopback
		// bind unless the operator explicitly opts in — and when they do, warn loudly.
		if err := validateListenAddr(*listen, *listenTrusted); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if *listenTrusted {
			fmt.Fprintf(os.Stderr, "[local-agent] WARNING: --listen-trusted-network set — the UNAUTHENTICATED agent "+
				"endpoint is exposed beyond loopback on %q. Anyone who can reach it can drive the agent's write/GitHub tools.\n", *listen)
		}
		if err := serveOpenAI(*listen, loop, plannerModel); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// Standalone drive mode (c): drain the goal queue unattended, then exit. An
	// optional --total-timeout caps the WHOLE drain (defense-in-depth on top of the
	// per-goal budget); Ctrl-C also stops it via the same context.
	if *queuePath != "" {
		dctx := ctx
		if *totalTimeoutSec > 0 {
			var cancel context.CancelFunc
			dctx, cancel = context.WithTimeout(ctx, time.Duration(*totalTimeoutSec)*time.Second)
			defer cancel()
		}
		checkpointP := *checkpointPath
		if checkpointP == "" {
			checkpointP = filepath.Join(tracesD, "_checkpoint.jsonl")
		}
		envelope := []string{"read"}
		if *allowWrite {
			envelope = append(envelope, "write")
		}
		if *allowFetch {
			envelope = append(envelope, "fetch")
		}
		if built.ShellGranted {
			envelope = append(envelope, "shell")
		}
		if built.RunGranted {
			envelope = append(envelope, "run")
		}
		if err := runStandalone(dctx, loop, standaloneOpts{
			queuePath:        *queuePath,
			tracesDir:        tracesD,
			askQueuePath:     askQ,
			worktree:         built.Worktree,
			checkpointPath:   checkpointP,
			goalTimeout:      time.Duration(*goalTimeoutSec) * time.Second,
			resume:           *resume,
			captureEnabled:   cfg.AgentTrajectoryCaptureEnabled,
			captureRate:      cfg.AgentTrajectoryRate,
			captureQueuePath: cfg.AgentTrajectoryQueuePath,
			envelope:         envelope,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// Two-tier drive mode (Task C8): aider's architect/editor one-shot handoff.
	// The planning model (architect) drafts a complete plan with read/search tools
	// only; a separate edit model (editor) executes it with the write tools the
	// --allow-* flags granted, seeing ONLY the plan (no history, not the original
	// request). One cold model swap. Falls back to a single-model editor run if the
	// architect produces no usable plan. This replaces the single-loop run above; it
	// builds its OWN two loops rather than using `built`/`loop`.
	if *twoTier {
		// Architect: read/search only (NO write), the planning model, architect prompt.
		archBuilt, aerr := agent.Build(agent.BuildConfig{
			PlannerBase:          plannerBase,
			Model:                archModel,
			Timeout:              timeout,
			MaxSteps:             *maxSteps,
			MaxTokens:            *maxTokens,
			MaxSameTool:          *maxSameTool,
			ReadRoot:             absRoot,
			Offload:              offload,
			Unattended:           true,
			AuditPath:            auditP,
			AskQueuePath:         askQ,
			AllowSearch:          *allowSearch, // read/search only; NO write for the architect
			GitHubToken:          os.Getenv("GITHUB_TOKEN"),
			GitHubRepo:           os.Getenv("GITHUB_REPO"),
			EgressHosts:          egressHosts,
			Memory:               mem,
			SystemPromptOverride: agent.ArchitectPrompt,
		})
		if aerr != nil {
			fmt.Fprintln(os.Stderr, "error: building architect:", aerr)
			os.Exit(1)
		}
		// Editor: the edit model with the write tools the --allow-* flags granted,
		// editor prompt. Same worktree/audit/queue as the normal single-loop build.
		editBuilt, eerr := agent.Build(agent.BuildConfig{
			PlannerBase:          plannerBase,
			Model:                edModel,
			Timeout:              timeout,
			MaxSteps:             *maxSteps,
			MaxTokens:            *maxTokens,
			MaxSameTool:          *maxSameTool,
			ReadRoot:             absRoot,
			Offload:              offload,
			Unattended:           true,
			AuditPath:            auditP,
			AskQueuePath:         askQ,
			AllowWrite:           *allowWrite,
			AllowOverwrite:       *allowOverwrite,
			AllowDelete:          *allowDelete,
			AllowFetch:           *allowFetch,
			AllowShell:           *allowShell,
			AllowRun:             *allowRun,
			AllowSearch:          *allowSearch,
			AllowGitHub:          *allowGitHub,
			GitHubToken:          os.Getenv("GITHUB_TOKEN"),
			GitHubRepo:           os.Getenv("GITHUB_REPO"),
			Worktree:             *worktree,
			EgressHosts:          egressHosts,
			Memory:               mem,
			SystemPromptOverride: agent.EditorPrompt,
		})
		if eerr != nil {
			fmt.Fprintln(os.Stderr, "error: building editor:", eerr)
			os.Exit(1)
		}
		for _, n := range editBuilt.Notes {
			fmt.Fprintln(os.Stderr, "[local-agent] "+n)
		}
		// Both tiers budget against the SAME resolved window (probed for the
		// default/editor-class model above — on this fleet the tiers share one
		// serving config; an explicit --ctx-tokens still overrides for both).
		architect := archBuilt.Loop.WithWorktree(archBuilt.Worktree).WithContextTokens(effCtx).WithSkeletonPrune(*skeletonPrune).WithGCFCompact(*gcfCompact)
		editor := editBuilt.Loop.WithWorktree(editBuilt.Worktree).WithContextTokens(effCtx).WithSkeletonPrune(*skeletonPrune).WithGCFCompact(*gcfCompact)
		fmt.Fprintf(os.Stderr, "[local-agent] two-tier: architect=%s editor=%s (one swap)\n", archModel, edModel)

		res, err := agent.RunTwoTier(ctx, objective, architect, editor)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if res.Fallback != agent.FallbackNone {
			fmt.Fprintf(os.Stderr, "[local-agent] two-tier FELL BACK to a single editor run (%s)\n", res.Fallback)
		}
		if *asJSON {
			b, _ := json.MarshalIndent(res, "", "  ")
			fmt.Println(string(b))
		} else {
			fmt.Println(res.Output)
			fmt.Fprintf(os.Stderr, "[local-agent] steps=%d stop=%s fallback=%q\n", res.Steps, res.StopReason, res.Fallback)
		}
		return
	}

	res, err := loop.Run(ctx, objective)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Println(res.Output)
		fmt.Fprintf(os.Stderr, "[local-agent] steps=%d stop=%s tools=%d\n", res.Steps, res.StopReason, len(built.Tools))
	}
}
