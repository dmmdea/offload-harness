package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/dmmdea/local-offload/internal/confhead"
	"github.com/dmmdea/local-offload/internal/config"
	"github.com/dmmdea/local-offload/internal/eval"
)

// abArmMetrics are the per-task quality/cost summaries for one A/B arm, derived
// from a slice of eval.Outcome for a single task.
type abArmMetrics struct {
	SelectiveAcc float64 // accuracy among accepted (correct / accepted)
	Coverage     float64 // accepted / N
	AvgCost      float64 // TokensOut / N (the existing eval cost unit)
	AUDC         float64 // cascade cost-quality area (entry vs this arm)
	Peak         float64 // max quality across the two operating points
	Escalated    int     // cases where Escalations > 0
}

// abTaskMetrics is the emitted per-task A/B record (brief's JSON contract) plus
// the frontier_win verdict and its component flags.
type abTaskMetrics struct {
	SelectiveAccOff float64 `json:"selective_acc_off"`
	SelectiveAccOn  float64 `json:"selective_acc_on"`
	CoverageOff     float64 `json:"coverage_off"`
	CoverageOn      float64 `json:"coverage_on"`
	AvgCostOff      float64 `json:"avg_cost_off"`
	AvgCostOn       float64 `json:"avg_cost_on"`
	AUDCOff         float64 `json:"audc_off"`
	AUDCOn          float64 `json:"audc_on"`
	PeakOff         float64 `json:"peak_off"`
	PeakOn          float64 `json:"peak_on"`
	// CalibSelectiveAcc is the calibrated-margin baseline's selective accuracy on
	// the same answer-always cases (the "learned head vs calibrated threshold"
	// comparator). NaN-safe: 0 when the baseline cannot be computed.
	CalibSelectiveAcc float64 `json:"calib_selective_acc"`
	// EscalationDelta counts cases where the ON arm escalated but the OFF arm did
	// not — the only behavioral difference the head can introduce. 0 => vacuous.
	EscalationDelta int    `json:"escalation_delta"`
	FrontierWin     bool   `json:"frontier_win"`
	Vacuous         bool   `json:"vacuous"`
	Verdict         string `json:"verdict"`
}

// avgCostOf returns TokensOut/N for a task's outcomes (the eval cost unit). 0 for empty.
func avgCostOf(outs []eval.Outcome) float64 {
	if len(outs) == 0 {
		return 0
	}
	tok := 0
	for _, o := range outs {
		tok += o.TokensOut
	}
	return float64(tok) / float64(len(outs))
}

// armMetricsFor reduces one task's outcomes (this arm) plus the cheap entry-only
// baseline (for the AUDC operating-point pair) into abArmMetrics. entryArm is
// the escalation-off arm used as the cheap operating point in the deferral
// curve; pass the same slice to get a degenerate (single-point) AUDC.
func armMetricsFor(arm, entryArm []eval.Outcome) abArmMetrics {
	n := len(arm)
	accepted, correct, escalated := 0, 0, 0
	for _, o := range arm {
		if o.Accepted {
			accepted++
			if o.Correct {
				correct++
			}
		}
		if o.Escalations > 0 {
			escalated++
		}
	}
	m := abArmMetrics{Escalated: escalated}
	if accepted > 0 {
		m.SelectiveAcc = float64(correct) / float64(accepted)
	}
	if n > 0 {
		m.Coverage = float64(accepted) / float64(n)
	}
	m.AvgCost = avgCostOf(arm)

	// Operating points: cheap entry-only vs this arm; reuse eval.DeferralCurve.
	entryAcc := acceptedAccuracy(entryArm)
	audc, _, peak := eval.DeferralCurve([]eval.OpPoint{
		{Label: "entry", Cost: avgCostOf(entryArm), Quality: entryAcc},
		{Label: "arm", Cost: m.AvgCost, Quality: m.SelectiveAcc},
	})
	m.AUDC, m.Peak = audc, peak
	return m
}

// acceptedAccuracy is correct/accepted over a slice (0 if nothing accepted).
func acceptedAccuracy(outs []eval.Outcome) float64 {
	accepted, correct := 0, 0
	for _, o := range outs {
		if o.Accepted {
			accepted++
			if o.Correct {
				correct++
			}
		}
	}
	if accepted == 0 {
		return 0
	}
	return float64(correct) / float64(accepted)
}

