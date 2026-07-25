package agent

import (
	"math"
	"sort"
)

// tokencal.go — online calibration of the compaction budget against the
// server's REAL token counts.
//
// THE DEFECT THIS FIXES (measured, Phase D / ADR 0017). The ladder decides
// whether to compact by comparing estimateTokens() — a flat chars/4 heuristic —
// against inputBudget(). That estimate undercounts reality badly: on the
// shipped bench (which sends the real tool specs, as production does) the
// measured real/estimated ratio was p50 2.15 / p95 2.81, while the shipped
// safety margin covers only 1.077. Net of the fixed tool-spec payload the
// content-density component alone is 1.3-1.8 on the transcripts that failed.
// The consequence was not theoretical. Three real transcripts were rejected by
// the server with exceed_context_size while the ladder DECLINED TO COMPACT,
// because by its own estimate they fit:
//
//	entry              estimate   budget   compacted?   server counted
//	hv-json-ledger        6,224    6,656          no          11,369
//	hv-docs-readme        6,219    6,656          no           8,749
//	hv-deep-compeval      5,693    6,656          no           9,190
//
// Compaction was gated on the very number that was wrong.
//
// WHY ONLINE CALIBRATION rather than a better heuristic or a tokenizer call.
// A per-kind chars/token table is a guess that rots as content changes, and
// this exact defect is what a guess produced. A /tokenize round-trip on the
// loop's critical path adds a network dependency and a new failure mode to
// every step. But the loop already RECEIVES ground truth: since Phase D each
// response carries usage.prompt_tokens for the payload we just sent. Two
// observations are enough to fit the error and correct the budget, at zero
// network cost, self-correcting per run and per content mix.
//
// THE MODEL is deliberately two-term: real ≈ intercept + slope·estimate.
//   - intercept absorbs the FIXED per-request payload estimateTokens cannot
//     see — chiefly the tool specs, measured at ~528 tokens on this box, which
//     is why small requests show ratios as high as 2.81 while large ones sit
//     near 1.3–1.8. A single multiplicative factor fitted on small requests
//     would over-compact everything.
//   - slope absorbs content density (code tokenizes ~1.9× denser than chars/4
//     assumes).
// A one-term fit conflates them and mis-corrects at both ends.
//
// Everything here is pure and deterministic: no clock, no network, no I/O.

// Calibration bounds. The fit is corrective, not authoritative — a pathological
// or adversarial sequence of observations must never be able to drive the
// budget to a degenerate value. Every bound here exists because a review
// demonstrated the failure it prevents.
const (
	calMinSlope     = 1.0  // never assume the server counts FEWER tokens than we estimate
	calMaxSlope     = 3.0  // an implausibly dense fit is distrusted, and calMaxShrink bounds its effect
	calMaxIntercept = 4096 // a fixed payload larger than this is not credible
	calMinObs       = 3    // two points let a single bad reading define the line
	calMinBudget    = 256  // the floor inputBudget() already guarantees
	// calMaxShrink bounds the TOTAL correction: calibration may never cut the
	// budget by more than half. Without it, a clamped-at-3.0 slope divides the
	// budget by three, which is not "distrusting the fit" — it is
	// near-eliminating the context on the strength of data we already decided
	// was implausible.
	calMaxShrink = 0.5
	// calKeep is the sliding window of observations. Bounded memory also means
	// bounded MEMORY OF MISTAKES: a transient bad reading ages out instead of
	// biasing the fit for the rest of the run.
	calKeep = 32
	// calMinSpreadAbs / calMinSpreadRel: a pair of observations only informs
	// the slope if their estimates differ meaningfully. A one-token spread is a
	// degenerate design matrix whose slope is pure noise — and it is reachable
	// in normal operation (two consecutive circuit-breaker refusals move the
	// estimate by tens of tokens while real tokens move independently).
	calMinSpreadAbs = 512
	calMinSpreadRel = 0.25
)

// TokenCalibrator learns real≈intercept+slope·estimate from observed responses
// and corrects a token budget expressed in ESTIMATE space.
//
// Keeping the correction on the BUDGET (rather than scaling estimateTokens)
// matters: every compaction rung compares estimateTokens against the budget as
// it works, so both sides must stay in the same space or the ladder's internal
// stopping conditions become inconsistent.
type calObs struct{ est, real float64 }

type TokenCalibrator struct {
	obs  []calObs // sliding window, oldest first
	seen int
}

