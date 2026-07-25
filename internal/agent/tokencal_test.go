package agent

import (
	"context"
	"strings"
	"testing"
)

// TestCalibratorUncalibratedIsIdentity: with no usable observations the budget
// must be returned UNCHANGED. This is the compatibility contract — an
// uncalibrated loop behaves exactly as it did before calibration existed.
func TestCalibratorUncalibratedIsIdentity(t *testing.T) {
	var c TokenCalibrator
	if got := c.Budget(6656); got != 6656 {
		t.Fatalf("no observations: budget = %d, want 6656 unchanged", got)
	}
	c.Observe(1000, 1800) // one point cannot separate slope from intercept
	if got := c.Budget(6656); got != 6656 {
		t.Fatalf("one observation: budget = %d, want 6656 unchanged", got)
	}
	// Two observations at the SAME estimate are still not separable.
	c = TokenCalibrator{}
	c.Observe(1000, 1800)
	c.Observe(1000, 1800)
	if _, _, ok := c.Fit(); ok {
		t.Fatal("identical estimates must not produce a fit — the terms are not separable")
	}
	if got := c.Budget(6656); got != 6656 {
		t.Fatalf("degenerate fit: budget = %d, want unchanged", got)
	}
	// Unmeasured / nonsense observations are ignored entirely.
	c = TokenCalibrator{}
	c.Observe(0, 500)
	c.Observe(500, 0)
	c.Observe(-1, -1)
	if c.Observations() != 0 {
		t.Fatalf("observations = %d, want 0 — unmeasured responses must not move the calibration", c.Observations())
	}
}

// TestCalibratorRecoversTwoTermModel: the fit must separate the FIXED payload
// (tool specs, invisible to estimateTokens) from CONTENT DENSITY. A one-term
// model conflates them and mis-corrects at both ends.
func TestCalibratorRecoversTwoTermModel(t *testing.T) {
	// Ground truth: real = 528 + 1.6*est (528 ≈ the measured tool-spec payload).
	const wantIntercept, wantSlope = 528.0, 1.6
	var c TokenCalibrator
	for _, est := range []int{300, 900, 2000, 4000} {
		c.Observe(est, int(wantIntercept+wantSlope*float64(est)))
	}
	slope, intercept, ok := c.Fit()
	if !ok {
		t.Fatal("fit should be available with four distinct observations")
	}
	if slope < wantSlope-0.05 || slope > wantSlope+0.05 {
		t.Errorf("slope = %v, want ~%v", slope, wantSlope)
	}
	if intercept < wantIntercept-20 || intercept > wantIntercept+20 {
		t.Errorf("intercept = %v, want ~%v", intercept, wantIntercept)
	}
	// And the corrected budget must be the one that actually fits:
	// real(estimate) <= allowance.
	allowance := 6656
	b := c.Budget(allowance)
	if realAt := wantIntercept + wantSlope*float64(b); realAt > float64(allowance)+1 {
		t.Fatalf("budget %d implies %v real tokens, over the %d allowance", b, realAt, allowance)
	}
	// It must actually be tighter than the raw allowance — that is the point.
	if b >= allowance {
		t.Fatalf("budget = %d, expected tighter than %d", b, allowance)
	}
}

// TestCalibratorReproducesTheMeasuredFailure: the three real transcripts that
// were REJECTED while the ladder declined to compact. With calibration from
// earlier steps of the same run, the budget must drop below their estimate, so
// the ladder now fires instead of shipping a doomed request.
func TestCalibratorReproducesTheMeasuredFailure(t *testing.T) {
	// hv-json-ledger step 4: estimate 6,224 vs 11,369 real. Earlier steps of
	// the same transcript are the calibration data a real run would have.
	var c TokenCalibrator
	c.Observe(800, 528+int(1.74*800))
	c.Observe(2400, 528+int(1.74*2400))
	c.Observe(4000, 528+int(1.74*4000))

	const failingEstimate = 6224
	const rawAllowance = 6656 // ctx 8192 - maxOut 1024 - margin 512

	if failingEstimate >= rawAllowance {
		t.Fatal("fixture wrong: the point is that the estimate LOOKED like it fit")
	}
	corrected := c.Budget(rawAllowance)
	if failingEstimate <= corrected {
		t.Fatalf("estimate %d still under corrected budget %d — the ladder would AGAIN decline to compact on a request the server rejects", failingEstimate, corrected)
	}
}