// escalationDelta counts cases where ON escalated but OFF did not. on and off
// MUST be the same task's outcomes in the same case order (eval.Run preserves
// input order, so off[i] and on[i] are the same gold case).
func escalationDelta(off, on []eval.Outcome) int {
	d := 0
	n := len(off)
	if len(on) < n {
		n = len(on)
	}
	for i := 0; i < n; i++ {
		if on[i].Escalations > 0 && off[i].Escalations == 0 {
			d++
		}
	}
	return d
}

// calibratedMarginSelAcc computes the calibrated-margin baseline's selective
// accuracy: treat the raw decision margin as a confidence score, fit a conformal
// escalation threshold tau via confhead.SelectThreshold on the OOF (margin,
// correct) pairs from the answer-always arm, then ACCEPT only rows with
// margin >= tau and report accuracy over that accepted set. This is the
// "calibrated threshold" comparator the head must beat. Rows escalated by the
// baseline (margin < tau) are assumed to be handled by the larger tier (i.e. not
// charged as accepted errors), exactly as a real escalation would be. Returns
// (selAcc, ok); ok=false when there are too few usable pairs.
func calibratedMarginSelAcc(answerAlways []eval.Outcome, targetErr float64) (float64, bool) {
	var margins []float64
	var correct []bool
	for _, o := range answerAlways {
		if !o.Accepted || o.Margin <= 0 {
			continue
		}
		margins = append(margins, o.Margin)
		correct = append(correct, o.Correct)
	}
	if len(margins) < 2 {
		return 0, false
	}
	tau := confhead.SelectThreshold(margins, correct, targetErr)
	accepted, ok := 0, 0
	for i, m := range margins {
		if m >= tau {
			accepted++
			if correct[i] {
				ok++
			}
		}
	}
	if accepted == 0 {
		return 0, true // all escalated: nothing accepted-wrong, but no accepted-right either
	}
	return float64(ok) / float64(accepted), true
}

// computeABTask combines the OFF arm, ON arm, and calibrated-baseline selective
// accuracy into the emitted per-task record and the frontier_win verdict.
//
// frontier_win (gate-2, per task) is ALL of:
//   - selective_acc_on >= selective_acc_off - eps   (no accuracy regression)
//   - avg_cost_on <= avg_cost_off * cost_budget       (no cost blow-up)
//   - audc_on >= audc_off                             (cost-quality frontier not hurt)
//   - selective_acc_on >= calib_selective_acc         (head beats the calibrated baseline)
//
// vacuous = escalation_delta == 0: the ON arm changed no behavior, so any
// "win" is an identity, not evidence. A vacuous task is NOT a frontier win.
func computeABTask(off, on abArmMetrics, calibSelAcc float64, escDelta int, eps, costBudget float64) abTaskMetrics {
	tm := abTaskMetrics{
		SelectiveAccOff: off.SelectiveAcc, SelectiveAccOn: on.SelectiveAcc,
		CoverageOff: off.Coverage, CoverageOn: on.Coverage,
		AvgCostOff: off.AvgCost, AvgCostOn: on.AvgCost,
		AUDCOff: off.AUDC, AUDCOn: on.AUDC,
		PeakOff: off.Peak, PeakOn: on.Peak,
		CalibSelectiveAcc: calibSelAcc,
		EscalationDelta:   escDelta,
	}
	accOK := on.SelectiveAcc >= off.SelectiveAcc-eps
	costOK := on.AvgCost <= off.AvgCost*costBudget
	audcOK := on.AUDC >= off.AUDC
	beatsCalib := on.SelectiveAcc >= calibSelAcc
	tm.Vacuous = escDelta == 0
	tm.FrontierWin = accOK && costOK && audcOK && beatsCalib && !tm.Vacuous

	switch {
	case tm.Vacuous:
		tm.Verdict = "VACUOUS (escalation_delta=0; ON==OFF, no behavior change)"
	case tm.FrontierWin:
		tm.Verdict = "FRONTIER_WIN"
	default:
		tm.Verdict = "NO_WIN"
	}
	return tm
}

