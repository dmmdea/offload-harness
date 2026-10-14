package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

// ---- fixtures -------------------------------------------------------------------------------
//
// The boundary rows use numbers that are exactly representable (8.0, 7.5, 7.0, 0.5, 16 pairs): a
// bootstrap replicate is a sum of 16 such values over 16, which is exact, so a row sits ON the
// boundary on every platform and a mutation of the comparison flips it deterministically.

const (
	tokensName = "forwarded_tokens"
	scorerID   = "acceptance-dsl"
)

// val reads a nullable interval end; a nil is NaN, which no comparison with a number satisfies.
func val(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

func repeatF(v float64, n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = v
	}
	return s
}

func plus(base []float64, d float64) []float64 {
	s := make([]float64, len(base))
	for i, v := range base {
		s[i] = v + d
	}
	return s
}

// alternate returns n values alternating a, b, a, b...
func alternate(a, b float64, n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		if i%2 == 0 {
			s[i] = a
		} else {
			s[i] = b
		}
	}
	return s
}

// arm builds an arm with one passing attempt per score, task ids t000.. in order. tokens, when
// non-nil, is the forwarded_tokens value of the same task.
func arm(tag string, scores, tokens []float64) Arm {
	a := Arm{Stamps: testStamps(tag)}
	for i, s := range scores {
		at := Attempt{Outcome: OutcomePass, Score: f64(s)}
		if tokens != nil {
			at.Metrics = map[string]*float64{tokensName: f64(tokens[i])}
		}
		a.Tasks = append(a.Tasks, Task{ID: taskIDs(len(scores))[i], Attempts: []Attempt{at}})
	}
	return a
}

func supPre(n int) PreReg {
	return PreReg{
		Mode:    ModeSuperiority,
		Primary: PreRegPrimary{Name: "goal_reached", Class: ClassUnitInterval},
		Tasks:   taskIDs(n),
	}
}

func niPre(n int) PreReg {
	return PreReg{
		Mode:       ModeNonInferiority,
		Primary:    PreRegPrimary{Name: "answer_quality", Class: ClassJudge010},
		Efficiency: []PreRegEfficiency{{Name: tokensName, Class: EffTokens}},
		Tasks:      taskIDs(n),
	}
}

func freeze(t *testing.T, p PreReg, incumbent *Arm) (Baseline, string) {
	t.Helper()
	b, _, h, err := FreezeBaseline(p, testCorpus, incumbent)
	if err != nil {
		t.Fatalf("freeze: %v", err)
	}
	return b, h
}

func detJudge() Judge { return Judge{Kind: "deterministic", Identity: scorerID} }

func inputFor(h string, inc, cand Arm) GateInput {
	return GateInput{BaselineHash: h, Incumbent: &inc, Candidate: cand, Judge: detJudge()}
}

func mustGate(t *testing.T, b Baseline, h string, in GateInput) GateResult {
	t.Helper()
	r, err := Gate(b, h, in)
	if err != nil {
		t.Fatalf("Gate refused: %v", err)
	}
	return r
}

func marshal(t *testing.T, r GateResult) string {
	t.Helper()
	j, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(j)
}

// ---- the table: verdicts ----------------------------------------------------------------------

func TestGateVerdictTable(t *testing.T) {
	sixteen := 16
	type row struct {
		name     string
		pre      PreReg
		inc, can Arm
		verdict  string
		reason   string // substring
		extra    func(t *testing.T, r GateResult)
	}
	zeros := repeatF(0, sixteen)
	ones := repeatF(1, sixteen)
	// 14 of 16 tasks flip from 0 to 1: a clear gain with a little spread.
	incSup := append(repeatF(0, 14), 1, 1)
	tokens := repeatF(1000, sixteen)

	rows := []row{
		{name: "1 superiority ADOPT", pre: supPre(sixteen), inc: arm("i", incSup, nil), can: arm("c", ones, nil),
			verdict: VerdictAdopt, extra: func(t *testing.T, r GateResult) {
				if r.KeepIncumbent || r.Lo <= 0 || r.NUsed != 16 {
					t.Fatalf("ADOPT must not keep the incumbent and must have lo>0: %+v", r)
				}
			}},
		{name: "2 superiority BLOCK", pre: supPre(sixteen), inc: arm("i", ones, nil), can: arm("c", append(repeatF(0, 14), 1, 1), nil),
			verdict: VerdictBlock, extra: func(t *testing.T, r GateResult) {
				if !r.KeepIncumbent || r.Hi >= 0 {
					t.Fatalf("BLOCK must keep the incumbent and have hi<0: %+v", r)
				}
			}},
		{name: "3 superiority INCONCLUSIVE (the CI spans zero)", pre: supPre(sixteen), inc: arm("i", alternate(0, 1, sixteen), nil), can: arm("c", alternate(1, 0, sixteen), nil),
			verdict: VerdictInconclusive, extra: func(t *testing.T, r GateResult) {
				if !r.KeepIncumbent || r.Lo >= 0 || r.Hi <= 0 {
					t.Fatalf("want an interval spanning zero: %+v", r)
				}
			}},
		{name: "4 non-inferiority ADOPT (inside the margin, tokens drop)", pre: niPre(sixteen),
			inc: arm("i", repeatF(8.0, sixteen), tokens), can: arm("c", repeatF(7.75, sixteen), repeatF(800, sixteen)),
			verdict: VerdictAdopt, extra: func(t *testing.T, r GateResult) {
				if r.Margin != 0.5 || len(r.Efficiency) != 1 || !r.Efficiency[0].Improves || val(r.Efficiency[0].Delta) != -200 {
					t.Fatalf("want margin 0.5 and an improving tokens metric (delta -200): %+v", r)
				}
			}},
		{name: "5 non-inferiority BLOCK (every difference -1.0, margin 0.5)", pre: niPre(sixteen),
			inc: arm("i", repeatF(8.0, sixteen), tokens), can: arm("c", repeatF(7.0, sixteen), repeatF(800, sixteen)),
			verdict: VerdictBlock, reason: "margin"},
		{name: "6 non-inferiority INCONCLUSIVE (the interval straddles -m)", pre: niPre(sixteen),
			inc: arm("i", repeatF(8.0, sixteen), tokens), can: arm("c", alternate(8.0, 7.0, sixteen), repeatF(800, sixteen)),
			verdict: VerdictInconclusive, extra: func(t *testing.T, r GateResult) {
				if !(r.Lo < -0.5 && r.Hi > -0.5) {
					t.Fatalf("setup: want lo < -m < hi, got %v %v", r.Lo, r.Hi)
				}
			}},
		{name: "6b boundary: every difference exactly -m with tokens down is INCONCLUSIVE, not ADOPT and not BLOCK", pre: niPre(sixteen),
			inc: arm("i", repeatF(8.0, sixteen), tokens), can: arm("c", repeatF(7.5, sixteen), repeatF(800, sixteen)),
			verdict: VerdictInconclusive, extra: func(t *testing.T, r GateResult) {
				if r.Lo != -0.5 || r.Hi != -0.5 || r.Delta != -0.5 {
					t.Fatalf("setup: the row must sit exactly on -m, got %v %v %v", r.Delta, r.Lo, r.Hi)
				}
				if !r.Efficiency[0].Improves {
					t.Fatal("setup: the efficiency metric must improve so only the primary boundary decides")
				}
			}},
		{name: "6c non-inferior with the metric present but equal: INCONCLUSIVE", pre: niPre(sixteen),
			inc: arm("i", repeatF(8.0, sixteen), tokens), can: arm("c", repeatF(7.75, sixteen), tokens),
			verdict: VerdictInconclusive, reason: "non-inferior but no declared efficiency metric improved", extra: func(t *testing.T, r GateResult) {
				if r.Lo <= -0.5 || r.Efficiency[0].Improves {
					t.Fatalf("setup: want lo > -m and a metric that does not improve: %+v", r)
				}
			}},
		{name: "6d non-inferior with the metric worse: INCONCLUSIVE", pre: niPre(sixteen),
			inc: arm("i", repeatF(8.0, sixteen), tokens), can: arm("c", repeatF(7.75, sixteen), repeatF(1200, sixteen)),
			verdict: VerdictInconclusive, reason: "no declared efficiency metric improved"},
		{name: "7 n below min_pairs is INCONCLUSIVE", pre: supPre(10), inc: arm("i", repeatF(0, 10), nil), can: arm("c", repeatF(1, 10), nil),
			verdict: VerdictInconclusive, reason: "n=10 below min_pairs=16"},
		{name: "9 a missing efficiency metric: non-inferiority cannot ADOPT", pre: niPre(sixteen),
			inc: arm("i", repeatF(8.0, sixteen), nil), can: arm("c", repeatF(8.0, sixteen), nil),
			verdict: VerdictInconclusive, reason: "no declared efficiency metric improved", extra: func(t *testing.T, r GateResult) {
				e := r.Efficiency[0]
				if e.N != 0 || e.Improves || e.UnresolvedPairs != 16 {
					t.Fatalf("an absent metric resolves nowhere and cannot improve: %+v", e)
				}
			}},
		{name: "11 incumbent tie (identical arrays): INCONCLUSIVE and keep_incumbent", pre: supPre(sixteen),
			inc: arm("i", alternate(0.25, 1, sixteen), nil), can: arm("c", alternate(0.25, 1, sixteen), nil),
			verdict: VerdictInconclusive, extra: func(t *testing.T, r GateResult) {
				if !r.KeepIncumbent || r.Lo != 0 || r.Hi != 0 || r.Delta != 0 {
					t.Fatalf("a tie must be exactly zero and keep the incumbent: %+v", r)
				}
			}},
		{name: "11b a tie on all zeros", pre: supPre(sixteen), inc: arm("i", zeros, nil), can: arm("c", zeros, nil),
			verdict: VerdictInconclusive},
	}
	for _, rw := range rows {
		t.Run(rw.name, func(t *testing.T) {
			b, h := freeze(t, rw.pre, nil)
			r := mustGate(t, b, h, inputFor(h, rw.inc, rw.can))
			if r.Verdict != rw.verdict {
				t.Fatalf("verdict %s (%s), want %s: %s", r.Verdict, r.Reason, rw.verdict, marshal(t, r))
			}
			if r.KeepIncumbent != (r.Verdict != VerdictAdopt) {
				t.Fatalf("keep_incumbent = %v with verdict %s", r.KeepIncumbent, r.Verdict)
			}
			if rw.reason != "" && !strings.Contains(r.Reason, rw.reason) {
				t.Fatalf("reason %q does not contain %q", r.Reason, rw.reason)
			}
			if rw.extra != nil {
				rw.extra(t, r)
			}
		})
	}
}