// TestCalibratorResistsOutliers pins the failure a review demonstrated: with
// least squares, ONE anomalous usage.prompt_tokens (a proxy reporting
// cumulative or prompt+completion tokens) dragged the slope to its clamp and
// cut the budget to 33% for the rest of the run, with no recovery.
func TestCalibratorResistsOutliers(t *testing.T) {
	healthy := func() *TokenCalibrator {
		c := &TokenCalibrator{}
		for i := 1; i <= 12; i++ {
			est := 400 * i
			c.Observe(est, 528+int(1.03*float64(est))) // prose: density ~1.03 + specs
		}
		return c
	}
	clean := healthy().Budget(6656)
	if clean < 5500 {
		t.Fatalf("healthy prose run budget = %d, expected ~5.9k (only the fixed spec payload removed)", clean)
	}
	// Same run, one 10x reading injected.
	poisoned := healthy()
	poisoned.Observe(2000, 10*(528+int(1.03*2000)))
	got := poisoned.Budget(6656)
	if got < clean-600 {
		t.Fatalf("one outlier moved the budget %d -> %d; a median fit must absorb a single bad reading", clean, got)
	}
	// And it must AGE OUT: further healthy observations restore the budget.
	for i := 1; i <= calKeep; i++ {
		est := 400 + 100*i
		poisoned.Observe(est, 528+int(1.03*float64(est)))
	}
	if recovered := poisoned.Budget(6656); recovered < clean-300 {
		t.Fatalf("budget did not recover after the outlier aged out: %d vs clean %d", recovered, clean)
	}
}

// TestCalibratorRejectsDegenerateSpread: two observations whose estimates
// differ by a token carry no slope information. A one-token spread previously
// satisfied the guard, so the noise slope pinned to the clamp and cut the
// budget to a third.
func TestCalibratorRejectsDegenerateSpread(t *testing.T) {
	var c TokenCalibrator
	c.Observe(5000, 6600)
	c.Observe(5001, 6640) // 1-token spread, 40-token jump => slope 40
	c.Observe(5002, 6560)
	if _, _, ok := c.Fit(); ok {
		t.Fatal("a degenerate spread must not produce a fit")
	}
	if got := c.Budget(6656); got != 6656 {
		t.Fatalf("degenerate spread: budget = %d, want the allowance unchanged", got)
	}
	// Once a genuinely wide-spread observation arrives, the fit engages.
	c.Observe(1000, 528+int(1.5*1000))
	if _, _, ok := c.Fit(); !ok {
		t.Fatal("a wide-spread observation should enable the fit")
	}
}

// TestCalibratorNeverShrinksBeyondHalf bounds the blast radius of any fit.
func TestCalibratorNeverShrinksBeyondHalf(t *testing.T) {
	var c TokenCalibrator
	// Absurdly dense readings that pin the slope at its clamp.
	for i := 1; i <= 6; i++ {
		est := 1000 * i
		c.Observe(est, 50*est)
	}
	const allowance = 6656
	got := c.Budget(allowance)
	if got < int(calMaxShrink*allowance) {
		t.Fatalf("budget %d cut below the %v floor of allowance %d — calibration is a correction, not an authority",
			got, calMaxShrink, allowance)
	}
	if got >= allowance {
		t.Fatalf("budget %d should still be tightened for genuinely dense content", got)
	}
}

// TestCalibratorInterceptFitsTheClampedSlope: deriving the intercept from an
// UNCLAMPED slope and clamping it afterwards leaves a line through neither the
// data nor the clamp, and yields the most aggressive budget the bounds allow.
func TestCalibratorInterceptFitsTheClampedSlope(t *testing.T) {
	var c TokenCalibrator
	for i := 1; i <= 6; i++ {
		est := 1000 * i
		c.Observe(est, 500+8*est) // true slope 8 -> clamps to calMaxSlope
	}
	slope, intercept, ok := c.Fit()
	if !ok {
		t.Fatal("expected a fit")
	}
	if slope != calMaxSlope {
		t.Fatalf("slope = %v, want the clamp %v", slope, calMaxSlope)
	}
	// With the slope clamped to 3 and the data at 8, residuals are large and
	// POSITIVE, so the intercept must be positive — not driven negative and
	// clamped to zero the way the unclamped-slope order produced.
	if intercept <= 0 {
		t.Fatalf("intercept = %v; refitting to the clamped slope must yield a positive fixed term", intercept)
	}
}

// TestCalibratorClamps: a pathological observation sequence must never drive
// the budget to a degenerate value.
func TestCalibratorClamps(t *testing.T) {
	// Server claims FEWER tokens than we estimate: slope clamps at 1.0, so the
	// budget is never inflated above the allowance.
	var c TokenCalibrator
	c.Observe(1000, 100)
	c.Observe(5000, 200)
	if slope, _, ok := c.Fit(); ok && slope < calMinSlope {
		t.Fatalf("slope = %v, must clamp at %v", slope, calMinSlope)
	}
	if got := c.Budget(6656); got > 6656 {
		t.Fatalf("budget = %d, must never exceed the allowance", got)
	}
	// Absurdly dense content: slope clamps, budget stays above the floor.
	c = TokenCalibrator{}
	c.Observe(100, 100000)
	c.Observe(200, 200000)
	slope, _, _ := c.Fit()
	if slope > calMaxSlope {
		t.Fatalf("slope = %v, must clamp at %v", slope, calMaxSlope)
	}
	if got := c.Budget(6656); got < calMinBudget {
		t.Fatalf("budget = %d, must not fall below the floor %d", got, calMinBudget)
	}
	// A huge fixed term clamps too, and the budget stays usable.
	c = TokenCalibrator{}
	c.Observe(100, 99999)
	c.Observe(4000, 103899)
	if _, intercept, _ := c.Fit(); intercept > calMaxIntercept {
		t.Fatalf("intercept = %v, must clamp at %v", intercept, calMaxIntercept)
	}
	if got := c.Budget(6656); got < calMinBudget {
		t.Fatalf("budget = %d below floor", got)
	}
}

