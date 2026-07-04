// Package calibration implements Phase 2 conformal threshold calibration for
// the local-offload harness. It reads the JSONL ledger produced by package
// ledger, derives per-task margin thresholds using Conformal Risk Control, and
// writes the resulting thresholds to a JSON file for the pipeline to consume.
//
// Algorithm (Conformal Risk Control, margin formulation):
//
//	For each task, collect labeled calls: (margin, correct).
//	correct = (Grounded != nil && *Grounded) || (EscalatedAgreed != nil && *EscalatedAgreed).
//	Skip: no usable label, Margin==0, CacheHit.
//	For a candidate threshold t (a margin cutoff), ACCEPT a call when margin >= t.
//	empirical error among accepted = count(accepted && !correct) / count(accepted).
//	Adjusted rate = (n_accepted * err(t) + 1) / (n_accepted + 1).
//	Choose the LARGEST t such that the adjusted rate <= alpha.
//	If no t qualifies, fall back to the most conservative (largest) observed margin.
//	Tasks with < 60 labeled rows are omitted (pipeline uses its constant instead).
//	The floor was lowered from 100 to 60 to match the confhead emission gate so
//	the calibration and training steps share the same data-sufficiency criterion.
package calibration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/dmmdea/local-offload/internal/ledger"
)

// point is one labeled (margin, correct) observation.
type point struct {
	margin  float64
	correct bool
}

// Run reads the ledger at ledgerPath, computes per-task conformal margin
// thresholds, writes a pretty-printed JSON map[task]float64 to outPath, and
// returns the map plus a human-readable summary report.
//
// alphas overrides the per-task target error rate; missing tasks fall back to
// defaultAlpha. Tasks with fewer than 60 usable labeled rows are omitted from
// the returned map (pipeline falls back to its hardcoded constant).
func Run(ledgerPath string, defaultAlpha float64, alphas map[string]float64, outPath string) (thresholds map[string]float64, report string, err error) {
	byTask, err := readLedger(ledgerPath)
	if err != nil {
		return nil, "", fmt.Errorf("calibration: read ledger: %w", err)
	}

	thresholds = make(map[string]float64)
	var sb strings.Builder
	sb.WriteString("Conformal calibration report\n")
	sb.WriteString(strings.Repeat("=", 52) + "\n")

	// Sort tasks for deterministic output.
	tasks := make([]string, 0, len(byTask))
	for t := range byTask {
		tasks = append(tasks, t)
	}
	sort.Strings(tasks)

	for _, task := range tasks {
		pts := byTask[task]
		n := len(pts)
		alpha := defaultAlpha
		if a, ok := alphas[task]; ok {
			alpha = a
		}

		if n < 60 {
			fmt.Fprintf(&sb, "  %-12s  n=%4d  <60 labeled — skipped (pipeline uses constant)\n", task, n)
			continue
		}

		t, achievedErr := conformalThreshold(pts, alpha)
		thresholds[task] = t
		fmt.Fprintf(&sb, "  %-12s  n=%4d  α=%.3f  t̂=%.4f  achieved_err=%.4f\n",
			task, n, alpha, t, achievedErr)
	}

	// Write JSON output.
	raw, err := json.MarshalIndent(thresholds, "", "  ")
	if err != nil {
		return nil, "", fmt.Errorf("calibration: marshal thresholds: %w", err)
	}
	// Atomic write (P4): the long-running MCP server polls this file and must only
	// ever read a COMPLETE file. Write a sibling .tmp then rename (atomic on the
	// same filesystem) so a reader never sees a half-written threshold map.
	tmp := outPath + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return nil, "", fmt.Errorf("calibration: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		return nil, "", fmt.Errorf("calibration: rename %s: %w", outPath, err)
	}

	return thresholds, sb.String(), nil
}

// conformalThreshold computes the largest margin cutoff t̂ such that the
// Conformal Risk Control adjusted error rate among accepted calls is <= alpha.
// It returns t̂ and the achieved empirical error rate at that threshold.
// If no threshold qualifies, it returns the maximum observed margin (most
// conservative: accept only the single most confident call).
func conformalThreshold(pts []point, alpha float64) (threshold float64, achievedErr float64) {
	// Collect unique candidate thresholds from the observed margin values,
	// sorted ascending.
	candidates := uniqueMargins(pts)

	// We want the LARGEST t such that adjustedRate(t) <= alpha.
	// Iterate candidates from largest to smallest; return the first that qualifies.
	bestT := math.NaN()
	bestErr := math.NaN()

	for i := len(candidates) - 1; i >= 0; i-- {
		t := candidates[i]
		adj, err := adjustedRate(pts, t)
		if adj <= alpha {
			bestT = t
			bestErr = err
			break
		}
	}

	if math.IsNaN(bestT) {
		// No threshold satisfies the error constraint; fall back to the most
		// conservative (largest) observed margin — accepts only the very best calls.
		maxM := 0.0
		for _, p := range pts {
			if p.margin > maxM {
				maxM = p.margin
			}
		}
		// Compute the actual achieved error at maxM for the report.
		_, err := adjustedRate(pts, maxM)
		return maxM, err
	}

	return bestT, bestErr
}

// adjustedRate computes the CRC-adjusted error rate for threshold t:
//
//	(n_accepted * empirical_err + 1) / (n_accepted + 1)
//
// It also returns the raw empirical error for reporting.
func adjustedRate(pts []point, t float64) (adjusted, empiricalErr float64) {
	var nAccepted, nErr int
	for _, p := range pts {
		if p.margin >= t {
			nAccepted++
			if !p.correct {
				nErr++
			}
		}
	}
	if nAccepted == 0 {
		// No accepted calls at this threshold — worst-case by convention.
		return 1.0, 1.0
	}
	empErr := float64(nErr) / float64(nAccepted)
	adj := (float64(nAccepted)*empErr + 1.0) / float64(nAccepted+1)
	return adj, empErr
}

// uniqueMargins returns sorted unique margin values from pts.
func uniqueMargins(pts []point) []float64 {
	seen := make(map[float64]bool, len(pts))
	for _, p := range pts {
		seen[p.margin] = true
	}
	out := make([]float64, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Float64s(out)
	return out
}

// readLedger reads the JSONL ledger and returns per-task labeled points.
// Skips: CacheHit, Margin==0, no usable label, malformed lines.
func readLedger(path string) (map[string][]point, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]point{}, nil
		}
		return nil, err
	}
	defer f.Close()

	byTask := make(map[string][]point)
	sc := bufio.NewScanner(f)
	// Increase scanner buffer for potentially long lines.
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e ledger.Entry
		if json.Unmarshal(line, &e) != nil {
			continue // skip malformed / partial trailing lines
		}

		// Filter: cache hits and zero-margin rows carry no useful calibration signal.
		if e.CacheHit || e.Margin == 0 {
			continue
		}

		// Determine label.
		correct, hasLabel := label(e)
		if !hasLabel {
			continue
		}

		byTask[e.Task] = append(byTask[e.Task], point{margin: e.Margin, correct: correct})
	}
	return byTask, sc.Err()
}

// label extracts the correctness signal from a ledger entry.
// Returns (correct, true) when a usable label is present.
func label(e ledger.Entry) (correct bool, ok bool) {
	if e.Grounded != nil {
		return *e.Grounded, true
	}
	if e.EscalatedAgreed != nil {
		return *e.EscalatedAgreed, true
	}
	return false, false
}