// Row 8: the excluded share, not the sample size, is what stops the run.
func TestGateExcludedShareAboveThresholdIsInconclusive(t *testing.T) {
	const declared = 40
	b, h := freeze(t, supPre(declared), nil)
	inc := arm("i", repeatF(0, declared), nil)
	cand := arm("c", repeatF(1, declared), nil)
	cand.Tasks = cand.Tasks[:24] // 16 declared tasks never ran on the candidate
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	if r.Verdict != VerdictInconclusive || !strings.Contains(r.Reason, "excluded_share=0.4") || !strings.Contains(r.Reason, "max_excluded_share=0.2") {
		t.Fatalf("want INCONCLUSIVE naming the share and the threshold, got %s: %s", r.Verdict, r.Reason)
	}
	if r.NUsed != 24 || r.ExcludedPairs != 16 || r.NDeclared != 40 || r.ExcludedShare != 0.4 {
		t.Fatalf("counts wrong: %+v", r)
	}
	// Missing tasks are one not_run attempt each on that arm.
	if r.AttemptCounts.Candidate["not_run"] != 16 || r.AttemptCounts.Incumbent["not_run"] != 0 || r.AttemptCounts.Candidate["pass"] != 24 {
		t.Fatalf("attempt counts wrong: %+v", r.AttemptCounts)
	}
	// Exactly at the threshold is allowed (the rule is "above").
	cand2 := arm("c", repeatF(1, declared), nil)
	cand2.Tasks = cand2.Tasks[:32] // 8 of 40 = 0.2
	r2 := mustGate(t, b, h, inputFor(h, inc, cand2))
	if r2.Verdict != VerdictAdopt {
		t.Fatalf("a share equal to the threshold is not above it: %s %s", r2.Verdict, r2.Reason)
	}
}

// The gate calls the existing paired bootstrap on the usable non-held-out pairs in sorted order,
// and reports its numbers untouched.
func TestGateUsesTheExistingBootstrapOnSortedPairs(t *testing.T) {
	inc := []float64{0.1, 0.9, 0.4, 0.5, 0.7, 0.2, 0.8, 0.3, 0.6, 0.0, 1.0, 0.55, 0.45, 0.65, 0.35, 0.25, 0.75, 0.15}
	cand := []float64{0.2, 0.95, 0.5, 0.5, 0.9, 0.1, 0.9, 0.5, 0.7, 0.1, 1.0, 0.6, 0.4, 0.8, 0.5, 0.3, 0.7, 0.3}
	n := len(inc)
	p := supPre(n)
	p.BootstrapB, p.MinPairs = 2000, 16
	seed := int64(42)
	p.Seed = &seed
	b, h := freeze(t, p, nil)
	ia, ca := arm("i", inc, nil), arm("c", cand, nil)
	// Present the candidate's tasks in reverse order: the gate sorts by id.
	for i, j := 0, len(ca.Tasks)-1; i < j; i, j = i+1, j-1 {
		ca.Tasks[i], ca.Tasks[j] = ca.Tasks[j], ca.Tasks[i]
	}
	r := mustGate(t, b, h, inputFor(h, ia, ca))
	d, lo, hi := BootstrapDeltaMean(inc, cand, 2000, 42)
	if r.Delta != d || r.Lo != lo || r.Hi != hi {
		t.Fatalf("gate (%v %v %v) != BootstrapDeltaMean (%v %v %v)", r.Delta, r.Lo, r.Hi, d, lo, hi)
	}
	if r.BootstrapB != 2000 || r.Seed != 42 {
		t.Fatalf("bootstrap settings not reported: %d %d", r.BootstrapB, r.Seed)
	}
}