// abRecommendation folds the per-task verdicts and the gate-1 outcome into the
// overall ENABLE recommendation. ENABLE requires gate-1 ADOPT AND every task a
// frontier_win with none vacuous/regressing. Anything else => NOT-ENABLE. The
// recommendation is advisory only — the actual flag flip is a separate human step.
func abRecommendation(gate1Adopt bool, tasks map[string]abTaskMetrics) (recommend string, enable bool) {
	allWin := len(tasks) > 0
	for _, t := range tasks {
		if !t.FrontierWin {
			allWin = false
		}
	}
	enable = gate1Adopt && allWin
	if enable {
		return "ENABLE", true
	}
	return "NOT-ENABLE", false
}

// redirectWrites returns a copy of cfg with every writable artifact path pointed
// at scratch (a throwaway temp dir) so an A/B run can NEVER mutate the live
// ~/.local-offload store. Read-only inputs (the staged confhead weights/
// thresholds) are left untouched — they are wired separately by the ON arm.
// This is the hard no-live-mutation guarantee from the brief.
func redirectWrites(cfg config.Config, scratch string) config.Config {
	c := cfg
	c.CachePath = filepath.Join(scratch, "cache.db")
	c.LedgerPath = filepath.Join(scratch, "ledger.jsonl")
	c.ThresholdsPath = filepath.Join(scratch, "thresholds.json")
	c.TierOverridesPath = filepath.Join(scratch, "tier_overrides.json")
	c.RouterWeightsPath = filepath.Join(scratch, "router-weights.json")
	c.RouterLabelsPath = filepath.Join(scratch, "router-labels.jsonl")
	c.ConfHeadLabelsPath = filepath.Join(scratch, "confhead-labels.jsonl")
	c.ShadowQueuePath = filepath.Join(scratch, "shadow-queue.jsonl")
	c.KNNIndexPath = filepath.Join(scratch, "knn-index.jsonl")
	c.ExemplarsDir = filepath.Join(scratch, "exemplars")
	c.MediaDir = filepath.Join(scratch, "media")
	c.SVGDir = filepath.Join(scratch, "svg")
	// Self-learning side files that the live pipeline only READS: leave the head/
	// threshold paths to the arm-specific wiring; redirect the rest defensively.
	return c
}

// abReport is the full emitted A/B record.
type abReport struct {
	Tasks          map[string]abTaskMetrics `json:"tasks"`
	Gate1Adopt     bool                     `json:"gate1_adopt"`
	Recommendation string                   `json:"recommendation"`
	Eps            float64                  `json:"eps"`
	CostBudget     float64                  `json:"cost_budget"`
	CalibTarget    float64                  `json:"calib_target"`
	StagedWeights  string                   `json:"staged_weights"`
	StagedThresh   string                   `json:"staged_thresholds"`
	ForceOffOn     bool                     `json:"force_off_on,omitempty"`
}

