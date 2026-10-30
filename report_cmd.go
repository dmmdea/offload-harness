// The read-only reporting verbs: ledger, models, eval and stats.

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

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/eval"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/report"
)

func runLedger(args []string) error {
	fs := flag.NewFlagSet("ledger", flag.ExitOnError)
	fs.String("config", "", "config file path")
	since := fs.Int("since", 0, "only count entries from the last N days")
	includeTraffic := includeTrafficFlag(fs)
	bySession := fs.Bool("by-session", false, "add a sessions array: each session's card tokens split by where they ran (this box or a fleet node)")
	sessionID := fs.String("session", "", "with --by-session, only this session's entry (implies --by-session)")
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	var sinceTS int64
	if *since > 0 {
		sinceTS = time.Now().AddDate(0, 0, -*since).Unix()
	}
	// Lock-free read: works even while the MCP server is appending to the ledger. Test, bake and
	// measurement traffic is left out and counted in excluded_non_production (register F48).
	s, err := ledger.SummarizeFileWith(cfg.LedgerPath, sinceTS, ledger.PricesFrom(cfg.OpusInputPricePerMTok, cfg.OpusOutputPricePerMTok), *includeTraffic)
	if err != nil {
		return err
	}
	// LO-8: surface WHY calls deferred — top defer reasons over the last 7 days
	// (fixed window regardless of --since, so the recent-failure signal is
	// always visible). Best-effort: a read error must not sink the summary.
	reasons, rerr := ledger.TopDeferReasonsWith(cfg.LedgerPath, time.Now().AddDate(0, 0, -7).Unix(), 5, *includeTraffic)
	if rerr != nil {
		fmt.Fprintln(os.Stderr, "note: defer-reason aggregation failed:", rerr)
	}
	out := struct {
		ledger.Summary
		TopDeferReasons7d []ledger.ReasonCount `json:"top_defer_reasons_7d,omitempty"`
		// Sessions is present only with --by-session (or --session): the default output is byte-identical to what
		// it was before the flag existed. A pointer, so a session the ledger does not hold prints [] and not nothing.
		Sessions *[]ledger.SessionShare `json:"sessions,omitempty"`
	}{s, reasons, nil}
	wantSessions := *bySession || *sessionID != ""
	if wantSessions {
		shares, _, serr := ledger.SessionSharesFile(cfg.LedgerPath, sinceTS, *includeTraffic)
		if serr != nil {
			return serr
		}
		picked := []ledger.SessionShare{}
		for _, sh := range shares {
			if *sessionID == "" || sh.Session == *sessionID {
				picked = append(picked, sh)
			}
		}
		out.Sessions = &picked
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	// LO-12: honest claim — this is the est. Opus VALUE of the work kept local,
	// not literal billed savings. Since 0.117.7 it prices BOTH halves (input at
	// opus_input_price_per_mtok, output at opus_output_price_per_mtok), so the
	// line names both token counts rather than only the input one.
	fmt.Printf("tokens kept local (est.): %d in + %d out (~$%.2f Opus value — an estimate, not billed savings)\n",
		s.TokensSaved, s.TokensOut, s.EstValueKeptLocal)
	// D-96: a window that reaches back before 0.117.7 sums rows that priced generated tokens at zero.
	if caveat := ledgerLifetimeCaveat(sinceTS); caveat != "" {
		fmt.Println(caveat)
	}
	if len(s.ExcludedNonProduction) > 0 {
		fmt.Printf("left out: test, bake and measurement traffic (%s job(s)); --include-test-traffic counts it\n", describeExcluded(s.ExcludedNonProduction))
	}
	if wantSessions {
		fmt.Println("sessions: remote_tokens ran on a fleet node and local_tokens on this box; remote_share is that card-side placement split, " +
			"not the share gate's cards/(cards+cloud). A chat-lane call reads as local, and a route \"queue\" delegation writes no row on this box's ledger, so it is in neither number (a floor). " +
			"served_* are jobs this box ran for the session as a fleet node.")
	}
	return nil
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
			// Said, never swallowed: a pipeline that cannot open produced a
			// silent `{}` report on <node-a> (2026-09-18, register A-102 (d)) that
			// read as "zero cases" for an hour.
			fmt.Fprintf(os.Stderr, "eval: open pipeline: %v\n", err)
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
	includeTraffic := includeTrafficFlag(fs)
	_ = fs.Parse(args)
	cfg := loadCfg(fs)
	entries, err := ledger.ReadAll(cfg.LedgerPath)
	if err != nil {
		return err
	}
	entries, excluded := productionEntries(entries, *includeTraffic)
	noteExcluded(excluded)
	m := report.Summarize(entries, cfg.ConfidenceMarginThreshold)
	b, _ := json.MarshalIndent(m, "", "  ")
	fmt.Println(string(b))
	return nil
}