// Observe records one (estimate, real) pair for a request the server accepted.
// Non-positive values are ignored — an unmeasured response must never move the
// calibration.
func (c *TokenCalibrator) Observe(est, real int) {
	if est <= 0 || real <= 0 {
		return
	}
	c.seen++
	c.obs = append(c.obs, calObs{float64(est), float64(real)})
	if len(c.obs) > calKeep {
		c.obs = c.obs[len(c.obs)-calKeep:]
	}
}

// Observations reports how many usable samples the calibrator currently holds.
func (c *TokenCalibrator) Observations() int { return len(c.obs) }

// Fit returns the current (slope, intercept, ok) using a ROBUST median fit
// (Theil–Sen): the slope is the median of pairwise slopes, the intercept the
// median residual. Least squares was the obvious choice and the wrong one —
// it has an unbounded influence function, so one anomalous usage.prompt_tokens
// (a proxy reporting cumulative or prompt+completion tokens, say) dragged the
// slope to its clamp and cut the budget for the remainder of the run. A median
// tolerates a minority of bad readings by construction.
//
// Pairs are only counted when their estimates differ MEANINGFULLY; a near-equal
// pair yields a noise slope, and requiring a real spread is what stops that
// noise from reaching the budget.
//
// ok is false until the data can actually support a two-term fit; callers then
// leave the budget untouched rather than acting on a guess.
func (c *TokenCalibrator) Fit() (slope, intercept float64, ok bool) {
	if len(c.obs) < calMinObs {
		return 0, 0, false
	}
	var slopes []float64
	for i := 0; i < len(c.obs); i++ {
		for j := i + 1; j < len(c.obs); j++ {
			a, b := c.obs[i], c.obs[j]
			spread := b.est - a.est
			if spread < 0 {
				spread = -spread
			}
			need := float64(calMinSpreadAbs)
			if rel := calMinSpreadRel * math.Max(a.est, b.est); rel > need {
				need = rel
			}
			if spread < need {
				continue
			}
			slopes = append(slopes, (b.real-a.real)/(b.est-a.est))
		}
	}
	if len(slopes) == 0 {
		return 0, 0, false // no pair spans enough range to inform a slope
	}
	slope = medianOf(slopes)
	// Clamp the slope FIRST, then fit the intercept to the clamped line.
	// Deriving the intercept from an unclamped slope and then clamping it
	// separately leaves a line through neither the data nor the clamp — it
	// produced the most aggressive budget the bounds allowed.
	if slope < calMinSlope {
		slope = calMinSlope
	}
	if slope > calMaxSlope {
		slope = calMaxSlope
	}
	residuals := make([]float64, 0, len(c.obs))
	for _, o := range c.obs {
		residuals = append(residuals, o.real-slope*o.est)
	}
	intercept = medianOf(residuals)
	if intercept < 0 {
		intercept = 0
	}
	if intercept > calMaxIntercept {
		intercept = calMaxIntercept
	}
	return slope, intercept, true
}

// medianOf returns the median without mutating the caller's slice.
func medianOf(xs []float64) float64 {
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	n := len(c)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

// Budget converts a REAL-token allowance into the ESTIMATE-space budget the
// ladder should compact against, so that a transcript passing the ladder's
// check also fits the server.
//
// Solving real = intercept + slope·estimate ≤ allowance gives
// estimate ≤ (allowance − intercept)/slope.
//
// With no usable fit it returns the allowance UNCHANGED — today's behaviour
// exactly, so an uncalibrated loop is byte-identical to before this file
// existed. The floor mirrors inputBudget()'s: a degenerate fit degrades to a
// small budget (aggressive compaction) rather than to a negative one.
func (c *TokenCalibrator) Budget(allowance int) int {
	slope, intercept, ok := c.Fit()
	if !ok {
		return allowance
	}
	b := (float64(allowance) - intercept) / slope
	// Bound the TOTAL correction. Calibration is a correction, not an
	// authority: however implausible the observations, it may not cut usable
	// context by more than half. Without this bound a slope pinned at its
	// clamp divides the budget by three — which is not distrusting the fit,
	// it is acting on data already judged implausible.
	if floor := calMaxShrink * float64(allowance); b < floor {
		b = floor
	}
	if b < calMinBudget {
		b = calMinBudget
	}
	if out := int(b); out < allowance {
		return out
	}
	// A fit implying MORE headroom than the raw allowance is not a licence to
	// exceed it: the allowance already encodes the served window.
	return allowance
}
