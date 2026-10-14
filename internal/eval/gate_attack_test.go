package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// These tests attack the gate the way a candidate that wants to be adopted would: by not
// finishing the tasks it fails, by hiding what it costs, by moving the pre-registered rules, by
// running few tasks with few resamples. Each one asserts the behaviour that stops it.

// ---- fixtures ---------------------------------------------------------------------------------

func hex64(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// armFrom builds an arm of n tasks (ids t000..) whose attempts come from mk.
func armFrom(tag string, n int, mk func(i int) []Attempt) Arm {
	a := Arm{Stamps: testStamps(tag)}
	ids := taskIDs(n)
	for i := 0; i < n; i++ {
		a.Tasks = append(a.Tasks, Task{ID: ids[i], Attempts: mk(i)})
	}
	return a
}

func tokenMetric(v float64) map[string]*float64 { return map[string]*float64{tokensName: f64(v)} }

// passTok is a passing attempt with a score and a token cost.
func passTok(score, tokens float64) Attempt {
	return Attempt{Outcome: OutcomePass, Score: f64(score), Metrics: tokenMetric(tokens)}
}

// passOnly is a passing attempt with a score and no cost.
// incScore is the incumbent's score on task i in the tests that are about something other than the primary: a
// little spread (1 and 0.98), because a sample whose pairs all differ by the same amount has no spread to read
// and its lower end is held back.
func incScore(i int) float64 {
	if i%2 == 1 {
		return 0.98
	}
	return 1
}

// armScore is the 0..10 score of task i for the efficiency tests: the incumbent ("i") has a little spread
// (8.0 and 8.1), the candidate scores 8.
func armScore(tag string, i int) float64 {
	if tag == "i" && i%2 == 1 {
		return 8.1
	}
	return 8
}

func passOnly(score float64) Attempt { return Attempt{Outcome: OutcomePass, Score: f64(score)} }

// failOnly is a task_fail attempt with a score and no cost.
func failOnly(score float64) Attempt { return Attempt{Outcome: OutcomeTaskFail, Score: f64(score)} }

// noScore is an attempt of a class that never carries a score.
func noScore(o AttemptOutcome) Attempt { return Attempt{Outcome: o} }

// niUnitPre is a non-inferiority pre-registration on a unit-interval primary (default margin 0.05).
func niUnitPre(n int) PreReg {
	p := niPre(n)
	p.Primary = PreRegPrimary{Name: "goal_reached", Class: ClassUnitInterval}
	return p
}

// customPre is a pre-registration on a custom primary with the given range.
func customPre(mode string, n int, lo, hi float64) PreReg {
	p := supPre(n)
	p.Mode = mode
	p.Primary = PreRegPrimary{Name: "points", Class: ClassCustom, Range: []float64{lo, hi}}
	if mode == ModeNonInferiority {
		p.Efficiency = []PreRegEfficiency{{Name: tokensName, Class: EffTokens}}
		p.Primary.Margin = f64(1)
	}
	return p
}

// refInterval is an independent reading of the paired bootstrap: the same resampling as
// BootstrapDeltaMean, the delta as the mean of the replicates, and the interval at the q and
// 1 - q percentiles of the sorted replicates.
func refInterval(inc, cand []float64, B int, seed int64, q float64) (delta, lo, hi float64) {
	n := len(inc)
	rng := rand.New(rand.NewSource(seed))
	reps := make([]float64, B)
	for b := range reps {
		var si, sc float64
		for i := 0; i < n; i++ {
			idx := rng.Intn(n)
			si += inc[idx]
			sc += cand[idx]
		}
		reps[b] = (sc - si) / float64(n)
	}
	for _, v := range reps {
		delta += v
	}
	delta /= float64(B)
	sort.Float64s(reps)
	at := func(q float64) float64 {
		idx := int(q * float64(B))
		if idx < 0 {
			idx = 0
		}
		if idx >= B {
			idx = B - 1
		}
		return reps[idx]
	}
	return delta, at(q), at(1 - q)
}

// ---- the statistic does not move ------------------------------------------------------------

// BootstrapDeltaMean is shared with the legacy callers (B = 2000, seed = 42): these values were
// recorded from the function before the gate learned to read other percentiles, and must not move.
func TestBootstrapDeltaMeanIsByteIdenticalForTheExistingCallers(t *testing.T) {
	inc := []float64{0, 0.125, 0.25, 0.5, 0.75, 1, 0.375, 0.625, 0.875, 0, 0.5, 0.25, 1, 0.125, 0.75, 0.5}
	cand := []float64{0.25, 0.125, 0.5, 0.5, 1, 1, 0.5, 0.625, 1, 0.125, 0.5, 0.5, 1, 0.375, 0.75, 0.875}
	for _, c := range []struct {
		B            int
		seed         int64
		delta, lo, h float64
	}{
		{2000, 42, 0.123953125, 0.0625, 0.1875},
		{500, 1, 0.126359375, 0.0703125, 0.1875},
		{10000, 1, 0.12544765625, 0.0625, 0.1875},
		{7, 3, 0.12053571428571429, 0.09375, 0.1484375},
	} {
		d, lo, hi := BootstrapDeltaMean(inc, cand, c.B, c.seed)
		if d != c.delta || lo != c.lo || hi != c.h {
			t.Errorf("B=%d seed=%d: got %#v %#v %#v, want %#v %#v %#v", c.B, c.seed, d, lo, hi, c.delta, c.lo, c.h)
		}
		// The independent reading agrees on the plain 95% interval.
		rd, rlo, rhi := refInterval(inc, cand, c.B, c.seed, 0.025)
		if rd != d || rlo != lo || rhi != hi {
			t.Errorf("B=%d seed=%d: the reference reading %v %v %v differs from BootstrapDeltaMean %v %v %v", c.B, c.seed, rd, rlo, rhi, d, lo, hi)
		}
	}
	// The guards are unchanged: nothing to conclude is zeros.
	if d, lo, hi := BootstrapDeltaMean(inc[:1], cand[:1], 2000, 1); d != 0 || lo != 0 || hi != 0 {
		t.Errorf("n < 2 must give zeros, got %v %v %v", d, lo, hi)
	}
	if d, lo, hi := BootstrapDeltaMean(inc, cand[:3], 2000, 1); d != 0 || lo != 0 || hi != 0 {
		t.Errorf("unaligned input must give zeros, got %v %v %v", d, lo, hi)
	}
	if d, lo, hi := BootstrapDeltaMean(inc, cand, 0, 1); d != 0 || lo != 0 || hi != 0 {
		t.Errorf("B < 1 must give zeros, got %v %v %v", d, lo, hi)
	}
}

// ---- worst-score imputation -----------------------------------------------------------------

// A candidate that times out or breaks inside a task is not skipped: the attempt enters the
// task's mean at the worst score. Incumbent [pass, fail, fail] against candidate [pass, timeout,
// infra_error] is a tie, not a win.
func TestWithinTaskTimeoutsCannotMakeACandidateBetter(t *testing.T) {
	const n = 16
	b, h := freeze(t, supPre(n), nil)
	inc := armFrom("i", n, func(int) []Attempt { return []Attempt{passOnly(1), failOnly(0), failOnly(0)} })
	cand := armFrom("c", n, func(int) []Attempt {
		return []Attempt{passOnly(1), noScore(OutcomeTimeout), noScore(OutcomeInfraError)}
	})
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	if r.Verdict == VerdictAdopt {
		t.Fatalf("a candidate whose two other attempts did not finish was adopted: %s", marshal(t, r))
	}
	if r.Delta != 0 || !(r.Lo < 0) || !(r.Hi > 0) {
		t.Fatalf("the unfinished attempts score 0, so the candidate ties the incumbent: delta %v lo %v hi %v", r.Delta, r.Lo, r.Hi)
	}
	if r.ImputedAttempts != 2*n || r.UnusableAttempts.Candidate != 2*n || r.UnusableAttempts.Incumbent != 0 {
		t.Fatalf("imputed %d, unusable %+v: want %d and {0 %d}", r.ImputedAttempts, r.UnusableAttempts, 2*n, 2*n)
	}
	if want := 2.0 / 3.0; math.Abs(r.ExcludedShare-want) > 1e-12 || r.Verdict != VerdictInconclusive || !strings.Contains(r.Reason, "excluded_share") {
		t.Fatalf("two thirds of the candidate's attempts are unusable: %s %s (share %v)", r.Verdict, r.Reason, r.ExcludedShare)
	}
}

// Four whole-task timeouts out of 20 sit exactly at the cap (0.20), four times the 0.05 margin.
// They no longer vanish from the mean: the candidate scores 0 on them and cannot be adopted.
func TestWholeTaskTimeoutsAtTheCapStillCountAgainstTheCandidate(t *testing.T) {
	const n = 20
	b, h := freeze(t, niUnitPre(n), nil)
	inc := armFrom("i", n, func(i int) []Attempt { return []Attempt{passTok(1, 1000+float64(i))} })
	cand := armFrom("c", n, func(i int) []Attempt {
		if i < 4 {
			return []Attempt{{Outcome: OutcomeTimeout, Metrics: tokenMetric(5000)}}
		}
		return []Attempt{passTok(1, 500+float64(i))}
	})
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	if r.Verdict == VerdictAdopt {
		t.Fatalf("a candidate that times out on 4 of 20 tasks was adopted: %s", marshal(t, r))
	}
	if r.NUsed != n || r.ExcludedPairs != 0 || r.ImputedAttempts != 4 {
		t.Fatalf("every declared task is a usable pair and four attempts were imputed: %+v", r)
	}
	if !(r.Delta < -0.1 && r.Delta > -0.3) || !(r.Lo < -0.05) {
		t.Fatalf("four zeros out of 20 pull the delta to about -0.2 and the lower end below -m: delta %v lo %v", r.Delta, r.Lo)
	}
	if math.Abs(r.ExcludedShare-0.2) > 1e-12 {
		t.Fatalf("share %v: 4 of 20 candidate attempts", r.ExcludedShare)
	}
}

// The worst score is the bad end of the primary class's range, and an unusable candidate attempt,
// a missing task, a cheat on either arm and an explicit not_run all enter at it.
func TestImputationUsesTheWorstScoreOfTheClass(t *testing.T) {
	const n = 20
	for _, tc := range []struct {
		name      string
		pre       PreReg
		incScore  float64
		wantDelta float64
		width     float64 // the width of the score range
	}{
		{"unit_interval", func() PreReg { p := supPre(n); return p }(), 1, -1, 1},
		{"judge_0_10", func() PreReg { p := supPre(n); p.Primary.Class = ClassJudge010; return p }(), 8, -8, 10},
		{"custom [-5, 20]", customPre(ModeSuperiority, n, -5, 20), 7, -12, 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, h := freeze(t, tc.pre, nil)
			inc := armFrom("i", n, func(int) []Attempt { return []Attempt{passOnly(tc.incScore)} })
			// Five ways not to have a score: a timeout, an infra_error, a judge_error, an explicit
			// not_run, and a task the arm never lists (below).
			outcomes := []AttemptOutcome{OutcomeTimeout, OutcomeInfraError, OutcomeJudgeError, OutcomeNotRun}
			cand := armFrom("c", n, func(i int) []Attempt { return []Attempt{noScore(outcomes[i%len(outcomes)])} })
			cand.Tasks = cand.Tasks[:n-5] // the last five are never listed
			r := mustGate(t, b, h, inputFor(h, inc, cand))
			// Every pair differs by the same amount, so the interval is the rule of three about it, clipped to the range.
			half := 3 * tc.width / n
			wantLo, wantHi := math.Max(-tc.width, tc.wantDelta-half), math.Min(tc.width, tc.wantDelta+half)
			if r.NUsed != n || r.Delta != tc.wantDelta || math.Abs(r.Lo-wantLo) > 1e-12 || math.Abs(r.Hi-wantHi) > 1e-12 {
				t.Fatalf("every candidate task scores the worst: want delta %v on [%v, %v], got n=%d %v [%v, %v]", tc.wantDelta, wantLo, wantHi, r.NUsed, r.Delta, r.Lo, r.Hi)
			}
			if r.ImputedAttempts != n || r.AttemptCounts.Candidate["not_run"] != 3+5 {
				t.Fatalf("all %d candidate attempts are imputed (5 are placeholders): %+v %d", n, r.AttemptCounts.Candidate, r.ImputedAttempts)
			}
			if r.Verdict != VerdictInconclusive || !strings.Contains(r.Reason, "excluded_share=1") {
				t.Fatalf("a candidate that finished nothing is INCONCLUSIVE on its unusable share: %s %s", r.Verdict, r.Reason)
			}
		})
	}
}

