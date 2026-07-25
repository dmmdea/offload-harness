package agent

// tokencal.go — online calibration of the compaction budget against the
// server's REAL token counts.
//
// THE DEFECT THIS FIXES (measured, Phase D / ADR 0017). The ladder decides
// whether to compact by comparing estimateTokens() — a flat chars/4 heuristic —
// against inputBudget(). That estimate undercounts reality badly and
// content-dependently: measured real/estimated p95 was 1.69 overall and 1.90 on
// code, while the shipped safety margin covers only 1.077. The consequence was
// not theoretical. Three real transcripts were rejected by the server with
// exceed_context_size while the ladder DECLINED TO COMPACT, because by its own
// estimate they fit:
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
// budget to a degenerate value.
const (
	calMinSlope      = 1.0  // never assume the server counts FEWER tokens than we estimate
	calMaxSlope      = 3.0  // beyond this, distrust the fit rather than near-eliminate the budget
	calMaxIntercept  = 4096 // a fixed payload larger than this is not credible
	calMinObs        = 2    // one point cannot separate slope from intercept
	calMinBudget     = 256  // the floor inputBudget() already guarantees
)

// TokenCalibrator learns real≈intercept+slope·estimate from observed responses
// and corrects a token budget expressed in ESTIMATE space.
//
// Keeping the correction on the BUDGET (rather than scaling estimateTokens)
// matters: every compaction rung compares estimateTokens against the budget as
// it works, so both sides must stay in the same space or the ladder's internal
// stopping conditions become inconsistent.
type TokenCalibrator struct {
	n                          int
	sumEst, sumReal            float64
	sumEstEst, sumEstReal      float64
	minEst, maxEst             float64
}

// Observe records one (estimate, real) pair for a request that the server
// accepted. Non-positive or absent values are ignored — an unmeasured response
// must never move the calibration.
func (c *TokenCalibrator) Observe(est, real int) {
	if est <= 0 || real <= 0 {
		return
	}
	e, r := float64(est), float64(real)
	if c.n == 0 || e < c.minEst {
		c.minEst = e
	}
	if c.n == 0 || e > c.maxEst {
		c.maxEst = e
	}
	c.n++
	c.sumEst += e
	c.sumReal += r
	c.sumEstEst += e * e
	c.sumEstReal += e * r
}

// Observations reports how many usable samples the calibrator holds.
func (c *TokenCalibrator) Observations() int { return c.n }

// Fit returns the current (slope, intercept, ok). ok is false until there are
// at least two observations that actually differ in size — with a single
// estimate value the two terms are not separable, and pretending otherwise is
// how a fit produces confident nonsense.
func (c *TokenCalibrator) Fit() (slope, intercept float64, ok bool) {
	if c.n < calMinObs || c.maxEst-c.minEst < 1 {
		return 0, 0, false
	}
	n := float64(c.n)
	den := n*c.sumEstEst - c.sumEst*c.sumEst
	if den == 0 {
		return 0, 0, false
	}
	slope = (n*c.sumEstReal - c.sumEst*c.sumReal) / den
	intercept = (c.sumReal - slope*c.sumEst) / n
	// Clamp into the credible range. A slope below 1 would claim the server
	// counts fewer tokens than chars/4 predicts; an intercept below 0 would
	// claim a negative fixed payload.
	if slope < calMinSlope {
		slope = calMinSlope
	}
	if slope > calMaxSlope {
		slope = calMaxSlope
	}
	if intercept < 0 {
		intercept = 0
	}
	if intercept > calMaxIntercept {
		intercept = calMaxIntercept
	}
	return slope, intercept, true
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