// Efficiency: the same B and seed, one bootstrap per declared metric over the pairs that
// resolved it, direction read per metric. The per-task differences are NOT constant (-300 on
// even tasks, -100 on odd ones; +50 and +30 for the higher-is-better metric), so the bootstrap
// replicates spread and a different seed or a different B moves delta, lo or hi: the equality
// against a direct BootstrapDeltaMean call with the baseline's B and seed pins both.
func TestGateEfficiencyMetricsUseTheSameBootstrap(t *testing.T) {
	const n = 16
	p := niPre(n)
	p.Efficiency = []PreRegEfficiency{
		{Name: tokensName, Class: EffTokens},
		{Name: "peak_gib", Class: EffMemoryGiB},
		{Name: "tokens_saved", Class: EffTokens, LowerIsBetter: boolp(false)},
	}
	b, h := freeze(t, p, nil)
	inc := arm("i", repeatF(8, n), repeatF(1000, n))
	cand := arm("c", repeatF(8, n), nil)
	tokI := []float64{1000, 1100, 900, 1050, 950, 1000, 1200, 800, 1000, 1000, 1000, 1000, 1000, 1000, 1000, 1000}
	tokC := make([]float64, n)
	savedI, savedC := repeatF(100, n), make([]float64, n)
	for i := range tokC {
		if i%2 == 0 {
			tokC[i], savedC[i] = tokI[i]-300, savedI[i]+50
		} else {
			tokC[i], savedC[i] = tokI[i]-100, savedI[i]+30
		}
	}
	for i := range inc.Tasks {
		inc.Tasks[i].Attempts[0].Metrics = map[string]*float64{tokensName: f64(tokI[i]), "peak_gib": f64(10), "tokens_saved": f64(savedI[i])}
		cand.Tasks[i].Attempts[0].Metrics = map[string]*float64{tokensName: f64(tokC[i]), "peak_gib": f64(10), "tokens_saved": f64(savedC[i])}
	}
	// peak_gib: equal on both arms, so it cannot improve. tokens_saved: higher is better, and higher.
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	if len(r.Efficiency) != 3 {
		t.Fatalf("want 3 efficiency rows in declared order, got %+v", r.Efficiency)
	}
	d, lo, hi := BootstrapDeltaMean(tokI, tokC, b.BootstrapB, b.Seed)
	e0 := r.Efficiency[0]
	if !(lo < hi) {
		t.Fatalf("setup: the tokens interval must have width, got [%v, %v]: a point interval cannot tell one seed from another", lo, hi)
	}
	if e0.Name != tokensName || val(e0.Delta) != d || val(e0.Lo) != lo || val(e0.Hi) != hi || e0.N != n || e0.UnresolvedPairs != 0 || !e0.Improves || !e0.LowerIsBetter || e0.Class != EffTokens {
		t.Fatalf("tokens row wrong: %+v (want %v %v %v)", e0, d, lo, hi)
	}
	if e1 := r.Efficiency[1]; e1.Improves || e1.Delta == nil || *e1.Delta != 0 || e1.N != n {
		t.Fatalf("an unchanged metric must not improve (and its zero is a measured zero, not a missing one): %+v", e1)
	}
	sd, slo, shi := BootstrapDeltaMean(savedI, savedC, b.BootstrapB, b.Seed)
	e2 := r.Efficiency[2]
	if !(slo < shi) {
		t.Fatalf("setup: the tokens_saved interval must have width, got [%v, %v]", slo, shi)
	}
	if !e2.Improves || e2.LowerIsBetter || !(val(e2.Lo) > 0) || val(e2.Delta) != sd || val(e2.Lo) != slo || val(e2.Hi) != shi {
		t.Fatalf("a higher-is-better metric improves when lo > 0, with the same B and seed: %+v (want %v %v %v)", e2, sd, slo, shi)
	}
	// GATE-R2: ONE improving metric is enough. tokens and tokens_saved improve, peak_gib does not,
	// and the primary (8 against 8) is inside the margin: ADOPT. "All must improve" would not.
	if r.Verdict != VerdictAdopt {
		t.Fatalf("at least one improving metric adopts, whatever the others do: %s %s", r.Verdict, r.Reason)
	}
	// Reverse the direction of tokens_saved: it is now worse, not improved, and tokens alone adopts.
	for i := range cand.Tasks {
		cand.Tasks[i].Attempts[0].Metrics["tokens_saved"] = f64(50)
	}
	r2 := mustGate(t, b, h, inputFor(h, inc, cand))
	if r2.Efficiency[2].Improves {
		t.Fatalf("a higher-is-better metric that dropped must not improve: %+v", r2.Efficiency[2])
	}
	if r2.Verdict != VerdictAdopt {
		t.Fatalf("one improving metric (tokens) of three still adopts: %s %s", r2.Verdict, r2.Reason)
	}
	// The converse: no declared metric improves (tokens dearer, tokens_saved down, peak_gib equal).
	for i := range cand.Tasks {
		cand.Tasks[i].Attempts[0].Metrics[tokensName] = f64(tokI[i] + 300)
	}
	r3 := mustGate(t, b, h, inputFor(h, inc, cand))
	for _, e := range r3.Efficiency {
		if e.Improves {
			t.Fatalf("setup: no metric may improve in this row: %+v", e)
		}
	}
	if r3.Verdict != VerdictInconclusive || r3.Reason != "non-inferior but no declared efficiency metric improved" {
		t.Fatalf("no improving metric: want INCONCLUSIVE naming it, got %s %s", r3.Verdict, r3.Reason)
	}
}

// A metric improves only when the WHOLE interval is on the good side: an interval that spans zero
// does not improve, in either direction.
func TestGateEfficiencyIntervalSpanningZeroDoesNotImprove(t *testing.T) {
	const n = 16
	p := niPre(n)
	p.Efficiency = []PreRegEfficiency{
		{Name: tokensName, Class: EffTokens},
		{Name: "tokens_saved", Class: EffTokens, LowerIsBetter: boolp(false)},
	}
	b, h := freeze(t, p, nil)
	inc := arm("i", repeatF(8, n), nil)
	cand := arm("c", repeatF(8, n), nil)
	for i := range inc.Tasks {
		// The candidate is 300 cheaper on half the tasks and 300 dearer on the other half.
		delta := -300.0
		if i%2 == 1 {
			delta = 300
		}
		inc.Tasks[i].Attempts[0].Metrics = map[string]*float64{tokensName: f64(1000), "tokens_saved": f64(100)}
		cand.Tasks[i].Attempts[0].Metrics = map[string]*float64{tokensName: f64(1000 + delta), "tokens_saved": f64(100 - delta)}
	}
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	for _, e := range r.Efficiency {
		if !(val(e.Lo) < 0 && val(e.Hi) > 0) {
			t.Fatalf("setup: %s must span zero, got lo %v hi %v", e.Name, val(e.Lo), val(e.Hi))
		}
		if e.Improves {
			t.Errorf("%s: an interval spanning zero must not improve: %+v", e.Name, e)
		}
	}
	if r.Verdict != VerdictInconclusive {
		t.Fatalf("no metric improved, so non-inferiority cannot ADOPT: %s %s", r.Verdict, r.Reason)
	}
}

// Unresolved cost: a pair with a nil value on either side is skipped for that metric, counted,
// and never read as 0.
func TestGateUnresolvedCostIsSkippedNeverZero(t *testing.T) {
	const n = 20
	b, h := freeze(t, niPre(n), nil)
	inc := arm("i", repeatF(8, n), repeatF(1000, n))
	cand := arm("c", repeatF(8, n), repeatF(500, n))
	// Three pairs: the candidate's cost is unresolved (null). If nil were read as 0 these would
	// look like free runs and drag the delta; skipped, the delta stays exactly -500.
	for _, i := range []int{3, 7, 11} {
		cand.Tasks[i].Attempts[0].Metrics[tokensName] = nil
	}
	// One pair: the metric key is absent on the incumbent side.
	delete(inc.Tasks[1].Attempts[0].Metrics, tokensName)
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	e := r.Efficiency[0]
	if e.N != 16 || e.UnresolvedPairs != 4 || val(e.Delta) != -500 || val(e.Lo) != -500 || val(e.Hi) != -500 || !e.Improves {
		t.Fatalf("unresolved pairs must be skipped and counted: %+v", e)
	}
	if r.Verdict != VerdictAdopt {
		t.Fatalf("16 resolved pairs reach min_pairs=16 and the primary is inside the margin: %s %s", r.Verdict, r.Reason)
	}
}

func TestGateEfficiencyNeedsMinPairsResolved(t *testing.T) {
	const n = 20
	b, h := freeze(t, niPre(n), nil)
	inc := arm("i", repeatF(8, n), repeatF(1000, n))
	cand := arm("c", repeatF(8, n), repeatF(500, n))
	for i := 0; i < 5; i++ { // 5 unresolved leaves 15 < 16 resolved pairs
		cand.Tasks[i].Attempts[0].Metrics[tokensName] = nil
	}
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	e := r.Efficiency[0]
	if e.N != 15 || e.UnresolvedPairs != 5 || e.Improves {
		t.Fatalf("below min_pairs resolved pairs the metric is insufficient and does not improve: %+v", e)
	}
	if r.Verdict != VerdictInconclusive || !strings.Contains(r.Reason, "no declared efficiency metric improved") {
		t.Fatalf("non-inferiority cannot ADOPT on an insufficient metric: %s %s", r.Verdict, r.Reason)
	}
	// And an all-unresolved metric has no interval at all: delta, lo and hi are nil (null in the
	// JSON), never the 0 BootstrapDeltaMean returns for "nothing to conclude".
	for i := range cand.Tasks {
		cand.Tasks[i].Attempts[0].Metrics[tokensName] = nil
	}
	r2 := mustGate(t, b, h, inputFor(h, inc, cand))
	e2 := r2.Efficiency[0]
	if e2.N != 0 || e2.UnresolvedPairs != n || e2.Improves || e2.Delta != nil || e2.Lo != nil || e2.Hi != nil {
		t.Fatalf("all-unresolved: %+v", e2)
	}
}