// A cheat enters as the worst score of the class on both arms (it used to be a hard-coded 0, which
// is the BEST score of a lower-is-better class and a middling one of a custom range).
func TestCheatEntersAsTheWorstScoreOfTheClass(t *testing.T) {
	const n = 16
	b, h := freeze(t, customPre(ModeSuperiority, n, 2, 12), nil)
	cheat := func(int) []Attempt { return []Attempt{{Outcome: OutcomeCheat, Score: f64(12)}} }
	passing := func(int) []Attempt { return []Attempt{passOnly(7)} }
	// Candidate cheats: it scores 2 (the worst), not 0 and not the 12 it claims.
	r := mustGate(t, b, h, inputFor(h, armFrom("i", n, passing), armFrom("c", n, cheat)))
	if r.Delta != -5 || r.Cheats.Candidate != n || r.NUsed != n {
		t.Fatalf("a cheating candidate scores the worst (2): want delta -5, got %+v", r)
	}
	// Incumbent cheats: it scores 2 as well.
	r = mustGate(t, b, h, inputFor(h, armFrom("i", n, cheat), armFrom("c", n, passing)))
	if r.Delta != 5 || r.Cheats.Incumbent != n {
		t.Fatalf("a cheating incumbent scores the worst (2): want delta +5, got %+v", r)
	}
}

// worst reads the direction too: the bad end of a lower-is-better range is its top.
func TestWorstIsTheBadEndOfTheRange(t *testing.T) {
	for _, tc := range []struct {
		p    Primary
		want float64
	}{
		{Primary{Class: ClassUnitInterval, HigherIsBetter: true}, 0},
		{Primary{Class: ClassUnitInterval, HigherIsBetter: false}, 1},
		{Primary{Class: ClassJudge010, HigherIsBetter: true}, 0},
		{Primary{Class: ClassJudge010, HigherIsBetter: false}, 10},
		{Primary{Class: ClassCustom, HigherIsBetter: true, Range: []float64{-3, 8}}, -3},
		{Primary{Class: ClassCustom, HigherIsBetter: false, Range: []float64{-3, 8}}, 8},
	} {
		if got := tc.p.worst(); got != tc.want {
			t.Errorf("%+v: worst = %v, want %v", tc.p, got, tc.want)
		}
	}
}

// The incumbent is not imputed: an attempt of its that did not finish is skipped, and a task with
// no finished attempt is an excluded pair. It is not the candidate that ran those.
func TestIncumbentUnusableAttemptsAreSkippedAndExcludeThePair(t *testing.T) {
	const n = 20
	b, h := freeze(t, supPre(n), nil)
	inc := armFrom("i", n, func(i int) []Attempt {
		switch {
		case i < 3: // pass + timeout: the task is the pass alone
			return []Attempt{passOnly(1), noScore(OutcomeTimeout)}
		case i < 5: // nothing usable: the pair is excluded
			return []Attempt{noScore(OutcomeJudgeError), noScore(OutcomeNotRun)}
		}
		return []Attempt{passOnly(0)}
	})
	cand := armFrom("c", n, func(int) []Attempt { return []Attempt{passOnly(1)} })
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	if r.ExcludedPairs != 2 || r.NUsed != 18 {
		t.Fatalf("t003 and t004 have no usable incumbent attempt: %+v", r)
	}
	if r.UnusableAttempts.Incumbent != 7 || r.ImputedAttempts != 0 || r.UnusableAttempts.Candidate != 0 {
		t.Fatalf("incumbent unusable attempts are counted, never imputed: %+v imputed %d", r.UnusableAttempts, r.ImputedAttempts)
	}
	// t000..t002 score 1 (the timeout is skipped, not averaged in as 0); the other 15 score 0.
	incS := append(repeatF(1, 3), repeatF(0, 15)...)
	d, lo, hi := primaryInterval(incS, repeatF(1, 18), 1)
	if r.Delta != d || r.Lo != lo || r.Hi != hi {
		t.Fatalf("the incumbent's task scores are wrong: gate %v %v %v want %v %v %v", r.Delta, r.Lo, r.Hi, d, lo, hi)
	}
}

// excluded_share is the largest of three ratios: excluded pairs over declared tasks, unusable
// incumbent attempts over incumbent attempts, unusable candidate attempts over candidate attempts.
func TestExcludedShareIsTheLargestOfTheThreeRatios(t *testing.T) {
	good := func(int) []Attempt { return []Attempt{passOnly(1)} }
	t.Run("excluded pairs", func(t *testing.T) {
		const n = 25
		b, h := freeze(t, supPre(n), nil)
		// 6 incumbent tasks hold one unusable attempt; the other 19 hold five good ones, so the
		// incumbent's attempt ratio is 6/101 and only the pair ratio, 6/25, is over the cap.
		inc := armFrom("i", n, func(i int) []Attempt {
			if i < 6 {
				return []Attempt{noScore(OutcomeInfraError)}
			}
			return []Attempt{passOnly(0), passOnly(0), passOnly(0), passOnly(0), passOnly(0)}
		})
		r := mustGate(t, b, h, inputFor(h, inc, armFrom("c", n, good)))
		if r.NUsed != 19 || r.ExcludedPairs != 6 || r.ExcludedShare != 6.0/25.0 || r.Verdict != VerdictInconclusive || !strings.Contains(r.Reason, "excluded_share=0.24") {
			t.Fatalf("want INCONCLUSIVE at share 0.24 with 19 pairs: %s", marshal(t, r))
		}
	})
	t.Run("incumbent attempts", func(t *testing.T) {
		const n = 20
		b, h := freeze(t, supPre(n), nil)
		inc := armFrom("i", n, func(i int) []Attempt {
			if i < 10 {
				return []Attempt{passOnly(0), noScore(OutcomeTimeout)}
			}
			return []Attempt{passOnly(0)}
		})
		r := mustGate(t, b, h, inputFor(h, inc, armFrom("c", n, good)))
		if r.ExcludedPairs != 0 || r.NUsed != n || r.ExcludedShare != 10.0/30.0 || r.Verdict != VerdictInconclusive || !strings.Contains(r.Reason, "excluded_share=0.33") {
			t.Fatalf("want INCONCLUSIVE at share 1/3 without an excluded pair: %s", marshal(t, r))
		}
	})
	t.Run("candidate attempts", func(t *testing.T) {
		const n = 20
		b, h := freeze(t, supPre(n), nil)
		cand := armFrom("c", n, func(i int) []Attempt {
			if i < 10 {
				return []Attempt{passOnly(1), noScore(OutcomeTimeout)}
			}
			return []Attempt{passOnly(1)}
		})
		r := mustGate(t, b, h, inputFor(h, armFrom("i", n, func(int) []Attempt { return []Attempt{passOnly(0)} }), cand))
		if r.ExcludedPairs != 0 || r.ImputedAttempts != 10 || r.ExcludedShare != 10.0/30.0 || r.Verdict != VerdictInconclusive {
			t.Fatalf("want INCONCLUSIVE at share 1/3: %s", marshal(t, r))
		}
	})
}

