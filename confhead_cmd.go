// The confidence-head verbs: train, eval and calibrate.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/dmmdea/offload-harness/internal/confhead"
	"github.com/dmmdea/offload-harness/internal/eval"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

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

	// Test, bake and measurement rows carry a fixture's margin, not the box's (register F48): the head is
	// judged and calibrated on production rows only, and the omission is stated.
	rows, excluded, err := ledger.ReadProduction(cfg.LedgerPath)
	if err != nil {
		return fmt.Errorf("confhead-eval: read ledger: %w", err)
	}
	noteExcluded(excluded)
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

	// Test, bake and measurement rows carry a fixture's margin, not the box's (register F48): the head is
	// judged and calibrated on production rows only, and the omission is stated.
	rows, excluded, err := ledger.ReadProduction(cfg.LedgerPath)
	if err != nil {
		return fmt.Errorf("confhead-calibrate: read ledger: %w", err)
	}
	noteExcluded(excluded)
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