// GATE-R3 / R2: a task's efficiency value is the mean of the RESOLVED values over its USABLE
// attempts. A nil value neither counts as 0 nor enters the divisor; an attempt that is not usable
// (infra_error here) contributes nothing, whatever it carries and wherever it sits in the list.
// Every task on the candidate arm has the same attempt list and every task on the incumbent costs
// 1500, so each bootstrap replicate is exactly want-1500 and delta, lo and hi pin the task value.
func TestGatePerTaskEfficiencyAggregation(t *testing.T) {
	const n = 16
	pass := func(tokens *float64) Attempt {
		return Attempt{Outcome: OutcomePass, Score: f64(8), Metrics: map[string]*float64{tokensName: tokens}}
	}
	for _, tc := range []struct {
		name     string
		attempts []Attempt
		want     float64
	}{
		{"a nil value is not 0 and not in the divisor; an infra_error's cost is ignored",
			[]Attempt{pass(f64(1000)), pass(nil), {Outcome: OutcomeInfraError, Metrics: map[string]*float64{tokensName: f64(1)}}}, 1000},
		{"two resolved values and a nil: the mean of the two",
			[]Attempt{pass(f64(1000)), pass(f64(600)), pass(nil)}, 800},
		{"an unusable attempt BEFORE the usable one is ignored too",
			[]Attempt{{Outcome: OutcomeTimeout, Metrics: map[string]*float64{tokensName: f64(1)}}, pass(f64(1000))}, 1000},
		{"a cheat is a usable attempt and its resolved cost counts",
			[]Attempt{{Outcome: OutcomeCheat, Score: f64(0), Metrics: map[string]*float64{tokensName: f64(400)}}, pass(f64(600))}, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, h := freeze(t, niPre(n), nil)
			inc := arm("i", repeatF(8, n), repeatF(1500, n))
			cand := arm("c", repeatF(8, n), nil)
			for i := range cand.Tasks {
				cand.Tasks[i].Attempts = append([]Attempt(nil), tc.attempts...)
			}
			r := mustGate(t, b, h, inputFor(h, inc, cand))
			e := r.Efficiency[0]
			want := tc.want - 1500
			if e.N != n || e.UnresolvedPairs != 0 || val(e.Delta) != want || val(e.Lo) != want || val(e.Hi) != want {
				t.Fatalf("the per-task cost must be %v (delta %v): got %+v", tc.want, want, e)
			}
		})
	}
}

// GATE-R6 power note under non-inferiority: with fewer than two usable pairs there is no interval
// to project from, so tasks_needed is "unbounded" (without the guard the margin yardstick would
// turn a zero half-width into "0 tasks needed").
func TestGatePowerNoteNonInferiorityBelowTwoPairs(t *testing.T) {
	p := niPre(4)
	p.MinPairs = 2
	b, h := freeze(t, p, nil)
	one := arm("c", repeatF(7.75, 4), repeatF(800, 4))
	one.Tasks = one.Tasks[:1]
	r := mustGate(t, b, h, inputFor(h, arm("i", repeatF(8, 4), repeatF(1000, 4)), one))
	if r.NUsed != 1 {
		t.Fatalf("setup: want one usable pair, got %d", r.NUsed)
	}
	if !r.Power.TasksNeeded.Unbounded || !r.Power.TasksToAdd.Unbounded || r.Power.HalfWidth != 0 {
		t.Fatalf("one usable pair projects nothing, under non-inferiority too: %+v", r.Power)
	}
	if j := marshal(t, r); !strings.Contains(j, `"tasks_needed":"unbounded"`) {
		t.Fatalf("unbounded must serialize as the word: %s", j)
	}
}

// GATE-R3: a metric with fewer than two resolved pairs has no interval, so its delta, lo and hi
// serialize as null, never as the 0 BootstrapDeltaMean returns for "nothing to conclude". A metric
// with two or more resolved pairs always has numbers, and a measured 0 stays 0.
func TestGateEfficiencyWithFewerThanTwoResolvedPairsIsNullNotZero(t *testing.T) {
	const n = 16
	decode := func(t *testing.T, r GateResult) map[string]map[string]any {
		t.Helper()
		var doc struct {
			Efficiency []map[string]any `json:"efficiency"`
		}
		if err := json.Unmarshal([]byte(marshal(t, r)), &doc); err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]any{}
		for _, e := range doc.Efficiency {
			out[e["name"].(string)] = e
		}
		return out
	}
	isNull := func(e map[string]any, k string) bool { v, present := e[k]; return present && v == nil }
	p := niPre(n)
	p.Efficiency = []PreRegEfficiency{
		{Name: tokensName, Class: EffTokens},
		{Name: "peak_gib", Class: EffMemoryGiB},
		{Name: "tokens_saved", Class: EffTokens, LowerIsBetter: boolp(false)},
	}
	b, h := freeze(t, p, nil)
	inc := arm("i", repeatF(8, n), nil)
	cand := arm("c", repeatF(8, n), nil)
	for i := range inc.Tasks {
		// tokens: resolved nowhere. peak_gib: resolved on exactly one pair. tokens_saved: resolved
		// on two pairs, and equal there (a measured zero difference).
		inc.Tasks[i].Attempts[0].Metrics = map[string]*float64{"tokens_saved": nil}
		cand.Tasks[i].Attempts[0].Metrics = map[string]*float64{"tokens_saved": nil}
	}
	inc.Tasks[0].Attempts[0].Metrics["peak_gib"], cand.Tasks[0].Attempts[0].Metrics["peak_gib"] = f64(10), f64(7)
	for _, i := range []int{3, 4} {
		inc.Tasks[i].Attempts[0].Metrics["tokens_saved"], cand.Tasks[i].Attempts[0].Metrics["tokens_saved"] = f64(100), f64(100)
	}
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	rows := decode(t, r)
	for _, name := range []string{tokensName, "peak_gib"} {
		e := rows[name]
		for _, k := range []string{"delta", "lo", "hi"} {
			if !isNull(e, k) {
				t.Errorf("%s: %s must be null with fewer than two resolved pairs, got %v (present %v)", name, k, e[k], e[k] != nil)
			}
		}
	}
	if rows[tokensName]["n"] != float64(0) || rows[tokensName]["unresolved_pairs"] != float64(n) {
		t.Errorf("an unresolved metric still reports its counts: %v", rows[tokensName])
	}
	if rows["peak_gib"]["n"] != float64(1) || rows["peak_gib"]["unresolved_pairs"] != float64(n-1) || rows["peak_gib"]["improves"] != false {
		t.Errorf("one resolved pair is not enough to improve, and is counted: %v", rows["peak_gib"])
	}
	// Two resolved pairs: the interval exists, and an exact 0 is a number, not null.
	e := rows["tokens_saved"]
	for _, k := range []string{"delta", "lo", "hi"} {
		if v, ok := e[k].(float64); !ok || v != 0 {
			t.Errorf("tokens_saved: %s with two resolved equal pairs is the number 0, got %v", k, e[k])
		}
	}
	if e["n"] != float64(2) {
		t.Errorf("tokens_saved n: %v", e["n"])
	}
	// The raw bytes carry no zero for the metrics that have no data.
	j := marshal(t, r)
	if strings.Count(j, `"delta":null`) != 2 || strings.Count(j, `"lo":null`) != 2 || strings.Count(j, `"hi":null`) != 2 {
		t.Errorf("want exactly two efficiency rows with null delta, lo and hi: %s", j)
	}
}

