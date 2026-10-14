package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
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
	if r.Delta != 0 || r.Lo != 0 || r.Hi != 0 {
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
	}{
		{"unit_interval", func() PreReg { p := supPre(n); return p }(), 1, -1},
		{"judge_0_10", func() PreReg { p := supPre(n); p.Primary.Class = ClassJudge010; return p }(), 8, -8},
		{"custom [-5, 20]", customPre(ModeSuperiority, n, -5, 20), 7, -12},
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
			if r.NUsed != n || r.Delta != tc.wantDelta || r.Lo != tc.wantDelta || r.Hi != tc.wantDelta {
				t.Fatalf("every candidate task scores the worst: want delta %v, got n=%d %v [%v, %v]", tc.wantDelta, r.NUsed, r.Delta, r.Lo, r.Hi)
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
	d, lo, hi := BootstrapDeltaMean(incS, repeatF(1, 18), b.BootstrapB, b.Seed)
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

// A measurement at n = 16 and B = 2000 measured a null ADOPT rate of 3.1% against a nominal 2.5%; at
// n = 8 it was 5.4%, and at B = 1 it was 45%. At the floors the rate must stay at or below 4%.
func TestNullAdoptRateAtTheFloorsIsCalibrated(t *testing.T) {
	if testing.Short() {
		t.Skip("3000 gate runs at B = 2000")
	}
	const n, sims = 16, 3000
	p := supPre(n)
	p.BootstrapB, p.MinPairs = 2000, 16
	b, h := freeze(t, p, nil)
	adopt := 0
	for s := 0; s < sims; s++ {
		rng := rand.New(rand.NewSource(int64(1000 + s)))
		inc, cand := Arm{Stamps: testStamps("i")}, Arm{Stamps: testStamps("c")}
		for _, id := range taskIDs(n) {
			si, sc := float64(rng.Intn(2)), float64(rng.Intn(2))
			inc.Tasks = append(inc.Tasks, Task{ID: id, Attempts: []Attempt{passOnly(si)}})
			cand.Tasks = append(cand.Tasks, Task{ID: id, Attempts: []Attempt{passOnly(sc)}})
		}
		if mustGate(t, b, h, inputFor(h, inc, cand)).Verdict == VerdictAdopt {
			adopt++
		}
	}
	rate := float64(adopt) / float64(sims)
	t.Logf("null ADOPT rate at n=%d, B=2000 over %d trials: %.4f", n, sims, rate)
	if rate > 0.04 {
		t.Fatalf("null ADOPT rate %.4f is above 0.04", rate)
	}
	// And the settings that gave 45%, 9% and 5.4% cannot be frozen.
	for _, bad := range []func(p *PreReg){
		func(p *PreReg) { p.BootstrapB = 1 },
		func(p *PreReg) { p.BootstrapB = 10 },
		func(p *PreReg) { p.MinPairs = 8 },
	} {
		q := supPre(n)
		bad(&q)
		if _, _, _, err := FreezeBaseline(q, testCorpus, nil); err == nil {
			t.Errorf("%+v must be refused", q)
		}
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
	inc := armFrom("i", n, func(int) []Attempt { return []Attempt{passTok(1, 1000)} })
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

// A candidate that times out on its expensive tasks has no cost for them: those pairs are unresolved,
// and the same cap catches it.
func TestTimedOutTasksLeaveTheirCostUnresolved(t *testing.T) {
	const n = 20
	b, h := freeze(t, niPre(n), nil)
	inc := armFrom("i", n, func(int) []Attempt {
		return []Attempt{{Outcome: OutcomePass, Score: f64(8), Metrics: tokenMetric(1000)}}
	})
	cand := armFrom("c", n, func(i int) []Attempt {
		if i < 5 {
			return []Attempt{{Outcome: OutcomeTimeout, Metrics: tokenMetric(1)}}
		}
		return []Attempt{{Outcome: OutcomePass, Score: f64(8), Metrics: tokenMetric(500)}}
	})
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	e := r.Efficiency[0]
	if e.N != 15 || e.UnresolvedPairs != 5 || !e.OverUnresolvedCap || r.Verdict == VerdictAdopt {
		t.Fatalf("a timed-out attempt's cost is not a measured cost: %+v %s", e, r.Verdict)
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
			return []Attempt{{Outcome: OutcomePass, Score: f64(8), Metrics: map[string]*float64{tokensName: f64(tok(i)), "memory_gib": f64(mem(i))}}}
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

func TestRegressionNeedsMinPairsAndAnIntervalWhollyOnTheWorseSide(t *testing.T) {
	const n = 20
	p := niPre(n)
	p.Efficiency = []PreRegEfficiency{
		{Name: tokensName, Class: EffTokens},
		{Name: "tokens_saved", Class: EffTokens, LowerIsBetter: boolp(false)},
	}
	share := 0.5 // wide enough that only min_pairs, not the unresolved cap, decides below
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
	// 16 resolved pairs, both dearer / lower: both regress.
	r := build(16, func(int) float64 { return 100 }, func(int) float64 { return -50 })
	if !r.Efficiency[0].Regresses || !r.Efficiency[1].Regresses {
		t.Fatalf("both metrics are wholly on the worse side: %+v", r.Efficiency)
	}
	// 15 resolved pairs is below min_pairs: no regression is declared (and nothing improves).
	r = build(15, func(int) float64 { return 100 }, func(int) float64 { return -50 })
	if r.Efficiency[0].Regresses || r.Efficiency[1].Regresses || r.Efficiency[0].Improves {
		t.Fatalf("fewer than min_pairs resolved pairs cannot regress or improve: %+v", r.Efficiency)
	}
	// An interval that spans zero is no regression.
	r = build(16, func(i int) float64 { return float64(300 * (1 - 2*(i%2))) }, func(i int) float64 { return float64(50 * (1 - 2*(i%2))) })
	if r.Efficiency[0].Regresses || r.Efficiency[1].Regresses {
		t.Fatalf("an interval spanning zero is not a regression: %+v", r.Efficiency)
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
				return []Attempt{{Outcome: OutcomePass, Score: f64(8), Metrics: m}}
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
// still inside the range, and a delta of the whole range is below it.
func TestAFailingCandidateIsBlockedUnderTheWidestMargin(t *testing.T) {
	const n = 16
	p := niUnitPre(n)
	p.Primary.Margin = f64(0.99)
	b, h := freeze(t, p, nil)
	inc := armFrom("i", n, func(int) []Attempt { return []Attempt{passTok(1, 1000)} })
	cand := armFrom("c", n, func(int) []Attempt {
		return []Attempt{{Outcome: OutcomeTaskFail, Score: f64(0), Metrics: tokenMetric(10)}}
	})
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	if r.Verdict != VerdictBlock {
		t.Fatalf("failing every task is worse than any margin inside the range: %s %s", r.Verdict, r.Reason)
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