// A constant difference exactly on a margin that is not a sum of floats is still exactly on it:
// 0.1 - 0.2 is -0.1, the margin. The delta is that single value, taken from the data, and the interval is the
// rule of three about it, which spans the margin: the verdict is INCONCLUSIVE. A mean computed as a sum reads
// -0.10000000000000002 (sixteen copies of -0.1 do not sum to -1.6).
func TestGateConstantDifferenceExactlyOnTheMarginIsInconclusive(t *testing.T) {
	const n = 16
	p := niUnitPre(n)
	p.Primary.Margin = f64(0.1)
	b, h := freeze(t, p, nil)
	r := mustGate(t, b, h, inputFor(h, arm("i", repeatF(0.2, n), repeatF(1000, n)), arm("c", repeatF(0.1, n), repeatF(800, n))))
	if r.Delta != -0.1 || !(r.Lo < -0.1) || !(r.Hi > -0.1) {
		t.Fatalf("every difference is exactly -0.1: want the one value inside its interval, got %v %v %v", r.Delta, r.Lo, r.Hi)
	}
	if r.Verdict != VerdictInconclusive {
		t.Fatalf("a difference exactly on the margin is neither ADOPT nor BLOCK: %s %s", r.Verdict, r.Reason)
	}
}

// The held-out slice is scored the same way: a candidate cannot skip the held-out tasks it fails,
// and when no held-out pair exists an ADOPT is held back as missing evidence, never BLOCK.
func TestHeldOutIsScoredLikeTheMainSliceAndMissingEvidenceIsInconclusive(t *testing.T) {
	const declared = 18
	p := supPre(declared)
	p.HeldOut = []string{"t016", "t017"}
	b, h := freeze(t, p, nil)
	incScores := append(repeatF(0, 16), 1, 1)
	inc := arm("i", incScores, nil)

	// Held-out candidate attempts that timed out score 0 against the incumbent's 1: the check fails.
	cand := arm("c", repeatF(1, declared), nil)
	cand.Tasks[16].Attempts = []Attempt{noScore(OutcomeTimeout)}
	cand.Tasks[17].Attempts = []Attempt{noScore(OutcomeInfraError)}
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	if r.Verdict != VerdictBlock || r.HeldOut == nil || r.HeldOut.N != 2 || r.HeldOut.Delta != -1 || !r.HeldOut.Failed || r.HeldOut.Missing {
		t.Fatalf("a candidate that times out on the held-out tasks fails them: %s %+v", r.Verdict, r.HeldOut)
	}

	// The incumbent is unusable on both held-out tasks: no held-out pair, so the ADOPT is held back.
	incGone := withInfraErrors(inc, 16, 17)
	good := arm("c", repeatF(1, declared), nil)
	r = mustGate(t, b, h, inputFor(h, incGone, good))
	if r.Verdict != VerdictInconclusive || r.Reason != "held-out evidence missing" || !r.KeepIncumbent {
		t.Fatalf("no held-out pair must turn an ADOPT into INCONCLUSIVE, got %s %s", r.Verdict, r.Reason)
	}
	if r.HeldOut == nil || r.HeldOut.N != 0 || r.HeldOut.Failed || !r.HeldOut.Missing {
		t.Fatalf("missing held-out evidence is missing, not failed: %+v", r.HeldOut)
	}
	if j := marshal(t, r); !strings.Contains(j, `"missing":true`) {
		t.Fatalf("held_out.missing must be in the result: %s", j)
	}
	// With the main slice not ADOPT the verdict stays what it was; and the field is false when present.
	tie := arm("c", incScores, nil)
	tie.Tasks[16].Attempts[0].Score, tie.Tasks[17].Attempts[0].Score = f64(0), f64(0)
	r = mustGate(t, b, h, inputFor(h, incGone, tie))
	if r.Verdict != VerdictInconclusive || r.Reason == "held-out evidence missing" || r.HeldOut == nil || !r.HeldOut.Missing || r.HeldOut.Failed {
		t.Fatalf("a main verdict that was not ADOPT is untouched by missing held-out evidence: %s %s %+v", r.Verdict, r.Reason, r.HeldOut)
	}
}

// Non-finite results from huge scores are refused, not reported as a verdict about +Inf.
func TestScoresTooLargeToAverageAreRefused(t *testing.T) {
	const n = 16
	b, h := freeze(t, customPre(ModeSuperiority, n, 0, 1.7e308), nil)
	inc := armFrom("i", n, func(int) []Attempt { return []Attempt{passOnly(0)} })
	cand := armFrom("c", n, func(int) []Attempt { return []Attempt{passOnly(1.7e308)} })
	r, err := Gate(b, h, inputFor(h, inc, cand))
	if err == nil {
		t.Fatalf("scores whose bootstrap sums overflow must be refused, got %s delta %v lo %v", r.Verdict, r.Delta, r.Lo)
	}
	if rf := refusalOf(t, err); !strings.Contains(rf.Reason, "overflow") {
		t.Fatalf("the refusal must say the scores overflow: %q", rf.Reason)
	}
}

// ---- floors ----------------------------------------------------------------------------------

func TestFreezeRefusesBootstrapAndMinPairsBelowTheFloors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(p *PreReg)
		want   string
	}{
		{"bootstrap_b 1", func(p *PreReg) { p.BootstrapB = 1 }, "bootstrap_b"},
		{"bootstrap_b 10", func(p *PreReg) { p.BootstrapB = 10 }, "bootstrap_b"},
		{"bootstrap_b 1999", func(p *PreReg) { p.BootstrapB = 1999 }, "bootstrap_b"},
		{"min_pairs 2", func(p *PreReg) { p.MinPairs = 2 }, "min_pairs"},
		{"min_pairs 8", func(p *PreReg) { p.MinPairs = 8 }, "min_pairs"},
		{"min_pairs 15", func(p *PreReg) { p.MinPairs = 15 }, "min_pairs"},
		{"min_pairs above the non-held-out tasks", func(p *PreReg) { p.HeldOut = []string{"t000", "t001"}; p.MinPairs = 19 }, "min_pairs"},
		{"the default min_pairs above the non-held-out tasks", func(p *PreReg) { p.HeldOut = []string{"t000", "t001", "t002", "t003", "t004"} }, "min_pairs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := supPre(20)
			tc.mutate(&p)
			_, raw, hash, err := FreezeBaseline(p, testCorpus, nil)
			if err == nil {
				t.Fatalf("want a refusal naming %s, froze %d bytes", tc.want, len(raw))
			}
			if r := refusalOf(t, err); !strings.Contains(r.Reason, tc.want) {
				t.Fatalf("refusal %q does not name %s", r.Reason, tc.want)
			}
			if raw != nil || hash != "" {
				t.Fatal("a refused freeze returns no bytes and no hash")
			}
		})
	}
	// The floors themselves are allowed, and so is a min_pairs equal to the tasks left.
	p := supPre(20)
	p.BootstrapB, p.MinPairs = 2000, 16
	if b, _, _, err := FreezeBaseline(p, testCorpus, nil); err != nil || b.BootstrapB != 2000 || b.MinPairs != 16 {
		t.Fatalf("the floors are allowed: %+v %v", b, err)
	}
	p = supPre(20)
	p.MinPairs = 20
	if _, _, _, err := FreezeBaseline(p, testCorpus, nil); err != nil {
		t.Fatalf("min_pairs equal to the non-held-out count is allowed: %v", err)
	}
	// n = 2 with both pairs at +0.01 used to ADOPT: it cannot even be pre-registered now.
	p = supPre(2)
	p.MinPairs = 2
	if _, _, _, err := FreezeBaseline(p, testCorpus, nil); err == nil {
		t.Fatal("a two-task pre-registration must be refused")
	}
}

// check re-validates what freeze validates, so a hand-built canonical file below a floor is refused.
func TestCheckBaselineRefusesAFileBelowTheFloors(t *testing.T) {
	b, _ := freeze(t, supPre(16), nil)
	for name, mut := range map[string]func(b *Baseline){
		"bootstrap_b 1999":              func(b *Baseline) { b.BootstrapB = 1999 },
		"bootstrap_b 1":                 func(b *Baseline) { b.BootstrapB = 1 },
		"min_pairs 15":                  func(b *Baseline) { b.MinPairs = 15 },
		"min_pairs above the task list": func(b *Baseline) { b.MinPairs = 17 },
	} {
		c := b
		mut(&c)
		raw, err := canonicalBaseline(c)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := CheckBaseline(raw); err == nil {
			t.Errorf("%s: check must refuse the file", name)
		}
	}
}

// adoptRate runs the gate end to end on trials simulated comparisons of the null that gen draws and
// returns the share the gate ADOPTs. Trial s draws its scores from seed 1000+s, so the result does not
// depend on the number of workers. A superiority null has no efficiency metric; a non-inferiority null has
// one that clearly improves (1000 tokens against 100 on every task), so only the primary decides.
func adoptRate(t *testing.T, mode string, n, trials int, gen pairGen) float64 {
	t.Helper()
	var p PreReg
	if mode == ModeSuperiority {
		p = supPre(n)
	} else {
		p = niUnitPre(n)
	}
	p.BootstrapB, p.MinPairs = MinBootstrapB, MinMinPairs
	b, h := freeze(t, p, nil)
	workers := runtime.GOMAXPROCS(0)
	next := make(chan int, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	adopt := 0
	var firstErr error
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := 0
			for s := range next {
				incS, candS := gen(rand.New(rand.NewSource(int64(1000+s))), n)
				inc, cand := Arm{Stamps: testStamps("i")}, Arm{Stamps: testStamps("c")}
				for i, id := range taskIDs(n) {
					ia, ca := Attempt{Outcome: OutcomePass, Score: f64(incS[i])}, Attempt{Outcome: OutcomePass, Score: f64(candS[i])}
					if mode == ModeNonInferiority {
						ia.Metrics, ca.Metrics = tokenMetric(1000), tokenMetric(100)
					}
					inc.Tasks = append(inc.Tasks, Task{ID: id, Attempts: []Attempt{ia}})
					cand.Tasks = append(cand.Tasks, Task{ID: id, Attempts: []Attempt{ca}})
				}
				r, err := Gate(b, h, inputFor(h, inc, cand))
				if err != nil {
					mu.Lock()
					firstErr = err
					mu.Unlock()
					continue
				}
				if r.Verdict == VerdictAdopt {
					local++
				}
			}
			mu.Lock()
			adopt += local
			mu.Unlock()
		}()
	}
	for s := 0; s < trials; s++ {
		next <- s
	}
	close(next)
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("Gate refused a simulated comparison: %v", firstErr)
	}
	return float64(adopt) / float64(trials)
}