// Row 18: an unresolved cost survives the whole path as null and never appears as 0.
func TestGateUnresolvedCostNeverSerializesAsZero(t *testing.T) {
	doc := `{
	  "outcome": "pass", "score": 1,
	  "metrics": {"forwarded_tokens": null}
	}`
	var a Attempt
	if err := json.Unmarshal([]byte(doc), &a); err != nil {
		t.Fatal(err)
	}
	if p, ok := a.Metrics[tokensName]; !ok || p != nil {
		t.Fatalf("null must decode to a present nil, got %v %v", p, ok)
	}
	out, _ := json.Marshal(a)
	if strings.Contains(string(out), `"forwarded_tokens":0`) || !strings.Contains(string(out), `"forwarded_tokens":null`) {
		t.Fatalf("round trip lost the null: %s", out)
	}
	// Through the gate: all candidate costs are null, so the metric reports n=0 and the result
	// JSON has no zero standing in for a cost it never measured.
	const n = 16
	b, h := freeze(t, niPre(n), nil)
	inc := arm("i", repeatF(8, n), repeatF(1000, n))
	cand := arm("c", repeatF(8, n), repeatF(1, n))
	for i := range cand.Tasks {
		cand.Tasks[i].Attempts[0].Metrics[tokensName] = nil
	}
	in := inputFor(h, inc, cand)
	raw, _ := json.Marshal(in)
	if strings.Contains(string(raw), `"forwarded_tokens":0`) {
		t.Fatalf("the input serialized an unresolved cost as 0: %s", raw)
	}
	var back GateInput
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	r := mustGate(t, b, h, back)
	if e := r.Efficiency[0]; e.N != 0 || e.UnresolvedPairs != n || e.Improves {
		t.Fatalf("a null cost must stay unresolved end to end: %+v", e)
	}
}

// Row 16: a cheat is counted on its own and scored 0 whatever score it carries.
func TestGateCheatCountedSeparatelyAndScoredZero(t *testing.T) {
	const n = 16
	b, h := freeze(t, supPre(n), nil)
	incScores := repeatF(0.5, n)
	inc := arm("i", incScores, nil)
	cand := arm("c", repeatF(1, n), nil)
	for i := 8; i < n; i++ { // half the tasks cheat while carrying a perfect score
		cand.Tasks[i].Attempts[0].Outcome = OutcomeCheat
	}
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	candScores := append(repeatF(1, 8), repeatF(0, 8)...)
	d, lo, hi := BootstrapDeltaMean(incScores, candScores, b.BootstrapB, b.Seed)
	if r.Delta != d || r.Lo != lo || r.Hi != hi {
		t.Fatalf("a cheat must enter as 0: gate %v %v %v want %v %v %v", r.Delta, r.Lo, r.Hi, d, lo, hi)
	}
	if r.Cheats.Candidate != 8 || r.Cheats.Incumbent != 0 || r.AttemptCounts.Candidate["cheat"] != 8 || r.AttemptCounts.Candidate["pass"] != 8 {
		t.Fatalf("cheats must be counted per arm: %+v %+v", r.Cheats, r.AttemptCounts)
	}
	if r.Verdict == VerdictAdopt {
		t.Fatalf("a candidate that cheats on half its tasks must not be adopted on its claimed scores: %s", r.Reason)
	}
	// The cheat is a usable attempt: it does not exclude the pair.
	if r.NUsed != n || r.ExcludedPairs != 0 {
		t.Fatalf("a cheat is scored, not excluded: %+v", r)
	}
}

// Row 17 and R3: only pass, task_fail and cheat score; a task's score is the mean of its usable
// attempts; the other classes are counted and excluded.
func TestGateInfraErrorsAreExcludedAndCounted(t *testing.T) {
	const n = 20
	b, h := freeze(t, supPre(n), nil)
	inc := arm("i", repeatF(0.5, n), nil)
	cand := arm("c", repeatF(1, n), nil)
	// t000: the candidate's passing attempt is joined by an infra_error: score stays 1.0.
	cand.Tasks[0].Attempts = append(cand.Tasks[0].Attempts, Attempt{Outcome: OutcomeInfraError, Score: f64(0)})
	// t001: two usable attempts, 1.0 and 0.0: the task scores their mean, 0.5.
	cand.Tasks[1].Attempts = append(cand.Tasks[1].Attempts, Attempt{Outcome: OutcomeTaskFail, Score: f64(0)})
	// t002: only an infra_error: the pair is unusable.
	cand.Tasks[2].Attempts = []Attempt{{Outcome: OutcomeInfraError}}
	// t003 timeout, t004 judge_error, t005 not_run (each with a score that must be ignored).
	cand.Tasks[3].Attempts = []Attempt{{Outcome: OutcomeTimeout, Score: f64(1)}}
	cand.Tasks[4].Attempts = []Attempt{{Outcome: OutcomeJudgeError, Score: f64(1)}}
	cand.Tasks[5].Attempts = []Attempt{{Outcome: OutcomeNotRun, Score: f64(1)}}
	// t006: a task with an empty attempt list counts as not run.
	cand.Tasks[6].Attempts = nil
	// The incumbent lost t019 to an infra_error as well.
	inc.Tasks[19].Attempts = []Attempt{{Outcome: OutcomeInfraError}}
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	if r.ExcludedPairs != 6 || r.NUsed != 14 || r.NDeclared != 20 || r.ExcludedShare != 0.3 {
		t.Fatalf("want 6 excluded of 20 (t002-t006 and t019), got %+v", r)
	}
	wantCand := OutcomeCounts{"pass": 15, "task_fail": 1, "cheat": 0, "infra_error": 2, "timeout": 1, "judge_error": 1, "not_run": 2}
	for k, v := range wantCand {
		if r.AttemptCounts.Candidate[k] != v {
			t.Errorf("candidate %s = %d, want %d (%+v)", k, r.AttemptCounts.Candidate[k], v, r.AttemptCounts.Candidate)
		}
	}
	if r.AttemptCounts.Incumbent["infra_error"] != 1 || r.AttemptCounts.Incumbent["pass"] != 19 {
		t.Errorf("incumbent counts wrong: %+v", r.AttemptCounts.Incumbent)
	}
	if len(r.AttemptCounts.Candidate) != 7 || len(r.AttemptCounts.Incumbent) != 7 {
		t.Errorf("every class must be present, even at zero: %+v", r.AttemptCounts)
	}
	// 6/20 = 0.3 > 0.2 and n = 14 < 16: the run is INCONCLUSIVE and says why.
	if r.Verdict != VerdictInconclusive {
		t.Fatalf("got %s %s", r.Verdict, r.Reason)
	}
	// Scores: the usable pairs, sorted by id, carry t000=1.0, t001=0.5 on the candidate.
	var incS, candS []float64
	for i := 0; i < n; i++ {
		if i >= 2 && i <= 6 || i == 19 {
			continue
		}
		incS = append(incS, 0.5)
		switch i {
		case 1:
			candS = append(candS, 0.5)
		default:
			candS = append(candS, 1)
		}
	}
	d, lo, hi := BootstrapDeltaMean(incS, candS, b.BootstrapB, b.Seed)
	if r.Delta != d || r.Lo != lo || r.Hi != hi {
		t.Fatalf("per-task scores wrong: gate %v %v %v want %v %v %v", r.Delta, r.Lo, r.Hi, d, lo, hi)
	}
}

