package confhead

import (
	"encoding/json"
	"math"
	"os"
	"sort"
)

// LoadThresholds reads the per-task conformal p(correct) escalation thresholds
// written by `confhead-calibrate` (Task 3) at path — a flat JSON object like
// {"summarize":0.69,"extract":0}. A missing, empty, or unparseable path yields
// an empty map. The result is always safe to index (a nil map reads as 0), so
// the pipeline never panics when the head/threshold file is absent.
func LoadThresholds(path string) map[string]float64 {
	if path == "" {
		return map[string]float64{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return map[string]float64{}
	}
	var m map[string]float64
	if json.Unmarshal(b, &m) != nil || m == nil {
		return map[string]float64{}
	}
	return m
}

// SelectThreshold computes the per-task escalation threshold tau on p(correct):
// at runtime the pipeline escalates a call to the larger tier when its
// p(correct) < tau, keeping (accepting) only the calls with score >= tau.
//
// It chooses the SMALLEST tau such that, among the ACCEPTED rows (score >= tau),
// the conformal finite-sample error bound is <= targetErr. Smallest qualifying
// tau means maximal acceptance / minimal escalation while still meeting the
// per-task error target.
//
// Conformal safety convention (mirrors internal/calibration exactly): the
// accepted error is bounded by the CRC-adjusted rate
//
//	adjusted = (nAccepted * empiricalErr + 1) / (nAccepted + 1)
//	         = (nErr + 1) / (nAccepted + 1)
//
// i.e. the same +1/(n+1) finite-sample slack used by the margin calibration, so
// the two thresholding paths are consistent. An empty accepted set scores 1.0
// (worst case) by the same convention. A PURE-correct accepted set (nErr == 0)
// is provably below any positive bound, so it scores adjusted error 0 — letting
// it meet a target of exactly 0 (the perfectly-ranked, zero-target case).
//
// Degenerate cases:
//   - If accepting EVERYTHING already meets the target (the RAW overall error
//     <= targetErr), no escalation is needed => tau = 0. The short-circuit uses
//     raw (not conformally-adjusted) overall error so a target >= the base error
//     rate maps to tau = 0 exactly, matching the calibration intent.
//   - If no non-empty accepted set can meet the target — or the inputs are empty
//     or mismatched — tau is set to the maximum observed score, so almost
//     everything escalates. tau is capped at 1.0 so it never exceeds the
//     p(correct) range (a score of exactly 1.0 would then accept nothing,
//     escalating all rows).
//
// scores and correct must be the same length; out-of-fold scores should be used
// by the caller so tau is not optimistic.
func SelectThreshold(scores []float64, correct []bool, targetErr float64) float64 {
	n := len(scores)
	if n == 0 || n != len(correct) {
		return 0
	}

	// Short-circuit: if accepting everything (tau=0) already meets the bound on
	// RAW overall error, no escalation is needed.
	if rawErrAtOrAbove(scores, correct, math.Inf(-1)) <= targetErr {
		return 0
	}

	// Candidate cutoffs are the unique observed scores, ascending. For a cutoff
	// tau we accept rows with score >= tau; sweeping tau upward shrinks the
	// accepted set from the bottom. We want the SMALLEST tau whose conformal
	// accepted-error bound is <= targetErr.
	cands := uniqueScores(scores)
	for _, tau := range cands {
		if adjustedErrAtOrAbove(scores, correct, tau) <= targetErr {
			return tau
		}
	}

	// No cutoff qualifies (e.g. constant or otherwise unseparable scores): fall
	// back to the max score so almost everything escalates. Cap at 1.0 so tau
	// stays within the p(correct) range.
	maxS := cands[len(cands)-1]
	if maxS > 1.0 {
		maxS = 1.0
	}
	return maxS
}

// adjustedErrAtOrAbove returns the CRC-adjusted error-rate bound among rows with
// score >= tau: (nErr + 1) / (nAccepted + 1) — the same +1/(n+1) finite-sample
// slack as internal/calibration. An empty accepted set returns 1.0 (worst case),
// matching that convention. A pure-correct accepted set (nErr == 0) returns 0,
// so it can meet a target of exactly 0.
func adjustedErrAtOrAbove(scores []float64, correct []bool, tau float64) float64 {
	nAccepted, nErr := acceptedCounts(scores, correct, tau)
	if nAccepted == 0 {
		return 1.0
	}
	if nErr == 0 {
		return 0
	}
	return float64(nErr+1) / float64(nAccepted+1)
}

// rawErrAtOrAbove returns the raw empirical error rate among rows with
// score >= tau (no conformal slack). An empty accepted set returns 1.0.
func rawErrAtOrAbove(scores []float64, correct []bool, tau float64) float64 {
	nAccepted, nErr := acceptedCounts(scores, correct, tau)
	if nAccepted == 0 {
		return 1.0
	}
	return float64(nErr) / float64(nAccepted)
}

// acceptedCounts returns (nAccepted, nErr) over rows with score >= tau.
func acceptedCounts(scores []float64, correct []bool, tau float64) (nAccepted, nErr int) {
	for i, s := range scores {
		if s >= tau {
			nAccepted++
			if !correct[i] {
				nErr++
			}
		}
	}
	return nAccepted, nErr
}

// uniqueScores returns the sorted unique score values (ascending).
func uniqueScores(scores []float64) []float64 {
	seen := make(map[float64]bool, len(scores))
	for _, s := range scores {
		seen[s] = true
	}
	out := make([]float64, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Float64s(out)
	return out
}