// The false-ADOPT rate of the gate on null data, measured end to end through Gate at the floors (16 tasks or
// more, B = 2000), in the cells below and no others: the true difference is 0 (superiority) or exactly
// -margin (non-inferiority, the worst case the rule must still refuse). Each cell must measure at most 4%
// (a nominal 2.5% one-sided) over its stated trials, with fixed seeds. The percentile interval the gate used
// to read measured 6.1% on the first non-inferiority cell, 10.0% on the second and 6.2% on the heavy-tailed
// superiority cell, over 20000 datasets each; TestCalibrationStudy prints the whole grid.
//
// The strong-incumbent and nested-loss cells are where most samples have no pair that differs, and a sample
// with no spread used to be read as its one value: pass rates .95 and .90 measured 9% at 16 tasks, .99 and .94
// 32%, and a candidate that only loses 5% of the incumbent's passes 37% at 20 tasks. They are tested at 16 to
// 24 tasks. They are not claimed at 60 tasks and more, where the score form is still above 4% in a few of them
// (the ADR has the figures).
func TestNullAdoptRateAtTheFloorsIsCalibrated(t *testing.T) {
	if testing.Short() {
		t.Skip("simulated gate runs; the cells below take a few seconds")
	}
	for _, tc := range []struct {
		name   string
		mode   string
		n      int
		trials int
		gen    pairGen
	}{
		{"superiority, pass/fail p=.5", ModeSuperiority, 16, 10000, genBinary(.5, .5, 1)},
		{"superiority, x^3-skewed scores", ModeSuperiority, 16, 10000, genCubed(1)},
		{"superiority, 90% of scores in [0, .1] and 10% in [.9, 1]", ModeSuperiority, 16, 10000, genRareLarge},
		{"non-inferiority at exactly -margin, pass rates .90 and .85", ModeNonInferiority, 20, 4000, genBinary(.90, .85, 1)},
		{"non-inferiority at exactly -margin, pass rates .90 and .85, the smallest sample", ModeNonInferiority, 16, 4000, genBinary(.90, .85, 1)},
		{"non-inferiority at exactly -margin, uniform scores", ModeNonInferiority, 24, 4000, genUniform(0, 1, 0, .9)},
		// A strong incumbent: most pairs tie, and a sample whose pairs all tie has no spread to
		// measure. The share of such samples is 0.86^16 = 8.9% here, 0.93^16 = 31% at .99/.94.
		{"non-inferiority at exactly -margin, pass rates .95 and .90, the smallest sample", ModeNonInferiority, 16, 4000, genBinary(.95, .90, 1)},
		{"non-inferiority at exactly -margin, pass rates .95 and .90", ModeNonInferiority, 24, 4000, genBinary(.95, .90, 1)},
		{"non-inferiority at exactly -margin, pass rates .99 and .94, the smallest sample", ModeNonInferiority, 16, 4000, genBinary(.99, .94, 1)},
		{"non-inferiority at exactly -margin, a candidate that only loses 5% of the incumbent's passes", ModeNonInferiority, 20, 4000, genNested(.9, .05/.9)},
		{"non-inferiority at exactly -margin, a candidate that only loses 5% of the incumbent's passes, 24 tasks", ModeNonInferiority, 24, 4000, genNested(.9, .05/.9)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rate := adoptRate(t, tc.mode, tc.n, tc.trials, tc.gen)
			t.Logf("false ADOPT rate, n=%d, B=2000, %d trials: %.4f", tc.n, tc.trials, rate)
			if rate > 0.04 {
				t.Fatalf("false ADOPT rate %.4f over %d trials is above 0.04", rate, tc.trials)
			}
		})
	}
	// And the settings that cannot be calibrated cannot be frozen.
	for _, bad := range []func(p *PreReg){
		func(p *PreReg) { p.BootstrapB = 1 },
		func(p *PreReg) { p.BootstrapB = 10 },
		func(p *PreReg) { p.MinPairs = 8 },
	} {
		q := supPre(16)
		bad(&q)
		if _, _, _, err := FreezeBaseline(q, testCorpus, nil); err == nil {
			t.Errorf("%+v must be refused", q)
		}
	}
}

// ---- a sample with no spread cannot certify anything -----------------------------------------

// all-tied builds a non-inferiority comparison on n tasks in which every pair is equal (score v on both
// arms) and the candidate spends half the tokens.
func allTied(t *testing.T, n int, pre PreReg, inc, cand float64) GateResult {
	t.Helper()
	b, h := freeze(t, pre, nil)
	return mustGate(t, b, h, inputFor(h,
		arm("i", repeatF(inc, n), repeatF(1000, n)),
		arm("c", repeatF(cand, n), repeatF(500, n))))
}

// Sixteen ties say little about a margin of 0.05: a candidate that lost 5% of the incumbent's passes would
// tie all sixteen pairs 44% of the time. A sample with no spread has no standard error to read, so its
// interval is the rule of three: the one value plus and minus 3 R / n, R the width of the score range (for pass/fail
// scores with no discordant pair among n, 3 / n is the one-sided 95% upper bound on the rate of discordant pairs, so
// the mean difference can be as low as -3 / n). A margin of 0.05 of the range is certified from 61 ties; 60 ties
// put the end exactly on it, and a difference exactly on the margin is not an ADOPT.
func TestAllTiedNonInferiorityDoesNotCertifyTheMarginOnASmallSample(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{16, VerdictInconclusive}, {24, VerdictInconclusive}, {40, VerdictInconclusive}, {60, VerdictInconclusive}, {61, VerdictAdopt}, {64, VerdictAdopt}, {80, VerdictAdopt}} {
		r := allTied(t, tc.n, niUnitPre(tc.n), 0.5, 0.5)
		if r.Delta != 0 || r.Verdict != tc.want {
			t.Errorf("n=%d, every pair tied, tokens halved: want %s, got %s %s (delta %v lo %v hi %v)", tc.n, tc.want, r.Verdict, r.Reason, r.Delta, r.Lo, r.Hi)
		}
		if half := 3 / float64(tc.n); math.Abs(r.Lo+half) > 1e-12 || math.Abs(r.Hi-half) > 1e-12 {
			t.Errorf("n=%d: the interval of a tie is [%v, %v], got [%v, %v]", tc.n, -half, half, r.Lo, r.Hi)
		}
	}
}

// The same on a 0..10 judge scale, where the margin is 0.5 and the interval is ten times as wide: a mutant that
// forgot the width of the range would certify the margin from 16 ties.
func TestAllTiedNonInferiorityScalesWithTheScoreRange(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{16, VerdictInconclusive}, {40, VerdictInconclusive}, {60, VerdictInconclusive}, {61, VerdictAdopt}, {80, VerdictAdopt}} {
		r := allTied(t, tc.n, niPre(tc.n), 8, 8)
		if r.Margin != 0.5 || r.Verdict != tc.want {
			t.Errorf("n=%d on 0..10 with margin %v: want %s, got %s %s (lo %v)", tc.n, r.Margin, tc.want, r.Verdict, r.Reason, r.Lo)
		}
		if half := 30 / float64(tc.n); math.Abs(r.Lo+half) > 1e-12 || math.Abs(r.Hi-half) > 1e-12 {
			t.Errorf("n=%d: the interval of a tie is [%v, %v], got [%v, %v]", tc.n, -half, half, r.Lo, r.Hi)
		}
	}
}

// A superiority tie is INCONCLUSIVE because its interval spans 0, and it stays so at any size: the incumbent wins
// ties. A constant difference far from zero still decides, in both directions and in both modes: every pair a win
// of the whole range is an ADOPT, every pair a loss of it is a BLOCK, and the interval stops at the range.
func TestAllTiedSuperiorityIsInconclusiveAndAConstantFarFromZeroStillDecides(t *testing.T) {
	const n = 16
	b, h := freeze(t, supPre(n), nil)
	for _, size := range []int{16, 80, 200} {
		bs, hs := freeze(t, supPre(size), nil)
		r := mustGate(t, bs, hs, inputFor(hs, arm("i", repeatF(0.5, size), nil), arm("c", repeatF(0.5, size), nil)))
		if half := 3 / float64(size); r.Verdict != VerdictInconclusive || r.Delta != 0 || math.Abs(r.Lo+half) > 1e-12 || math.Abs(r.Hi-half) > 1e-12 || !r.KeepIncumbent {
			t.Errorf("n=%d: a tie keeps the incumbent with an interval about 0: %s delta %v [%v, %v] keep %v", size, r.Verdict, r.Delta, r.Lo, r.Hi, r.KeepIncumbent)
		}
	}
	r := mustGate(t, b, h, inputFor(h, arm("i", repeatF(0, n), nil), arm("c", repeatF(1, n), nil)))
	if r.Verdict != VerdictAdopt || r.Delta != 1 || math.Abs(r.Lo-(1-3.0/n)) > 1e-12 || r.Hi != 1 {
		t.Errorf("every pair +1: want ADOPT on [0.8125, 1], got %s [%v, %v]", r.Verdict, r.Lo, r.Hi)
	}
	r = mustGate(t, b, h, inputFor(h, arm("i", repeatF(1, n), nil), arm("c", repeatF(0, n), nil)))
	if r.Verdict != VerdictBlock || r.Delta != -1 || r.Lo != -1 || math.Abs(r.Hi-(-1+3.0/n)) > 1e-12 {
		t.Errorf("every pair -1: want BLOCK on [-1, -0.8125], got %s [%v, %v]", r.Verdict, r.Lo, r.Hi)
	}
	// Non-inferiority: every pair a loss of the whole range is a BLOCK, and a win of it with tokens halved an ADOPT.
	bn, hn := freeze(t, niUnitPre(n), nil)
	r = mustGate(t, bn, hn, inputFor(hn, arm("i", repeatF(1, n), repeatF(1000, n)), arm("c", repeatF(0, n), repeatF(500, n))))
	if r.Verdict != VerdictBlock {
		t.Errorf("non-inferiority, every pair -1: want BLOCK, got %s %s", r.Verdict, r.Reason)
	}
	r = mustGate(t, bn, hn, inputFor(hn, arm("i", repeatF(0, n), repeatF(1000, n)), arm("c", repeatF(1, n), repeatF(500, n))))
	if r.Verdict != VerdictAdopt {
		t.Errorf("non-inferiority, every pair +1 and tokens halved: want ADOPT, got %s %s", r.Verdict, r.Reason)
	}
}