// R5: held-out pairs are a point-estimate check; a failure turns a main ADOPT into BLOCK.
func TestGateHeldOutFailureTurnsAdoptIntoBlock(t *testing.T) {
	const declared = 18
	p := supPre(declared)
	p.HeldOut = []string{"t016", "t017"}
	b, h := freeze(t, p, nil)
	incScores := append(repeatF(0, 16), 1, 1)
	// Main pairs: candidate clearly better. Held-out pairs: candidate worse.
	good := arm("c", append(repeatF(1, 16), 0, 0), nil)
	inc := arm("i", incScores, nil)
	r := mustGate(t, b, h, inputFor(h, inc, good))
	if r.Verdict != VerdictBlock || r.Reason != "held-out check failed; reject without re-tuning" || !r.KeepIncumbent {
		t.Fatalf("want BLOCK on the held-out failure, got %s %s", r.Verdict, r.Reason)
	}
	if r.HeldOut == nil || r.HeldOut.N != 2 || r.HeldOut.Delta != -1 || !r.HeldOut.Failed {
		t.Fatalf("held-out report wrong: %+v", r.HeldOut)
	}
	if r.NUsed != 16 || r.NDeclared != 16 || r.Lo <= 0 {
		t.Fatalf("held-out pairs must not enter the main comparison: %+v", r)
	}
	// Held-out not worse: ADOPT stands.
	ok := arm("c", repeatF(1, 18), nil)
	r2 := mustGate(t, b, h, inputFor(h, inc, ok))
	if r2.Verdict != VerdictAdopt || r2.HeldOut == nil || r2.HeldOut.Failed || r2.HeldOut.Delta != 0 {
		t.Fatalf("a held-out delta of 0 passes superiority: %+v %s", r2.HeldOut, r2.Verdict)
	}
	// No usable held-out pair at all: fails, "no held-out evidence" is not "held-out passed".
	none := arm("c", repeatF(1, 18), nil)
	none.Tasks = none.Tasks[:16]
	r3 := mustGate(t, b, h, inputFor(h, inc, none))
	if r3.Verdict != VerdictBlock || r3.HeldOut == nil || r3.HeldOut.N != 0 || !r3.HeldOut.Failed {
		t.Fatalf("no usable held-out pair must fail: %+v %s", r3.HeldOut, r3.Verdict)
	}
	// A failed held-out does not touch a verdict that was not ADOPT, and is still reported.
	tie := arm("c", incScores, nil)
	tie.Tasks[16].Attempts[0].Score, tie.Tasks[17].Attempts[0].Score = f64(0), f64(0)
	r4 := mustGate(t, b, h, inputFor(h, inc, tie))
	if r4.Verdict != VerdictInconclusive || r4.HeldOut == nil || !r4.HeldOut.Failed {
		t.Fatalf("an INCONCLUSIVE main verdict stays INCONCLUSIVE: %s %+v", r4.Verdict, r4.HeldOut)
	}
	// No held-out declared: held_out is null.
	b0, h0 := freeze(t, supPre(16), nil)
	r5 := mustGate(t, b0, h0, inputFor(h0, arm("i", incSupN(16), nil), arm("c", repeatF(1, 16), nil)))
	if r5.HeldOut != nil || !strings.Contains(marshal(t, r5), `"held_out":null`) {
		t.Fatalf("held_out must be null when none is declared: %s", marshal(t, r5))
	}
}

func incSupN(n int) []float64 { return append(repeatF(0, n-2), 1, 1) }

// Non-inferiority's held-out threshold is -m, not 0.
func TestGateHeldOutNonInferiorityThreshold(t *testing.T) {
	const declared = 18
	p := niPre(declared)
	p.HeldOut = []string{"t016", "t017"}
	b, h := freeze(t, p, nil)
	tokI, tokC := repeatF(1000, declared), repeatF(800, declared)
	inc := arm("i", repeatF(8, declared), tokI)
	// Held-out candidate scores exactly 7.5: delta -0.5 = -m, which is not below -m.
	c := arm("c", append(repeatF(7.75, 16), 7.5, 7.5), tokC)
	r := mustGate(t, b, h, inputFor(h, inc, c))
	if r.Verdict != VerdictAdopt || r.HeldOut.Delta != -0.5 || r.HeldOut.Failed {
		t.Fatalf("a held-out delta exactly at -m passes: %s %+v", r.Verdict, r.HeldOut)
	}
	c2 := arm("c", append(repeatF(7.75, 16), 7.25, 7.25), tokC)
	r2 := mustGate(t, b, h, inputFor(h, inc, c2))
	if r2.Verdict != VerdictBlock || !r2.HeldOut.Failed || r2.HeldOut.Delta != -0.75 {
		t.Fatalf("a held-out delta below -m fails: %s %+v", r2.Verdict, r2.HeldOut)
	}
}

// ---- refusals ---------------------------------------------------------------------------------

func TestGateRefusals(t *testing.T) {
	const n = 16
	good := func() (Baseline, string, GateInput) {
		b, h := freeze(t, supPre(n), nil)
		return b, h, inputFor(h, arm("i", repeatF(0, n), nil), arm("c", repeatF(1, n), nil))
	}
	modelJudge := Judge{Kind: "model", Identity: "judge-model-1"}
	okReceipt := &JudgeReceipt{ScorerID: "judge-model-1", PairsHash: "abc", Passed: true, BuildSHA: "def"}
	for _, tc := range []struct {
		name   string
		mutate func(b *Baseline, h *string, in *GateInput)
		want   string
	}{
		{"10 baseline hash mismatch (input)", func(b *Baseline, h *string, in *GateInput) { in.BaselineHash = strings.Repeat("0", 64) }, "baseline_hash"},
		{"10b baseline hash mismatch (handed in)", func(b *Baseline, h *string, in *GateInput) { *h = strings.Repeat("0", 64) }, "baseline_hash"},
		{"10c baseline edited after freeze", func(b *Baseline, h *string, in *GateInput) { b.MinPairs = 2 }, "does not match"},
		{"13 model judge without a receipt", func(b *Baseline, h *string, in *GateInput) { in.Judge = modelJudge }, "receipt"},
		{"14 model judge with a failing receipt", func(b *Baseline, h *string, in *GateInput) {
			in.Judge = modelJudge
			r := *okReceipt
			r.Passed = false
			in.Receipt = &r
		}, "did not pass"},
		{"14b model judge, receipt for another scorer", func(b *Baseline, h *string, in *GateInput) {
			in.Judge = modelJudge
			r := *okReceipt
			r.ScorerID = "another-model"
			in.Receipt = &r
		}, "scorer_id"},
		{"unknown judge kind", func(b *Baseline, h *string, in *GateInput) { in.Judge.Kind = "ensemble" }, "judge kind"},
		{"empty judge kind", func(b *Baseline, h *string, in *GateInput) { in.Judge.Kind = "" }, "judge kind"},
		{"empty judge identity", func(b *Baseline, h *string, in *GateInput) { in.Judge.Identity = "" }, "identity"},
		{"no incumbent anywhere", func(b *Baseline, h *string, in *GateInput) { in.Incumbent = nil }, "incumbent"},
		{"task not in the baseline (candidate)", func(b *Baseline, h *string, in *GateInput) {
			in.Candidate.Tasks = append(in.Candidate.Tasks, Task{ID: "zzz-new", Attempts: []Attempt{{Outcome: OutcomePass, Score: f64(1)}}})
		}, "not in the baseline"},
		{"task not in the baseline (incumbent)", func(b *Baseline, h *string, in *GateInput) {
			in.Incumbent.Tasks = append(in.Incumbent.Tasks, Task{ID: "zzz-new"})
		}, "not in the baseline"},
		{"scored attempt without a score", func(b *Baseline, h *string, in *GateInput) { in.Candidate.Tasks[0].Attempts[0].Score = nil }, "no score"},
		{"task_fail without a score", func(b *Baseline, h *string, in *GateInput) {
			in.Candidate.Tasks[0].Attempts[0] = Attempt{Outcome: OutcomeTaskFail}
		}, "no score"},
		{"invalid outcome", func(b *Baseline, h *string, in *GateInput) { in.Candidate.Tasks[0].Attempts[0].Outcome = "passed" }, "outcome"},
		{"empty outcome", func(b *Baseline, h *string, in *GateInput) { in.Incumbent.Tasks[0].Attempts[0].Outcome = "" }, "outcome"},
		{"repeated task id", func(b *Baseline, h *string, in *GateInput) {
			in.Candidate.Tasks = append(in.Candidate.Tasks, in.Candidate.Tasks[0])
		}, "twice"},
		{"candidate stamps missing", func(b *Baseline, h *string, in *GateInput) { in.Candidate.Stamps.EngineBuild = "" }, "stamps"},
		{"incumbent stamps missing", func(b *Baseline, h *string, in *GateInput) { in.Incumbent.Stamps = Stamps{} }, "stamps"},
		{"score outside the class range", func(b *Baseline, h *string, in *GateInput) { in.Candidate.Tasks[3].Attempts[0].Score = f64(1.5) }, "outside"},
		{"negative score", func(b *Baseline, h *string, in *GateInput) { in.Candidate.Tasks[3].Attempts[0].Score = f64(-0.1) }, "outside"},
		{"non-finite score", func(b *Baseline, h *string, in *GateInput) { in.Candidate.Tasks[3].Attempts[0].Score = f64(math.NaN()) }, "non-finite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, h, in := good()
			tc.mutate(&b, &h, &in)
			r, err := Gate(b, h, in)
			if err == nil {
				t.Fatalf("want a refusal containing %q, got verdict %s", tc.want, r.Verdict)
			}
			rf := refusalOf(t, err)
			if !strings.Contains(rf.Reason, tc.want) {
				t.Fatalf("refusal %q does not contain %q", rf.Reason, tc.want)
			}
			if r.Verdict != "" {
				t.Fatalf("a refusal carries no verdict, got %q", r.Verdict)
			}
		})
	}
	// A model judge with a passing receipt for the same scorer is accepted, and a deterministic
	// judge ignores a receipt it does not need.
	b, h, in := good()
	in.Judge, in.Receipt = modelJudge, okReceipt
	if _, err := Gate(b, h, in); err != nil {
		t.Fatalf("a model judge with a passing receipt must be accepted: %v", err)
	}
	b, h, in = good()
	in.Receipt = &JudgeReceipt{ScorerID: "x", Passed: false}
	if _, err := Gate(b, h, in); err != nil {
		t.Fatalf("a deterministic judge needs no receipt: %v", err)
	}
}