// runConfheadAB is the A1 decision-gate (gate-2): it runs the SAME gold cases
// through the live cascade twice, differing ONLY in ConfHeadEnabled (OFF =
// baseline, ON = staged head + thresholds via a TEMP config), plus an
// answer-always arm for the calibrated-margin baseline comparator. It computes
// the per-task frontier_win verdict and the (advisory) ENABLE recommendation,
// prints them as JSON, and NEVER mutates any live file or flips the flag.
func runConfheadAB(cfg config.Config, cases []eval.Case, stagedDir string, eps, costBudget, calibTarget float64, gate1Adopt, forceOffOn bool) error {
	scratch, err := os.MkdirTemp("", "confhead-ab-*")
	if err != nil {
		return fmt.Errorf("confhead-ab: scratch dir: %w", err)
	}
	defer os.RemoveAll(scratch)

	stagedWeights := filepath.Join(stagedDir, "confhead-weights.json")
	stagedThresh := filepath.Join(stagedDir, "confhead-thresholds.json")
	if _, err := os.Stat(stagedWeights); err != nil {
		return fmt.Errorf("confhead-ab: staged weights not found at %s: %w", stagedWeights, err)
	}
	if _, err := os.Stat(stagedThresh); err != nil {
		return fmt.Errorf("confhead-ab: staged thresholds not found at %s: %w", stagedThresh, err)
	}

	run := func(c config.Config) []eval.Outcome {
		p, cleanup, err := openPipeline(c)
		if err != nil {
			fmt.Fprintln(os.Stderr, "confhead-ab: open pipeline:", err)
			return nil
		}
		defer cleanup()
		return eval.Run(context.Background(), p, cases)
	}

	// Each arm gets an ISOLATED cache: a shared cache would let the OFF arm's
	// answers satisfy the ON arm from cache (TokensOut=0, gate never reached),
	// zeroing ON cost and masking the only variable under test. "" disables the
	// cache so every arm runs the full cascade and the cost numbers are real.
	armCfg := func(suffix string) config.Config {
		c := redirectWrites(cfg, scratch)
		c.CachePath = "" // no cross-arm cache contamination
		c.LedgerPath = filepath.Join(scratch, "ledger-"+suffix+".jsonl")
		return c
	}

	// OFF arm: the live baseline (confhead disabled), writes redirected to scratch.
	offCfg := armCfg("off")
	offCfg.ConfHeadEnabled = false
	offOut := run(offCfg)

	// ON arm: confhead enabled, reading the STAGED weights/thresholds (never the
	// live ones), all writes redirected to scratch. The test seam force-OFF makes
	// this arm byte-identical to OFF for the determinism check.
	onCfg := armCfg("on")
	onCfg.ConfHeadEnabled = !forceOffOn
	onCfg.ConfHeadPath = stagedWeights
	onCfg.ConfHeadThresholdsPath = stagedThresh
	onOut := run(onCfg)

	// Answer-always arm (OFF, all confidence gates off) so every triage/classify
	// case yields a (Margin, correct) pair for the calibrated-margin baseline.
	ansCfg := armCfg("ans")
	ansCfg.ConfHeadEnabled = false
	ansCfg.EscalationModel = ""
	ansCfg.ConfidenceMarginThreshold = 0
	ansCfg.ClassifyMinConfidence = 0
	ansOut := run(ansCfg)

	tasks := computeABReport(offOut, onOut, ansOut, eps, costBudget, calibTarget)
	rec, _ := abRecommendation(gate1Adopt, tasks)

	rep := abReport{
		Tasks: tasks, Gate1Adopt: gate1Adopt, Recommendation: rec,
		Eps: eps, CostBudget: costBudget, CalibTarget: calibTarget,
		StagedWeights: stagedWeights, StagedThresh: stagedThresh, ForceOffOn: forceOffOn,
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(b))
	return nil
}

// computeABReport groups the three arms' outcomes by task and produces the
// per-task A/B metrics + verdicts. Tasks present in the OFF arm drive the map.
func computeABReport(offOut, onOut, ansOut []eval.Outcome, eps, costBudget, calibTarget float64) map[string]abTaskMetrics {
	byTask := func(outs []eval.Outcome, task string) []eval.Outcome {
		var sub []eval.Outcome
		for _, o := range outs {
			if o.Case.Task == task {
				sub = append(sub, o)
			}
		}
		return sub
	}
	tasksSet := map[string]bool{}
	for _, o := range offOut {
		tasksSet[o.Case.Task] = true
	}
	tasksList := make([]string, 0, len(tasksSet))
	for t := range tasksSet {
		tasksList = append(tasksList, t)
	}
	sort.Strings(tasksList)

	out := map[string]abTaskMetrics{}
	for _, task := range tasksList {
		offSub := byTask(offOut, task)
		onSub := byTask(onOut, task)
		ansSub := byTask(ansOut, task)

		// Entry-only operating point for each arm's AUDC: the answer-always arm is
		// the common cheap point (escalation off). Both arms use the same cheap
		// baseline so AUDC differences come only from the arm's own quality/cost.
		off := armMetricsFor(offSub, ansSub)
		on := armMetricsFor(onSub, ansSub)

		calibSel, calibOK := calibratedMarginSelAcc(ansSub, calibTarget)
		if !calibOK {
			// No usable margin pairs (e.g. summarize/extract carry no margin): the
			// calibrated comparator is inapplicable, so it cannot block the win.
			// Set it to the OFF selective acc so "beats calib" reduces to "matches OFF".
			calibSel = off.SelectiveAcc
		}
		escDelta := escalationDelta(offSub, onSub)
		out[task] = computeABTask(off, on, calibSel, escDelta, eps, costBudget)
	}
	return out
}