// A constant difference that is exactly the margin on paper is -0.04999999999999993 (0.95 - 0.90) or
// -0.05000000000000004 in floats; the single value used to decide it by the last bit. It is a constant loss of
// the margin, with no spread to read, and it never ADOPTs, at any size.
func TestConstantDifferenceAtTheMarginByFloatArithmeticIsNotAdopted(t *testing.T) {
	for _, n := range []int{16, 80, 200} {
		for _, tc := range []struct{ inc, cand float64 }{{0.95, 0.90}, {0.70, 0.65}, {0.50, 0.45}, {0.60, 0.55}} {
			r := allTied(t, n, niUnitPre(n), tc.inc, tc.cand)
			if r.Verdict != VerdictInconclusive {
				t.Errorf("n=%d, every pair %v -> %v is a loss of the margin: want INCONCLUSIVE, got %s %s (lo %v hi %v)", n, tc.inc, tc.cand, r.Verdict, r.Reason, r.Lo, r.Hi)
			}
		}
	}
}

// The reviewer's P3 variant: the unfinished attempt records no tokens. A candidate that leaves the cost of
// its timeouts out would keep the efficiency win of its other 24 attempts, so a task with an unfinished
// attempt of unknown cost is unresolved on that metric (it cannot be measured), and over the cap it stops
// the ADOPT.
func TestAnUnfinishedAttemptWithNoRecordedCostLeavesTheTaskUnresolved(t *testing.T) {
	const n, k = 20, 25
	b, h := freeze(t, niUnitPre(n), nil)
	inc := armFrom("i", n, func(int) []Attempt {
		as := make([]Attempt, k)
		for j := range as {
			as[j] = passTok(1, 1000)
		}
		return as
	})
	for _, o := range []AttemptOutcome{OutcomeTimeout, OutcomeInfraError, OutcomeJudgeError} {
		cand := armFrom("c", n, func(int) []Attempt {
			as := make([]Attempt, 0, k)
			for j := 0; j < k-1; j++ {
				as = append(as, passTok(1, 900))
			}
			return append(as, Attempt{Outcome: o, Metrics: map[string]*float64{tokensName: nil}})
		})
		r := mustGate(t, b, h, inputFor(h, inc, cand))
		e := r.Efficiency[0]
		if e.N != 0 || e.UnresolvedPairs != n || !e.OverUnresolvedCap || e.Improves || r.Verdict == VerdictAdopt {
			t.Errorf("%s with no recorded cost: the candidate's spend on every task is unknown: %+v %s", o, e, r.Verdict)
		}
	}
	// The same attempt with its cost recorded is a measured task, as before.
	cand := armFrom("c", n, func(int) []Attempt {
		as := make([]Attempt, 0, k)
		for j := 0; j < k-1; j++ {
			as = append(as, passTok(1, 900))
		}
		return append(as, Attempt{Outcome: OutcomeTimeout, Metrics: tokenMetric(30000)})
	})
	if e := mustGate(t, b, h, inputFor(h, inc, cand)).Efficiency[0]; e.N != n || e.UnresolvedPairs != 0 {
		t.Errorf("a recorded cost resolves the task: %+v", e)
	}
}

// ---- the judge is pre-registered -------------------------------------------------------------

func TestPreRegistrationMustFreezeTheJudge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		judge Judge
		want  string
	}{
		{"no judge", Judge{}, "judge"},
		{"unknown kind", Judge{Kind: "ensemble", Identity: "x"}, "judge"},
		{"empty identity", Judge{Kind: "deterministic"}, "judge"},
		{"empty kind", Judge{Identity: "x"}, "judge"},
	} {
		p := supPre(16)
		p.Judge = tc.judge
		_, _, _, err := FreezeBaseline(p, testCorpus, nil)
		if err == nil {
			t.Errorf("%s: freeze must refuse", tc.name)
			continue
		}
		if r := refusalOf(t, err); !strings.Contains(r.Reason, tc.want) {
			t.Errorf("%s: refusal %q does not mention the judge", tc.name, r.Reason)
		}
	}
	for _, j := range []Judge{{Kind: "deterministic", Identity: "acceptance-dsl"}, {Kind: "model", Identity: "some-model"}} {
		p := supPre(16)
		p.Judge = j
		b, raw, _, err := FreezeBaseline(p, testCorpus, nil)
		if err != nil || b.Judge != j || !strings.Contains(string(raw), `"judge": {`) {
			t.Fatalf("the judge must be frozen into the baseline: %+v %v\n%s", b.Judge, err, raw)
		}
		if cb, _, err := CheckBaseline(raw); err != nil || cb.Judge != j {
			t.Fatalf("check must read the frozen judge back: %+v %v", cb.Judge, err)
		}
	}
	// A hand-built canonical baseline with no judge is refused by check.
	b, _ := freeze(t, supPre(16), nil)
	b.Judge = Judge{}
	raw, _ := canonicalBaseline(b)
	if _, _, err := CheckBaseline(raw); err == nil {
		t.Fatal("check must refuse a baseline that froze no judge")
	}
}

func TestGateVersionIsTwoAndAnOlderBaselineIsRefused(t *testing.T) {
	if GateVersion != "2" {
		t.Fatalf("the rules changed (imputation, floors, frozen judge, vetoes): GateVersion = %q, want 2", GateVersion)
	}
	b, _ := freeze(t, supPre(16), nil)
	b.GateVersion = "1"
	raw, _ := canonicalBaseline(b)
	if _, _, err := CheckBaseline(raw); err == nil || !strings.Contains(err.Error(), "gate_version") {
		t.Fatalf("a version 1 baseline must be refused, got %v", err)
	}
}

// The three judge attacks of the review: an arbitrary identity on a frozen incumbent, a model id
// declared deterministic to skip the receipt, and a receipt with empty hashes.
func TestJudgeSwapAttacksAreRefused(t *testing.T) {
	const n = 16
	inc := arm("i", repeatF(0.4, n), nil)
	cand := arm("c", repeatF(0.9, n), nil)
	sorted := sortedArm(inc)
	b, h := freeze(t, supPre(n), &sorted)
	in := GateInput{BaselineHash: h, Candidate: cand, Judge: Judge{Kind: "deterministic", Identity: "totally-different-scorer"}}
	if _, err := Gate(b, h, in); err == nil || !strings.Contains(err.Error(), "judge identity") {
		t.Fatalf("a judge other than the frozen one must be refused, got %v", err)
	}
	in.Judge = detJudge()
	if r, err := Gate(b, h, in); err != nil || r.Verdict != VerdictAdopt {
		t.Fatalf("the frozen judge is accepted: %v %v", err, r.Verdict)
	}

	p := supPre(n)
	p.Judge = Judge{Kind: "model", Identity: "gpt-judge-v1"}
	bm, hm := freeze(t, p, nil)
	in = GateInput{BaselineHash: hm, Incumbent: &inc, Candidate: cand, Judge: Judge{Kind: "deterministic", Identity: "gpt-judge-v1"}}
	if _, err := Gate(bm, hm, in); err == nil || !strings.Contains(err.Error(), "judge kind") {
		t.Fatalf("declaring a frozen model judge deterministic to skip its receipt must be refused, got %v", err)
	}
	in.Judge = p.Judge
	in.Receipt = &JudgeReceipt{ScorerID: "gpt-judge-v1", Passed: true}
	if _, err := Gate(bm, hm, in); err == nil || !strings.Contains(err.Error(), "pairs_hash") {
		t.Fatalf("a receipt with empty hashes must be refused, got %v", err)
	}
	// A hash must be exactly 64 characters of lower-case hex: a short one, a long one and a
	// non-hex one of the right length are each refused, in either field.
	bad := map[string]string{
		"63 hex characters":         hex64("x")[:63],
		"65 hex characters":         hex64("x") + "0",
		"64 characters, non-hex":    strings.Repeat("g", 64),
		"64 characters, upper-case": strings.ToUpper(hex64("x")),
	}
	for name, h := range bad {
		for field, rc := range map[string]JudgeReceipt{
			"pairs_hash":   {ScorerID: "gpt-judge-v1", PairsHash: h, BuildSHA: hex64("build"), Passed: true},
			"build_sha256": {ScorerID: "gpt-judge-v1", PairsHash: hex64("pairs"), BuildSHA: h, Passed: true},
		} {
			rc := rc
			in.Receipt = &rc
			if _, err := Gate(bm, hm, in); err == nil || !strings.Contains(err.Error(), field) {
				t.Errorf("%s in %s must be refused naming the field, got %v", name, field, err)
			}
		}
	}
	in.Receipt = &JudgeReceipt{ScorerID: "gpt-judge-v1", PairsHash: hex64("pairs"), BuildSHA: hex64("build"), Passed: true}
	if _, err := Gate(bm, hm, in); err != nil {
		t.Fatalf("a complete receipt for the frozen model judge is accepted: %v", err)
	}
}

