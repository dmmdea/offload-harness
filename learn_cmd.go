// The self-learning offline subcommands (run by offload-dream.ps1 or by hand):
// calibrate, health, train-router, shadow-label, agent-trajectory, optimize, audit-sample.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/calibration"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/embedmemo"
	"github.com/dmmdea/offload-harness/internal/eval"
	"github.com/dmmdea/offload-harness/internal/exemplars"
	"github.com/dmmdea/offload-harness/internal/grounding"
	"github.com/dmmdea/offload-harness/internal/health"
	"github.com/dmmdea/offload-harness/internal/judge"
	"github.com/dmmdea/offload-harness/internal/knn"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/nimclient"
	"github.com/dmmdea/offload-harness/internal/nimoracle"
	"github.com/dmmdea/offload-harness/internal/pipeline"
	"github.com/dmmdea/offload-harness/internal/router"
	"github.com/dmmdea/offload-harness/internal/shadow"
	"github.com/dmmdea/offload-harness/internal/trajectory"
)

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
	// Both label sources (register D-126: the ledger and the confhead labels
	// sidecar, where the classify/triage agreement labels land), calibrated on
	// the scale this box EMITS (register D-130: a cutoff derived from a mix of
	// the two ~10x-apart margin scales belongs to neither).
	_, report, err := calibration.RunSources([]string{cfg.LedgerPath, cfg.ConfHeadLabelsPath}, *alpha, cfg.TargetErrorRate, dst, core.MarginScaleOf(cfg.ConfidenceMarginFullDenominator))
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
	rep, err := health.Run(cfg.LedgerPath, dst, core.MarginScaleOf(cfg.ConfidenceMarginFullDenominator))
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
//
// --oracle nim swaps the counterfactual oracle from the local escalation tier
// to the remote NIM endpoint (cfg.NIMEndpoint / cfg.NIMModel; key from
// $NVIDIA_API_KEY, never config): each item's oracle answer is produced by the
// remote model and adapted to the judge-shaped Data via internal/nimoracle.
// Only the escalation slot goes remote — the B1 E2B router-counterfactual
// stays local — and oracle-judged label rows carry "oracle":"nim" for
// provenance. Deliberate remote experiment: input text leaves the box.
func runShadowLabel(args []string) error {
	fs := flag.NewFlagSet("shadow-label", flag.ExitOnError)
	fs.String("config", "", "config file path")
	n := fs.Int("n", 200, "max items to label this run")
	oracle := fs.String("oracle", "local", "counterfactual oracle: local (escalation tier) or nim (remote NIM endpoint; tasks "+strings.Join(nimoracle.SupportedTasks(), "/")+")")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	if *oracle != "local" && *oracle != "nim" {
		return fmt.Errorf("shadow-label: unknown --oracle %q (want local or nim)", *oracle)
	}

	p, cleanup, err := openPipeline(cfg)
	if err != nil {
		return fmt.Errorf("shadow-label: open pipeline: %w", err)
	}
	defer cleanup()

	// The NIM client is built and PREFLIGHTED before Drain: Drain destructively
	// claims the queue, and per-item chat failures are skipped as un-judgeable —
	// so a dead endpoint, bad key, wrong model id, or a model that truncates at
	// the configured budget would otherwise burn every queued item for zero
	// labels. nimoracle.Preflight runs the SUMMARIZE shape (the structurally
	// largest instructed reply) at its real budget through the same Adapt path
	// items take — a classify-sized ping would prove auth but pass at budgets
	// that then truncate every summarize item. On failure nothing has been
	// drained. The paid preflight is skipped entirely when there is no queued
	// work to protect (neither a queue file nor a crashed drain's claim file).
	runTier := p.RunTier
	oracleTag := ""
	stats := &nimoracle.Stats{}
	if *oracle == "nim" {
		if !fileHasContent(cfg.ShadowQueuePath) && !fileHasContent(cfg.ShadowQueuePath+".draining") {
			fmt.Println("shadow-label: queue empty")
			return nil
		}
		baseURL := cfg.NIMEndpoint
		key := nimclient.KeyForBase(baseURL) // env key only for NVIDIA hosts; never transmitted to a non-NVIDIA base
		if key == "" && nimclient.IsHostedNVIDIA(baseURL) {
			return fmt.Errorf("shadow-label: NVIDIA_API_KEY (or NGC_API_KEY) is not set — required for the hosted endpoint %s.\n"+
				"Get a free key at build.nvidia.com, then: export NVIDIA_API_KEY=nvapi-...\n"+
				"(A self-hosted NIM via nim_endpoint http://host:8000/v1 needs no key.)", baseURL)
		}
		timeout := time.Duration(cfg.NIMTimeoutSec) * time.Second
		if timeout <= 0 {
			timeout = 120 * time.Second
		}
		client := nimclient.New(baseURL, key, timeout)
		if err := nimoracle.Preflight(context.Background(), client.Chat, cfg.NIMModel, cfg.NIMMaxTokens); err != nil {
			return fmt.Errorf("shadow-label: NIM preflight failed (nothing drained): %w", err)
		}
		runTier = nimoracle.WrapRunTier(client.Chat, cfg.NIMModel, cfg.NIMMaxTokens, cfg.EscalationModel, p.RunTier, stats)
		oracleTag = "nim"
		fmt.Printf("shadow-label: oracle=nim model=%s base=%s (escalation slot only; E2B counterfactual stays local)\n", cfg.NIMModel, baseURL)
	}

	items, err := shadow.Drain(cfg.ShadowQueuePath)
	if err != nil {
		return fmt.Errorf("shadow-label: drain queue: %w", err)
	}
	if len(items) == 0 {
		fmt.Println("shadow-label: queue empty")
		return nil
	}

	// T2-C: the drain is THE repetitive embedding workload on this box — it
	// re-embeds the same stored inputs on every run, and re-scores the same
	// reference summaries across every item. Memoizing it is where the memo earns
	// most of its keep, and leaving it on a plain embedder was the gap that made
	// the package's own stated rationale untrue as shipped.
	memoPath, memoEpoch, memoMax := cfg.EmbedMemoSettings()
	emb := judge.NewMemoizedEmbedder(cfg.Endpoint, cfg.EmbedModel(), 30*time.Second,
		judge.MemoOptions{Path: memoPath, Epoch: memoEpoch, MaxEntries: memoMax})
	if emb.Memo == nil {
		fmt.Printf("shadow-label: embed memo not active — %s\n", emb.Reason)
	}
	defer func() {
		if err := embedmemo.CloseShared(); err != nil {
			fmt.Fprintln(os.Stderr, "note: embed memo counters may not have been persisted:", err)
		}
	}()
	deps := shadow.LabelDeps{
		Escalation:            cfg.EscalationModel,
		E2B:                   cfg.TriageModel,
		RunTier:               runTier,
		Oracle:                oracleTag,
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
	if oracleTag != "" {
		fmt.Printf("shadow-label: oracle stats: %s\n", stats)
		if w == 0 {
			// Fail LOUD, and only after every summary line above has printed
			// (the router count is a diagnostic — e.g. a broken confhead sidecar
			// path yields w==0 with router rows already on disk): a zero-label
			// nim run over a non-empty (and destructively drained) queue is a
			// systemic failure until proven otherwise — exit non-zero so
			// schedulers cannot mistake it for a clean run.
			return fmt.Errorf("shadow-label: oracle=nim wrote 0 labels for %d drained items (drain is destructive — those items are gone); %s", len(items), stats)
		}
	}
	return nil
}

// fileHasContent reports whether path exists with non-zero size. Used to skip
// the paid NIM preflight when there is no queued shadow work to protect.
func fileHasContent(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Size() > 0
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
// a side-effect-free replay cannot fairly re-run them. NOTE: the replay loop
// deliberately keeps the ladder rungs OFF and a fixed 8192 window (a frozen
// compaction regime) so candidate-vs-incumbent deltas are never confounded by
// compaction-default changes like ADR 0015's flip.
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
	// The replay agent must score prompts on the model PRODUCTION agents plan
	// with — the agent seat, not the bare workhorse (else prompt A/B verdicts
	// grade the wrong planner).
	plannerModel := cfg.AgentPlannerModel("")
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
		MaxTokens:            cfg.AgentMaxTokens,
		Thinking:             cfg.AgentThinking,
		Sampling:             cfg.AgentSampling,
		SamplingFinal:        cfg.AgentSamplingFinal,
		ReadRoot:             absRoot,
		Offload:              pipeline.NewRecordlessOffload(cfg, plannerModel, timeout),
		NPU:                  pipeline.NewLoopNPU(cfg),
		Accel:                pipeline.NewLoopAccel(cfg),
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
	includeTraffic := includeTrafficFlag(fs)
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	entries, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil {
		return err
	}
	entries, excluded := productionEntries(entries, *includeTraffic)
	noteExcluded(excluded)
	var picked []ledger.Entry
	for _, e := range ledger.JobRows(entries) { // a local job is one job (C-62)
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