// R8 era check and the incumbent rules when the baseline froze one.
func TestGateFrozenIncumbentRules(t *testing.T) {
	const n = 16
	frozenInc := arm("i", repeatF(0, n), nil)
	b, h := freeze(t, supPre(n), &frozenInc)
	cand := arm("c", repeatF(1, n), nil)

	// No incumbent in the input: the frozen one is used.
	in := GateInput{BaselineHash: h, Candidate: cand, Judge: detJudge()}
	r := mustGate(t, b, h, in)
	if r.Verdict != VerdictAdopt || r.Stamps.Incumbent != frozenInc.Stamps {
		t.Fatalf("the frozen incumbent must be used: %s %+v", r.Verdict, r.Stamps)
	}
	// An equal incumbent in the input is fine, even with its tasks in another order.
	same := arm("i", repeatF(0, n), nil)
	same.Tasks[0], same.Tasks[5] = same.Tasks[5], same.Tasks[0]
	if _, err := Gate(b, h, inputFor(h, same, cand)); err != nil {
		t.Fatalf("an equal incumbent must be accepted: %v", err)
	}
	// Different stamps: another era.
	other := arm("i", repeatF(0, n), nil)
	other.Stamps.SeatConfigSHA256 = "a-different-seat-config"
	if _, err := Gate(b, h, inputFor(h, other, cand)); err == nil || !strings.Contains(err.Error(), "era") {
		t.Fatalf("a different incumbent stamp must be refused as another era, got %v", err)
	}
	// Same stamps, different data: not equal.
	diff := arm("i", repeatF(0, n), nil)
	diff.Tasks[2].Attempts[0].Score = f64(1)
	if _, err := Gate(b, h, inputFor(h, diff, cand)); err == nil || !strings.Contains(err.Error(), "not equal") {
		t.Fatalf("an incumbent that differs from the frozen one must be refused, got %v", err)
	}
	// The input's incumbent is not mutated by the check.
	if same.Tasks[0].ID != "t005" {
		t.Fatal("Gate reordered the caller's incumbent")
	}
}

// ---- determinism, purity, shape ---------------------------------------------------------------

func TestGateIsDeterministicAndPure(t *testing.T) {
	const n = 18
	p := niPre(n)
	p.HeldOut = []string{"t017"}
	b, h := freeze(t, p, nil)
	inc := arm("i", alternate(8.0, 7.0, n), repeatF(1000, n))
	cand := arm("c", alternate(7.75, 7.0, n), repeatF(900, n))
	in := inputFor(h, inc, cand)
	before, _ := json.Marshal(in)
	bBefore, _ := json.Marshal(b)
	r1 := marshal(t, mustGate(t, b, h, in))
	r2 := marshal(t, mustGate(t, b, h, in))
	if r1 != r2 {
		t.Fatalf("the same input twice must give byte-identical JSON:\n%s\n%s", r1, r2)
	}
	after, _ := json.Marshal(in)
	bAfter, _ := json.Marshal(b)
	if string(before) != string(after) || string(bBefore) != string(bAfter) {
		t.Fatal("Gate mutated its inputs")
	}
	// No clock in the output, and no wall-time vocabulary in any key.
	var keys []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, vv := range x {
				keys = append(keys, k)
				walk(vv)
			}
		case []any:
			for _, vv := range x {
				walk(vv)
			}
		}
	}
	var tree any
	if err := json.Unmarshal([]byte(r1), &tree); err != nil {
		t.Fatal(err)
	}
	walk(tree)
	for _, k := range keys {
		if timeTerm(k) != "" {
			t.Errorf("result key %q carries a wall-time term", k)
		}
	}
	// Field order is the struct's: verdict first, the verb's hash field last.
	if !strings.HasPrefix(r1, `{"verdict":`) || !strings.Contains(r1, `"reason":`) {
		t.Fatalf("unexpected leading fields: %.60s", r1)
	}
	order := []string{`"verdict"`, `"reason"`, `"keep_incumbent"`, `"mode"`, `"margin"`, `"delta"`, `"lo"`, `"hi"`, `"n_used"`,
		`"n_declared"`, `"excluded_pairs"`, `"excluded_share"`, `"attempt_counts"`, `"cheats"`, `"efficiency"`, `"held_out"`,
		`"power"`, `"stamps"`, `"judge"`, `"era_marker"`, `"gate_version"`, `"corpus_hash"`, `"baseline_hash"`, `"bootstrap_b"`, `"seed"`}
	last := -1
	for _, k := range order {
		i := strings.Index(r1, k)
		if i < 0 || i < last {
			t.Fatalf("field %s missing or out of order in %s", k, r1)
		}
		last = i
	}
}

func TestGateReportsBaselineIdentity(t *testing.T) {
	const n = 16
	b, h := freeze(t, supPre(n), nil)
	r := mustGate(t, b, h, inputFor(h, arm("i", repeatF(0, n), nil), arm("c", repeatF(1, n), nil)))
	if r.GateVersion != "1" || r.CorpusHash != b.CorpusHash || r.BaselineHash != h || r.Mode != ModeSuperiority || r.Margin != 0 {
		t.Fatalf("identity fields wrong: %+v", r)
	}
	if r.Judge != detJudge() || r.Stamps.Candidate != testStamps("c") || r.Stamps.Incumbent != testStamps("i") {
		t.Fatalf("judge and stamps must be reported: %+v", r)
	}
	if r.GateBuildSHA256 != "" {
		t.Fatal("Gate is pure: the verb, not Gate, stamps the binary's hash")
	}
}