// The embedded schema describes the receipt the gate now demands.
func TestSchemaDescribesTheReceiptHashes(t *testing.T) {
	var doc struct {
		Defs struct {
			Receipt struct {
				Properties map[string]struct {
					Pattern   string `json:"pattern"`
					MinLength int    `json:"minLength"`
				} `json:"properties"`
			} `json:"receipt"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(GateInputSchema, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"pairs_hash", "build_sha256"} {
		if got := doc.Defs.Receipt.Properties[k].Pattern; got != "^[0-9a-f]{64}$" {
			t.Errorf("receipt.%s pattern = %q, want the 64 lower-case hex pattern", k, got)
		}
	}
	if doc.Defs.Receipt.Properties["scorer_id"].MinLength < 1 {
		t.Error("receipt.scorer_id must be non-empty in the schema")
	}
}

// ---- efficiency ------------------------------------------------------------------------------

func TestEfficiencyValuesMustBeFiniteAndNotNegative(t *testing.T) {
	const n = 16
	b, h := freeze(t, niPre(n), nil)
	for _, side := range []string{"candidate", "incumbent"} {
		for _, bad := range []float64{-1e6, -1, math.Copysign(0, -1) - 1e-300, math.Inf(1), math.NaN()} {
			inc := arm("i", repeatF(8, n), repeatF(1000, n))
			cand := arm("c", repeatF(8, n), repeatF(800, n))
			if side == "candidate" {
				cand.Tasks[5].Attempts[0].Metrics[tokensName] = f64(bad)
			} else {
				inc.Tasks[5].Attempts[0].Metrics[tokensName] = f64(bad)
			}
			if r, err := Gate(b, h, inputFor(h, inc, cand)); err == nil {
				t.Errorf("%s metric %v must be refused, got %s", side, bad, r.Verdict)
			}
		}
	}
	inc := arm("i", repeatF(8, n), repeatF(1000, n))
	cand := arm("c", repeatF(8, n), repeatF(0, n))
	if _, err := Gate(b, h, inputFor(h, inc, cand)); err != nil {
		t.Fatalf("a measured zero cost is allowed: %v", err)
	}
	// A negative cost in a frozen incumbent cannot be frozen.
	bad := arm("i", repeatF(8, n), repeatF(1000, n))
	bad.Tasks[0].Attempts[0].Metrics[tokensName] = f64(-5)
	if _, _, _, err := FreezeBaseline(niPre(n), testCorpus, &bad); err == nil {
		t.Fatal("freeze must refuse an incumbent with a negative metric")
	}
}

// A candidate 5x costlier on 24 of 40 tasks, with those costs null, used to ADOPT on the 16 it
// reported. A metric whose unresolved share is over the cap cannot improve, and in
// non-inferiority it blocks the ADOPT.
func TestUnresolvedMetricShareAboveTheCapBlocksAdoptInNonInferiority(t *testing.T) {
	const n = 40
	b, h := freeze(t, niUnitPre(n), nil)
	inc := armFrom("i", n, func(i int) []Attempt { return []Attempt{passTok(incScore(i), 1000)} })
	cand := armFrom("c", n, func(i int) []Attempt {
		if i < 24 {
			return []Attempt{{Outcome: OutcomePass, Score: f64(1), Metrics: map[string]*float64{tokensName: nil}}}
		}
		return []Attempt{passTok(1, 100)}
	})
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	e := r.Efficiency[0]
	if e.N != 16 || e.UnresolvedPairs != 24 || !e.OverUnresolvedCap || e.Improves {
		t.Fatalf("24 of 40 pairs unresolved is over the 0.20 cap and cannot improve: %+v", e)
	}
	if r.Verdict != VerdictInconclusive || !strings.Contains(r.Reason, tokensName) || !strings.Contains(r.Reason, "unresolved") {
		t.Fatalf("the verdict must be INCONCLUSIVE and name the unmeasured metric: %s %s", r.Verdict, r.Reason)
	}
	// Exactly at the cap (8 of 40) is allowed.
	cand2 := armFrom("c", n, func(i int) []Attempt {
		if i < 8 {
			return []Attempt{{Outcome: OutcomePass, Score: f64(1), Metrics: map[string]*float64{tokensName: nil}}}
		}
		return []Attempt{passTok(1, 100)}
	})
	r2 := mustGate(t, b, h, inputFor(h, inc, cand2))
	if e := r2.Efficiency[0]; e.OverUnresolvedCap || !e.Improves || r2.Verdict != VerdictAdopt {
		t.Fatalf("an unresolved share equal to the cap is not above it: %+v %s %s", e, r2.Verdict, r2.Reason)
	}
}

// A candidate that times out on its expensive tasks cannot leave their cost out: a timeout ran, and what it
// recorded is spent. Its tasks are pairs with the cost the timeouts burned, not unresolved pairs.
func TestTimedOutTasksCarryTheCostTheyRecorded(t *testing.T) {
	const n = 20
	b, h := freeze(t, niPre(n), nil)
	inc := armFrom("i", n, func(int) []Attempt {
		return []Attempt{{Outcome: OutcomePass, Score: f64(8), Metrics: tokenMetric(1000)}}
	})
	cand := armFrom("c", n, func(i int) []Attempt {
		if i < 5 {
			return []Attempt{{Outcome: OutcomeTimeout, Metrics: tokenMetric(5000)}}
		}
		return []Attempt{{Outcome: OutcomePass, Score: f64(8), Metrics: tokenMetric(500)}}
	})
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	e := r.Efficiency[0]
	// 5 tasks cost 5000 and 15 cost 500, against 1000 on every task: the mean difference is
	// (5 x 4000 - 15 x 500) / 20 = +625 (the reported delta is the bootstrap's, within a few tens of it).
	if e.N != n || e.UnresolvedPairs != 0 || e.OverUnresolvedCap || e.Improves || math.Abs(val(e.Delta)-625) > 50 {
		t.Fatalf("the timeouts' recorded cost is part of the candidate's spend: %+v", e)
	}
	if r.Verdict == VerdictAdopt {
		t.Fatalf("a candidate that times out on a quarter of the tasks was adopted: %s", marshal(t, r))
	}
}

// A cost the adapter did not record is a null and stays unresolved, an unfinished attempt's included:
// the gate cannot charge for what it was never told. The pairs are counted, and over the cap they stop
// the ADOPT.
func TestTimedOutTasksWithNullCostStayUnresolved(t *testing.T) {
	const n = 20
	b, h := freeze(t, niPre(n), nil)
	inc := armFrom("i", n, func(int) []Attempt {
		return []Attempt{{Outcome: OutcomePass, Score: f64(8), Metrics: tokenMetric(1000)}}
	})
	cand := armFrom("c", n, func(i int) []Attempt {
		if i < 5 {
			return []Attempt{{Outcome: OutcomeTimeout, Metrics: map[string]*float64{tokensName: nil}}}
		}
		return []Attempt{{Outcome: OutcomePass, Score: f64(8), Metrics: tokenMetric(500)}}
	})
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	e := r.Efficiency[0]
	if e.N != 15 || e.UnresolvedPairs != 5 || !e.OverUnresolvedCap || r.Verdict == VerdictAdopt {
		t.Fatalf("a null cost on a timed-out task is unresolved, never 0: %+v %s", e, r.Verdict)
	}
}

// Probe P3, the cost lever. 20 tasks of 25 attempts: the incumbent passes all 25 at 1000 tokens, the
// candidate passes 24 at 900 and its 25th attempt times out after burning 20000. Its true spend per task is
// (24 x 900 + 20000) / 25 = 1664 tokens, 1.66 times the incumbent's. The timeout scores the worst (0.96
// against 1.0, inside the 0.05 margin), and it used to carry no cost, so the gate saw 900 tokens and
// ADOPTed an efficiency win that was a loss.
func TestAnUnfinishedAttemptsRecordedCostIsSpentNotHidden(t *testing.T) {
	const n, k = 20, 25
	b, h := freeze(t, niUnitPre(n), nil)
	inc := armFrom("i", n, func(i int) []Attempt {
		as := make([]Attempt, k)
		for j := range as {
			as[j] = passTok(incScore(i), 1000)
		}
		return as
	})
	cand := armFrom("c", n, func(int) []Attempt {
		as := make([]Attempt, 0, k)
		for j := 0; j < k-1; j++ {
			as = append(as, passTok(1, 900))
		}
		return append(as, Attempt{Outcome: OutcomeTimeout, Metrics: tokenMetric(20000)})
	})
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	if !(r.Lo > -0.05) || r.ImputedAttempts != n || math.Abs(r.ExcludedShare-20.0/500.0) > 1e-12 {
		t.Fatalf("setup: the primary must be inside the margin with 20 imputed attempts of 500, so that only the cost decides: %s", marshal(t, r))
	}
	e := r.Efficiency[0]
	if e.N != n || val(e.Delta) != 664 || val(e.Lo) != 664 || val(e.Hi) != 664 {
		t.Fatalf("the candidate spends 1664 tokens a task against 1000: want a delta of +664, got %+v", e)
	}
	if e.Improves || !e.Regresses {
		t.Fatalf("a candidate 1.66 times dearer regresses and does not improve: %+v", e)
	}
	if r.Verdict != VerdictInconclusive || r.Reason != "declared efficiency metric "+tokensName+" got significantly worse" {
		t.Fatalf("the efficiency win was a loss and must not ADOPT: %s %s", r.Verdict, r.Reason)
	}
}

func TestSuperiorityReportsEfficiencyAndIgnoresItForTheVerdict(t *testing.T) {
	const n = 16
	p := supPre(n)
	p.Efficiency = []PreRegEfficiency{{Name: tokensName, Class: EffTokens}}
	b, h := freeze(t, p, nil)
	inc := armFrom("i", n, func(int) []Attempt { return []Attempt{passTok(0, 100)} })
	cand := armFrom("c", n, func(int) []Attempt { return []Attempt{passTok(1, 1000)} })
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	if r.Verdict != VerdictAdopt {
		t.Fatalf("superiority adopts on the primary alone: %s %s", r.Verdict, r.Reason)
	}
	if e := r.Efficiency[0]; !e.Regresses || e.Improves {
		t.Fatalf("a ten-times dearer candidate is reported as regressing: %+v", e)
	}
}

// Memory 10x worse with tokens better used to ADOPT. A declared metric that got
// significantly worse vetoes a non-inferiority ADOPT.
func TestSignificantlyWorseEfficiencyMetricVetoesAdopt(t *testing.T) {
	const n = 16
	p := niPre(n)
	p.Efficiency = []PreRegEfficiency{{Name: tokensName, Class: EffTokens}, {Name: "memory_gib", Class: EffMemoryGiB}}
	b, h := freeze(t, p, nil)
	mk := func(tag string, tok func(i int) float64, mem func(i int) float64) Arm {
		return armFrom(tag, n, func(i int) []Attempt {
			return []Attempt{{Outcome: OutcomePass, Score: f64(armScore(tag, i)), Metrics: map[string]*float64{tokensName: f64(tok(i)), "memory_gib": f64(mem(i))}}}
		})
	}
	inc := mk("i", func(int) float64 { return 1000 }, func(int) float64 { return 2 })
	cand := mk("c", func(i int) float64 { return 900 - float64(i) }, func(i int) float64 { return 20 + float64(i) })
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	tok, mem := r.Efficiency[0], r.Efficiency[1]
	if !tok.Improves || tok.Regresses || mem.Improves || !mem.Regresses {
		t.Fatalf("tokens improve, memory regresses: %+v %+v", tok, mem)
	}
	if r.Verdict != VerdictInconclusive || r.Reason != "declared efficiency metric memory_gib got significantly worse" || !r.KeepIncumbent {
		t.Fatalf("the regressing metric must veto: %s %s", r.Verdict, r.Reason)
	}
	// With the memory back to equal nothing vetoes and tokens carry the ADOPT.
	cand2 := mk("c", func(i int) float64 { return 900 - float64(i) }, func(int) float64 { return 2 })
	r2 := mustGate(t, b, h, inputFor(h, inc, cand2))
	if r2.Verdict != VerdictAdopt || r2.Efficiency[1].Regresses {
		t.Fatalf("an unchanged second metric does not veto: %s %s %+v", r2.Verdict, r2.Reason, r2.Efficiency[1])
	}
}

// A regression is read on any metric that has an interval, that is on two or more resolved pairs: only an
// IMPROVEMENT needs min_pairs resolved pairs. (A metric resolved on 2 to min_pairs-1 pairs used to be neither,
// so a ten-times regression on it was neutral.)
func TestRegressionNeedsTwoResolvedPairsAndAnIntervalWhollyOnTheWorseSide(t *testing.T) {
	const n = 20
	p := niPre(n)
	p.Efficiency = []PreRegEfficiency{
		{Name: tokensName, Class: EffTokens},
		{Name: "tokens_saved", Class: EffTokens, LowerIsBetter: boolp(false)},
	}
	share := 0.95 // wide enough that the unresolved cap does not decide below
	p.MaxExcludedShare = &share
	b, h := freeze(t, p, nil)
	build := func(resolved int, dTok, dSaved func(i int) float64) GateResult {
		inc := armFrom("i", n, func(int) []Attempt {
			return []Attempt{{Outcome: OutcomePass, Score: f64(8), Metrics: map[string]*float64{tokensName: f64(1000), "tokens_saved": f64(100)}}}
		})
		cand := armFrom("c", n, func(i int) []Attempt {
			m := map[string]*float64{tokensName: nil, "tokens_saved": nil}
			if i < resolved {
				m[tokensName], m["tokens_saved"] = f64(1000+dTok(i)), f64(100+dSaved(i))
			}
			return []Attempt{{Outcome: OutcomePass, Score: f64(8), Metrics: m}}
		})
		return mustGate(t, b, h, inputFor(h, inc, cand))
	}
	dearer := func(int) float64 { return 100 }
	lower := func(int) float64 { return -50 }
	// 16 resolved pairs, both dearer / lower: both regress.
	r := build(16, dearer, lower)
	if !r.Efficiency[0].Regresses || !r.Efficiency[1].Regresses {
		t.Fatalf("both metrics are wholly on the worse side: %+v", r.Efficiency)
	}
	// 15 resolved pairs is below min_pairs: both still regress (a regression needs only an interval), and
	// nothing improves.
	r = build(15, dearer, lower)
	if !r.Efficiency[0].Regresses || !r.Efficiency[1].Regresses || r.Efficiency[0].Improves || r.Efficiency[1].Improves {
		t.Fatalf("15 resolved pairs have an interval: both regress, neither improves: %+v", r.Efficiency)
	}
	// Two resolved pairs are the least that has an interval.
	r = build(2, dearer, lower)
	if e := r.Efficiency[0]; e.N != 2 || e.Delta == nil || !e.Regresses || e.Improves {
		t.Fatalf("two resolved pairs regress: %+v", e)
	}
	// One resolved pair has no interval: delta, lo and hi are null and nothing regresses.
	r = build(1, dearer, lower)
	if e := r.Efficiency[0]; e.N != 1 || e.Delta != nil || e.Lo != nil || e.Hi != nil || e.Regresses || e.Improves {
		t.Fatalf("one resolved pair has no interval and cannot regress: %+v", e)
	}
	// An IMPROVEMENT still needs min_pairs resolved pairs: 15 cheaper pairs are not enough, 16 are.
	cheaper := func(int) float64 { return -100 }
	more := func(int) float64 { return 50 }
	if e := build(15, cheaper, more).Efficiency; e[0].Improves || e[1].Improves || e[0].Regresses || e[1].Regresses {
		t.Fatalf("15 resolved pairs are too few to improve and are not a regression: %+v", e)
	}
	if e := build(16, cheaper, more).Efficiency; !e[0].Improves || !e[1].Improves {
		t.Fatalf("16 resolved pairs reach min_pairs: %+v", e)
	}
	// An interval that spans zero is no regression.
	r = build(16, func(i int) float64 { return float64(300 * (1 - 2*(i%2))) }, func(i int) float64 { return float64(50 * (1 - 2*(i%2))) })
	if r.Efficiency[0].Regresses || r.Efficiency[1].Regresses {
		t.Fatalf("an interval spanning zero is not a regression: %+v", r.Efficiency)
	}
}

// Probe P7. 18 tasks: tokens are better on every pair, memory is ten times worse on 15 pairs and null on 3.
// 15 resolved pairs is below min_pairs (16) and the 3 unresolved are 0.167 of the 18, under the 0.20 cap, so
// memory neither improved nor counted as unmeasured, a regression on it went unread, and the tokens
// carried an ADOPT.
func TestAMetricResolvedOnFewerThanMinPairsStillVetoesWhenItIsMuchWorse(t *testing.T) {
	const n = 18
	p := niPre(n)
	p.Efficiency = []PreRegEfficiency{{Name: tokensName, Class: EffTokens}, {Name: "memory_gib", Class: EffMemoryGiB}}
	b, h := freeze(t, p, nil)
	mk := func(tag string, tok float64, mem func(i int) *float64) Arm {
		return armFrom(tag, n, func(i int) []Attempt {
			return []Attempt{{Outcome: OutcomePass, Score: f64(armScore(tag, i)), Metrics: map[string]*float64{tokensName: f64(tok), "memory_gib": mem(i)}}}
		})
	}
	inc := mk("i", 1000, func(int) *float64 { return f64(2) })
	cand := mk("c", 800, func(i int) *float64 {
		if i < 15 {
			return f64(20)
		}
		return nil
	})
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	tok, mem := r.Efficiency[0], r.Efficiency[1]
	if !tok.Improves || tok.Regresses {
		t.Fatalf("setup: tokens improve on all 18 pairs: %+v", tok)
	}
	if mem.N != 15 || mem.UnresolvedPairs != 3 || mem.OverUnresolvedCap || mem.Improves || !mem.Regresses {
		t.Fatalf("memory is resolved on 15 pairs, under the cap, and ten times worse: %+v", mem)
	}
	if r.Verdict == VerdictAdopt || r.Reason != "declared efficiency metric memory_gib got significantly worse" {
		t.Fatalf("a ten-times memory regression must veto the ADOPT whatever the tokens do: %s %s", r.Verdict, r.Reason)
	}
}

// With k declared metrics each one is read on the two-sided 1 - 0.05/k interval. One metric keeps
// the plain 95% interval of BootstrapDeltaMean.
func TestSeveralMetricsAreReadOnABonferroniInterval(t *testing.T) {
	const n = 16
	// The candidate is cheaper on a of the tasks and dearer on the rest, by 1 each; find an a for
	// which the plain interval clears zero and the 8-metric one does not.
	build := func(a int) (incV, candV []float64) {
		incV, candV = repeatF(10, n), make([]float64, n)
		for i := range candV {
			if i < a {
				candV[i] = 9
			} else {
				candV[i] = 11
			}
		}
		return
	}
	a := -1
	for try := 16; try >= 8 && a < 0; try-- {
		iv, cv := build(try)
		_, _, hi1 := refInterval(iv, cv, 10000, 1, 0.025)
		_, _, hi8 := refInterval(iv, cv, 10000, 1, 0.025/8)
		if hi1 < 0 && hi8 >= 0 {
			a = try
		}
	}
	if a < 0 {
		t.Fatal("setup: no dataset separates the plain and the Bonferroni reading")
	}
	incV, candV := build(a)
	mkArms := func(names []string) (Arm, Arm) {
		mk := func(tag string, vals []float64) Arm {
			return armFrom(tag, n, func(i int) []Attempt {
				m := map[string]*float64{}
				for _, nm := range names {
					m[nm] = f64(vals[i])
				}
				return []Attempt{{Outcome: OutcomePass, Score: f64(armScore(tag, i)), Metrics: m}}
			})
		}
		return mk("i", incV), mk("c", candV)
	}
	// One metric: the plain 95% interval, equal to BootstrapDeltaMean, and it improves.
	p1 := niPre(n)
	b1, h1 := freeze(t, p1, nil)
	i1, c1 := mkArms([]string{tokensName})
	r1 := mustGate(t, b1, h1, inputFor(h1, i1, c1))
	d, lo, hi := BootstrapDeltaMean(incV, candV, b1.BootstrapB, b1.Seed)
	e := r1.Efficiency[0]
	if e.Level != 0.95 || val(e.Delta) != d || val(e.Lo) != lo || val(e.Hi) != hi || !e.Improves || r1.Verdict != VerdictAdopt {
		t.Fatalf("one metric is read at 0.95 exactly as BootstrapDeltaMean: %+v (want %v %v %v) %s", e, d, lo, hi, r1.Verdict)
	}
	// Eight metrics with the same data: each is read at 1 - 0.05/8, where none improves.
	names := make([]string, 8)
	p8 := niPre(n)
	p8.Efficiency = nil
	for k := range names {
		names[k] = fmt.Sprintf("tokens_%d", k)
		p8.Efficiency = append(p8.Efficiency, PreRegEfficiency{Name: names[k], Class: EffTokens})
	}
	b8, h8 := freeze(t, p8, nil)
	i8, c8 := mkArms(names)
	r8 := mustGate(t, b8, h8, inputFor(h8, i8, c8))
	wd, wlo, whi := refInterval(incV, candV, b8.BootstrapB, b8.Seed, 0.025/8)
	for _, e := range r8.Efficiency {
		if math.Abs(e.Level-(1-0.05/8)) > 1e-12 || val(e.Delta) != wd || val(e.Lo) != wlo || val(e.Hi) != whi {
			t.Fatalf("eight metrics are read at 1 - 0.05/8: %+v (want %v %v %v)", e, wd, wlo, whi)
		}
		if e.Improves {
			t.Fatalf("%s improves on the plain interval only; at 1 - 0.05/8 it must not: %+v", e.Name, e)
		}
	}
	if !(wlo <= lo && whi >= hi) {
		t.Fatalf("the Bonferroni interval contains the plain one: [%v, %v] vs [%v, %v]", wlo, whi, lo, hi)
	}
	if r8.Verdict != VerdictInconclusive || !strings.Contains(r8.Reason, "no declared efficiency metric improved") {
		t.Fatalf("no metric improves at the corrected level: %s %s", r8.Verdict, r8.Reason)
	}
}

func TestResultCarriesTheNewFieldsInJSON(t *testing.T) {
	const n = 16
	b, h := freeze(t, niPre(n), nil)
	r := mustGate(t, b, h, inputFor(h, arm("i", repeatF(8, n), repeatF(1000, n)), arm("c", repeatF(8, n), repeatF(800, n))))
	j := marshal(t, r)
	for _, want := range []string{`"imputed_attempts":0`, `"unusable_attempts":{"incumbent":0,"candidate":0}`, `"over_unresolved_cap":false`, `"regresses":false`, `"level":0.95`} {
		if !strings.Contains(j, want) {
			t.Errorf("the result JSON lacks %s: %s", want, j)
		}
	}
}

// ---- the margin ------------------------------------------------------------------------------

func TestMarginAtOrAboveTheClassWidthIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		class  string
		rng    []float64
		margin float64
		ok     bool
	}{
		{"unit 2", ClassUnitInterval, nil, 2, false},
		{"unit 1", ClassUnitInterval, nil, 1, false},
		{"unit 0.99", ClassUnitInterval, nil, 0.99, true},
		{"judge 11", ClassJudge010, nil, 11, false},
		{"judge 10", ClassJudge010, nil, 10, false},
		{"judge 9.5", ClassJudge010, nil, 9.5, true},
		{"custom width 10", ClassCustom, []float64{2, 12}, 10, false},
		{"custom width 10, margin 9.99", ClassCustom, []float64{2, 12}, 9.99, true},
		{"custom negative range", ClassCustom, []float64{-5, 0}, 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := niPre(16)
			p.Primary = PreRegPrimary{Name: "q", Class: tc.class, Margin: f64(tc.margin), Range: tc.rng}
			_, _, _, err := FreezeBaseline(p, testCorpus, nil)
			switch {
			case tc.ok && err != nil:
				t.Fatalf("margin %v inside the width must be allowed: %v", tc.margin, err)
			case !tc.ok && err == nil:
				t.Fatalf("margin %v at or above the width must be refused", tc.margin)
			case !tc.ok && !strings.Contains(refusalOf(t, err).Reason, "margin"):
				t.Fatalf("the refusal must name the margin: %v", err)
			}
		})
	}
	// Superiority ignores the margin: any value is recorded as 0.
	p := supPre(16)
	p.Primary.Margin = f64(5)
	if b, _, _, err := FreezeBaseline(p, testCorpus, nil); err != nil || b.Primary.Margin != 0 {
		t.Fatalf("superiority records margin 0 whatever was written: %+v %v", b.Primary, err)
	}
	// A hand-built canonical baseline with margin 2 on a unit class is refused by check.
	b, _ := freeze(t, niUnitPre(16), nil)
	b.Primary.Margin = 2
	raw, _ := canonicalBaseline(b)
	if _, _, err := CheckBaseline(raw); err == nil {
		t.Fatal("check must refuse a margin above the class width")
	}
}

// A candidate that fails every task cannot ADOPT on cost: the widest margin that can be frozen is
// still inside the range, and a delta of the whole range is below it. Every pair loses the whole range,
// so there is no spread and the interval is the rule of three about -1: 16 tasks cannot put its upper end
// (-0.8125) below a margin of 0.99, and 400 can (3/400 = 0.0075).
func TestAFailingCandidateIsNeverAdoptedUnderTheWidestMargin(t *testing.T) {
	for _, tc := range []struct {
		n      int
		margin float64
		want   string
	}{{16, 0.99, VerdictInconclusive}, {16, 0.5, VerdictBlock}, {400, 0.99, VerdictBlock}} {
		p := niUnitPre(tc.n)
		p.Primary.Margin = f64(tc.margin)
		b, h := freeze(t, p, nil)
		inc := armFrom("i", tc.n, func(int) []Attempt { return []Attempt{passTok(1, 1000)} })
		cand := armFrom("c", tc.n, func(int) []Attempt {
			return []Attempt{{Outcome: OutcomeTaskFail, Score: f64(0), Metrics: tokenMetric(10)}}
		})
		r := mustGate(t, b, h, inputFor(h, inc, cand))
		if r.Verdict != tc.want || r.Lo != -1 || r.KeepIncumbent != true {
			t.Errorf("n=%d, margin %v: failing every task: want %s on lo=-1, got %s %s (lo %v hi %v)", tc.n, tc.margin, tc.want, r.Verdict, r.Reason, r.Lo, r.Hi)
		}
	}
}

// ---- the custom class ------------------------------------------------------------------------

func TestCustomClassNeedsAValidRange(t *testing.T) {
	for _, mode := range []string{ModeSuperiority, ModeNonInferiority} {
		for _, tc := range []struct {
			name string
			rng  []float64
		}{
			{"no range", nil},
			{"one number", []float64{1}},
			{"three numbers", []float64{0, 1, 2}},
			{"lo equals hi", []float64{3, 3}},
			{"hi below lo", []float64{5, 1}},
			{"NaN end", []float64{math.NaN(), 1}},
			{"infinite end", []float64{0, math.Inf(1)}},
			{"width overflows", []float64{-1e308, 1e308}},
		} {
			p := customPre(mode, 16, 0, 1)
			p.Primary.Range = tc.rng
			_, _, _, err := FreezeBaseline(p, testCorpus, nil)
			if err == nil {
				t.Errorf("%s/%s: freeze must refuse", mode, tc.name)
				continue
			}
			if r := refusalOf(t, err); !strings.Contains(r.Reason, "range") {
				t.Errorf("%s/%s: the refusal must name the range: %q", mode, tc.name, r.Reason)
			}
		}
	}
	// A range on a class that has its own is refused too.
	p := supPre(16)
	p.Primary.Range = []float64{0, 5}
	if _, _, _, err := FreezeBaseline(p, testCorpus, nil); err == nil {
		t.Fatal("a range on unit_interval must be refused")
	}
	// check refuses a hand-built custom baseline without a range.
	b, _, _, err := FreezeBaseline(customPre(ModeSuperiority, 16, 0, 1), testCorpus, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.Primary.Range = nil
	raw, _ := canonicalBaseline(b)
	if _, _, err := CheckBaseline(raw); err == nil {
		t.Fatal("check must refuse a custom baseline without a range")
	}
}

func TestCustomRangeIsFrozenAndBoundsTheScores(t *testing.T) {
	const n = 16
	b, raw, hash, err := FreezeBaseline(customPre(ModeSuperiority, n, -5, 20), testCorpus, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"range": [`) || len(b.Primary.Range) != 2 {
		t.Fatalf("the range is frozen into the canonical bytes:\n%s", raw)
	}
	if cb, ch, err := CheckBaseline(raw); err != nil || ch != hash || len(cb.Primary.Range) != 2 || cb.Primary.Range[0] != -5 || cb.Primary.Range[1] != 20 {
		t.Fatalf("check reads the range back: %+v %v", cb.Primary, err)
	}
	score := func(v float64) (Arm, Arm) {
		inc := armFrom("i", n, func(int) []Attempt { return []Attempt{passOnly(0)} })
		cand := armFrom("c", n, func(int) []Attempt { return []Attempt{passOnly(1)} })
		cand.Tasks[3].Attempts[0].Score = f64(v)
		return inc, cand
	}
	for _, v := range []float64{-5, 20, 0} { // the ends are inside the closed range
		inc, cand := score(v)
		if _, err := Gate(b, hash, inputFor(hash, inc, cand)); err != nil {
			t.Errorf("score %v is inside [-5, 20]: %v", v, err)
		}
	}
	for _, v := range []float64{-5.0001, 20.0001, 1e9} {
		inc, cand := score(v)
		if _, err := Gate(b, hash, inputFor(hash, inc, cand)); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("score %v is outside [-5, 20] and must be refused, got %v", v, err)
		}
		inc2, cand2 := score(0)
		inc2.Tasks[2].Attempts[0].Score = f64(v)
		if _, err := Gate(b, hash, inputFor(hash, inc2, cand2)); err == nil {
			t.Errorf("an incumbent score %v outside the range must be refused", v)
		}
	}
	// A frozen incumbent outside the range cannot be frozen.
	badInc := armFrom("i", n, func(int) []Attempt { return []Attempt{passOnly(30)} })
	if _, _, _, err := FreezeBaseline(customPre(ModeSuperiority, n, -5, 20), testCorpus, &badInc); err == nil {
		t.Fatal("a frozen incumbent outside the custom range must be refused")
	}
}