// --- loop-level: the ladder must now fire where it previously did not -------

// usageClient reports realistic server accounting: real = fixed + density*est,
// so the loop can calibrate exactly as it does live.
type usageClient struct {
	fixed   int
	density float64
	calls   int
	seenEst []int
	fired   []bool
}

func (u *usageClient) Chat(_ context.Context, msgs []Msg, _ []ToolSpec, _ int) (Completion, error) {
	est := estimateTokens(msgs)
	u.seenEst = append(u.seenEst, est)
	u.calls++
	// Whether compaction touched the transcript is visible as an artifact.
	compacted := false
	for _, m := range msgs {
		if m.Role == "tool" && IsCompactionArtifact(m.Content) {
			compacted = true
		}
	}
	u.fired = append(u.fired, compacted)
	real := u.fixed + int(u.density*float64(est))
	if u.calls >= 6 {
		return Completion{Msg: Msg{Role: "assistant", Content: "done"}, FinishReason: "stop",
			Serve: &ServeStats{PromptN: real, UsagePromptTokens: real}}, nil
	}
	return Completion{
		Msg:          Msg{Role: "assistant", ToolCalls: []ToolCall{{ID: "c" + string(rune('0'+u.calls)), Name: "read_file", Args: `{"p":"` + string(rune('a'+u.calls)) + `"}`}}},
		FinishReason: "tool_calls",
		Serve:        &ServeStats{PromptN: real, UsagePromptTokens: real},
	}, nil
}

// TestLoopCalibratesAndCompactsSooner: with a server whose real token count is
// 1.8x our estimate plus a fixed payload, the loop must learn that and start
// compacting BEFORE the estimate alone would have triggered it. Without
// calibration this transcript sails past the budget check and would be rejected
// live — the exact measured failure.
func TestLoopCalibratesAndCompactsSooner(t *testing.T) {
	body := strings.Repeat("dense json-ish content {\"k\":\"v\"} that tokenizes far denser than chars/4\n", 110)
	tools := []Tool{{ToolSpec: ToolSpec{Name: "read_file"}, Exec: func(_ context.Context, _ string) (string, error) {
		return body, nil
	}}}

	run := func(withCal bool) (fires, maxEst, exhausted int) {
		c := &usageClient{fixed: 528, density: 1.8}
		loop := NewLoop(c, tools, 8).WithSystem("sys").WithContextTokens(8192).WithMaxTokens(1024).
			WithToolResultCap(len(body) + 1).WithSkeletonPrune(true)
		if !withCal {
			loop.DisableTokenCalibration()
		}
		res, err := loop.Run(context.Background(), "objective: read several dense files")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		for i, f := range c.fired {
			if f {
				fires++
			}
			if c.seenEst[i] > maxEst {
				maxEst = c.seenEst[i]
			}
		}
		return fires, maxEst, res.CompactionsExhausted
	}

	calFires, calMaxEst, calExhausted := run(true)
	rawFires, rawMaxEst, _ := run(false)

	if calFires <= rawFires {
		t.Fatalf("calibrated run compacted %d times vs uncalibrated %d — calibration must make the ladder fire SOONER on dense content", calFires, rawFires)
	}
	// Calibration must substantially reduce the peak REAL prompt size.
	const allowance = 8192 - 1024
	realCal := 528 + int(1.8*float64(calMaxEst))
	realRaw := 528 + int(1.8*float64(rawMaxEst))
	if realCal >= realRaw {
		t.Fatalf("calibrated peak real %d not below uncalibrated %d", realCal, realRaw)
	}
	if realRaw <= allowance {
		t.Fatalf("fixture too weak: the uncalibrated arm (%d real) never reached the failure regime (>%d)", realRaw, allowance)
	}
	t.Logf("peak real prompt: uncalibrated %d (over the %d allowance — rejected live), calibrated %d",
		realRaw, allowance, realCal)

	// HONEST LIMIT, asserted rather than assumed: calibration makes the ladder
	// fire sooner, but it cannot conjure headroom the ladder is forbidden to
	// reclaim. Here keepRecent protects both large tool results, so the ladder
	// EXHAUSTS and the request still goes out over budget — reported via
	// CompactionsExhausted (ADR 0016) rather than silently. Calibration and the
	// keep-recent floor are independent limits; this pins that they compose the
	// way the docs claim.
	if realCal > allowance && calExhausted == 0 {
		t.Errorf("calibrated run exceeded the allowance (%d > %d) WITHOUT reporting an exhausted ladder — that combination should be impossible",
			realCal, allowance)
	}
}