// GATE-R8: the era marker is the sha256 of the canonical JSON of the gate version, both stamps
// and the judge identity, and of nothing else.
func TestGateEraMarker(t *testing.T) {
	const n = 16
	b, h := freeze(t, supPre(n), nil)
	inc, cand := arm("i", repeatF(0, n), nil), arm("c", repeatF(1, n), nil)
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	want := func(inc, cand Stamps, identity string) string {
		doc := struct {
			GateVersion     string `json:"gate_version"`
			IncumbentStamps Stamps `json:"incumbent_stamps"`
			CandidateStamps Stamps `json:"candidate_stamps"`
			JudgeIdentity   string `json:"judge_identity"`
		}{"1", inc, cand, identity}
		j, _ := json.Marshal(doc)
		s := sha256.Sum256(j)
		return hex.EncodeToString(s[:])
	}
	if r.EraMarker != want(inc.Stamps, cand.Stamps, scorerID) {
		t.Fatalf("era marker %s != %s", r.EraMarker, want(inc.Stamps, cand.Stamps, scorerID))
	}
	// Scores do not move it.
	r2 := mustGate(t, b, h, inputFor(h, arm("i", repeatF(0.5, n), nil), arm("c", repeatF(0.25, n), nil)))
	if r2.EraMarker != r.EraMarker {
		t.Fatal("the era marker must not depend on the scores")
	}
	// Each of its parts does.
	for name, mut := range map[string]func(inc, cand *Arm, j *Judge){
		"incumbent harness": func(inc, cand *Arm, j *Judge) { inc.Stamps.HarnessBuildSHA256 = "x" },
		"incumbent seat":    func(inc, cand *Arm, j *Judge) { inc.Stamps.SeatConfigSHA256 = "x" },
		"incumbent engine":  func(inc, cand *Arm, j *Judge) { inc.Stamps.EngineBuild = "x" },
		"candidate harness": func(inc, cand *Arm, j *Judge) { cand.Stamps.HarnessBuildSHA256 = "x" },
		"candidate seat":    func(inc, cand *Arm, j *Judge) { cand.Stamps.SeatConfigSHA256 = "x" },
		"candidate engine":  func(inc, cand *Arm, j *Judge) { cand.Stamps.EngineBuild = "x" },
		"judge identity":    func(inc, cand *Arm, j *Judge) { j.Identity = "another-scorer" },
	} {
		i2, c2, j2 := arm("i", repeatF(0, n), nil), arm("c", repeatF(1, n), nil), detJudge()
		mut(&i2, &c2, &j2)
		in := inputFor(h, i2, c2)
		in.Judge = j2
		if got := mustGate(t, b, h, in).EraMarker; got == r.EraMarker {
			t.Errorf("changing the %s must change the era marker", name)
		}
	}
	// The judge kind is not part of it (only the identity).
	in := inputFor(h, inc, cand)
	in.Judge = Judge{Kind: "model", Identity: scorerID}
	in.Receipt = &JudgeReceipt{ScorerID: scorerID, Passed: true}
	if mustGate(t, b, h, in).EraMarker != r.EraMarker {
		t.Fatal("only the judge identity is in the era marker")
	}
}

// GATE-R6 power note: the half-width of the main interval read three ways, and how many tasks to add.
func TestGatePowerNote(t *testing.T) {
	const n = 16
	// A small effect on a small corpus: 2 of 16 tasks gain 1.0.
	b, h := freeze(t, supPre(n), nil)
	inc := arm("i", repeatF(0, n), nil)
	cand := arm("c", append(repeatF(0, 14), 1, 1), nil)
	r := mustGate(t, b, h, inputFor(h, inc, cand))
	hw := (r.Hi - r.Lo) / 2
	if hw <= 0 {
		t.Fatalf("setup: want a non-degenerate interval, got %v %v", r.Lo, r.Hi)
	}
	if r.Power.HalfWidth != hw {
		t.Fatalf("half_width %v != (hi-lo)/2 %v", r.Power.HalfWidth, hw)
	}
	for _, c := range []struct {
		n    float64
		got  float64
		name string
	}{{16, r.Power.Projected.N16, "16"}, {24, r.Power.Projected.N24, "24"}, {120, r.Power.Projected.N120, "120"}} {
		if want := hw * math.Sqrt(float64(n)/c.n); c.got != want {
			t.Errorf("projected[%s] = %v, want %v", c.name, c.got, want)
		}
	}
	// Superiority: the observed |delta| stands in for the margin.
	wantNeeded := int(math.Ceil(float64(n) * (hw / math.Abs(r.Delta)) * (hw / math.Abs(r.Delta))))
	if r.Power.TasksNeeded.Unbounded || r.Power.TasksNeeded.N != wantNeeded {
		t.Fatalf("tasks_needed %+v, want %d", r.Power.TasksNeeded, wantNeeded)
	}
	if want := wantNeeded - n; want > 0 && r.Power.TasksToAdd.N != want || want <= 0 && r.Power.TasksToAdd.N != 0 {
		t.Fatalf("tasks_to_add %+v, want max(0, %d)", r.Power.TasksToAdd, want)
	}
	if r.Power.TasksToAdd.N <= 0 {
		t.Fatalf("setup: this row must ask for more tasks (needed %d, n %d)", wantNeeded, n)
	}
	// Superiority with no observed effect: unbounded, in JSON the word.
	tie := mustGate(t, b, h, inputFor(h, inc, arm("c", repeatF(0, n), nil)))
	if !tie.Power.TasksNeeded.Unbounded || !tie.Power.TasksToAdd.Unbounded {
		t.Fatalf("a zero effect needs an unbounded number of tasks: %+v", tie.Power)
	}
	if j := marshal(t, tie); !strings.Contains(j, `"tasks_needed":"unbounded"`) || !strings.Contains(j, `"tasks_to_add":"unbounded"`) {
		t.Fatalf("unbounded must serialize as the word: %s", j)
	}
	if j := marshal(t, r); !strings.Contains(j, fmt.Sprintf(`"tasks_needed":%d`, wantNeeded)) || !strings.Contains(j, `"projected":{"16":`) {
		t.Fatalf("bounded counts serialize as numbers and projected keys are 16, 24, 120: %s", j)
	}
	// Non-inferiority: the margin is the yardstick.
	bn, hn := freeze(t, niPre(n), nil)
	rn := mustGate(t, bn, hn, inputFor(hn, arm("i", repeatF(8, n), repeatF(1000, n)), arm("c", alternate(8, 7, n), repeatF(800, n))))
	hwn := (rn.Hi - rn.Lo) / 2
	wantN := int(math.Ceil(float64(n) * (hwn / 0.5) * (hwn / 0.5)))
	if rn.Power.TasksNeeded.Unbounded || rn.Power.TasksNeeded.N != wantN {
		t.Fatalf("non-inferiority tasks_needed %+v, want %d (hw %v)", rn.Power.TasksNeeded, wantN, hwn)
	}
	if rn.Power.TasksToAdd.N != max(0, wantN-n) {
		t.Fatalf("tasks_to_add %+v", rn.Power.TasksToAdd)
	}
	// Below two usable pairs there is no interval to project from.
	bs, hs := freeze(t, func() PreReg { p := supPre(4); p.MinPairs = 2; return p }(), nil)
	one := arm("c", repeatF(1, 4), nil)
	one.Tasks = one.Tasks[:1]
	rs := mustGate(t, bs, hs, inputFor(hs, arm("i", repeatF(0, 4), nil), one))
	if rs.NUsed != 1 || !rs.Power.TasksNeeded.Unbounded || rs.Power.HalfWidth != 0 {
		t.Fatalf("one usable pair projects nothing: %+v", rs.Power)
	}
}

func TestTaskCountJSON(t *testing.T) {
	for _, c := range []struct {
		in   TaskCount
		want string
	}{{TaskCount{N: 7}, `7`}, {TaskCount{N: 0}, `0`}, {TaskCount{Unbounded: true}, `"unbounded"`}} {
		j, err := json.Marshal(c.in)
		if err != nil || string(j) != c.want {
			t.Errorf("marshal %+v = %s %v, want %s", c.in, j, err, c.want)
		}
		var back TaskCount
		if err := json.Unmarshal(j, &back); err != nil || back != c.in {
			t.Errorf("round trip of %s = %+v %v", j, back, err)
		}
	}
	var bad TaskCount
	if err := json.Unmarshal([]byte(`"lots"`), &bad); err == nil {
		t.Error(`"lots" must not decode`)
	}
}
