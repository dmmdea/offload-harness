package eval

// The closed-form calibration study: how often candidate primary intervals say ADOPT when they should not, and
// when they should, on the grids the docs and the ADR quote.
//
// It complements TestCalibrationStudy (gate_sim_test.go), which compares bootstrap intervals and needs
// thousands of resamples per dataset. The intervals here are closed forms, so a cell takes milliseconds, and a
// cell whose scores are pass/fail is not even simulated: a dataset of pass/fail pairs is three counts, and the
// rate is the exact multinomial sum of gate_exact_test.go. Cells with other scores (uniform, deciles, x^3-skewed,
// judge integers, attempt means, partial credit) are simulated, with seeds derived from the cell's name and size.
//
//	OFFLOAD_GATE_STUDY=1 go test ./internal/eval/ -run 'TestCalibrationStudy|TestSparseCalibrationStudy' -v -timeout 1h
//
// TestCalibrationStudy runs this study after its bootstrap tables. Optional: OFFLOAD_GATE_STUDY_SIMS (datasets per
// simulated cell, default 20000), OFFLOAD_GATE_STUDY_ONLY (a substring of the cell names to run) and
// OFFLOAD_GATE_STUDY_ENV_NMAX (the largest size of the envelope scan, default 500).

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// ivlFn is an interval for the mean of n paired differences d, each in [-span, span].
type ivlFn func(d []float64, span float64) (lo, hi float64)

// ivlAug is the score form of the t interval on the sample with pseudo-pairs added: wp at +span, wm at -span and wz
// ties (the augmented size n + wp + wm + wz is an integer in every variant used). Centred, the interval is placed on
// the sample's own mean and only its width comes from the augmented sample; otherwise it is placed on the augmented
// mean. The production fallback is ivlAug(2, 2, 0, true).
func ivlAug(wp, wm, wz float64, centred bool) ivlFn {
	return func(d []float64, span float64) (float64, float64) {
		n := len(d)
		nn := float64(n) + wp + wm + wz
		var sum float64
		for _, v := range d {
			sum += v
		}
		am := (sum + (wp-wm)*span) / nn
		var ss float64
		for _, v := range d {
			ss += (v - am) * (v - am)
		}
		ss += wp*(span-am)*(span-am) + wm*(-span-am)*(-span-am) + wz*am*am
		c := tCritical975(int(math.Round(nn)) - 1)
		half := c * math.Sqrt(ss/nn) / math.Sqrt(math.Max(nn-c*c, 1))
		ctr := am
		if centred {
			ctr = sum / float64(n)
		}
		return math.Max(-span, ctr-half), math.Min(span, ctr+half)
	}
}

// ivlProduction is the gate's primary interval, called as the gate calls it.
func ivlProduction(d []float64, span float64) (float64, float64) {
	_, lo, hi := primaryInterval(make([]float64, len(d)), d, span)
	return lo, hi
}

// studyMethod is a named interval.
type studyMethod struct {
	name string
	ivl  ivlFn
}

func studyMethods() []studyMethod {
	return []studyMethod{
		{"v2 (previous rule)", intervalV2},
		{"half pair at 4 cells (Agresti-Min)", ivlAug(.5, .5, 1, false)},
		{"1 pair at each end", ivlAug(1, 1, 0, true)},
		{"2 pairs at each end, all data", ivlAug(2, 2, 0, true)},
		{"production (Tango + 2 pairs)", ivlProduction},
	}
}

func (m studyMethod) adopts(d []float64, span, mu0 float64) bool {
	lo, _ := m.ivl(d, span)
	return lo > mu0+equalTolerance*span
}

// ---- cells ----------------------------------------------------------------------------------------

// studyCell is one null or power cell: pass/fail pairs with win and loss probabilities u and v (exact), or pairs
// drawn by gen (simulated); mu0 is the boundary the lower end has to clear (0, or -margin) and span the score range.
type studyCell struct {
	name  string
	n     int
	mu0   float64
	span  float64
	exact bool
	u, v  float64
	gen   pairGen
}

func (c studyCell) width() float64 {
	if c.span > 0 {
		return c.span
	}
	return 1
}

func exactCell(name string, n int, mu0, u, v float64) studyCell {
	return studyCell{name: name, n: n, mu0: mu0, exact: true, u: u, v: v}
}

// rate is the cell's ADOPT rate for the method: exact for pass/fail cells, over sims datasets otherwise.
func (c studyCell) rate(m studyMethod, sims int) float64 {
	span := c.width()
	if c.exact {
		var r float64
		trinaryMass(c.n, c.u, c.v, 1e-14, func(x, y int, p float64) {
			if m.adopts(trinaryData(c.n, x, y, span), span, c.mu0) {
				r += p
			}
		})
		return r
	}
	base := cellSeed(c.name, c.n)
	adopt := 0
	d := make([]float64, c.n)
	for s := 0; s < sims; s++ {
		inc, cand := c.gen(rand.New(rand.NewSource(base+int64(s))), c.n)
		for i := range d {
			d[i] = cand[i] - inc[i]
		}
		if m.adopts(d, span, c.mu0) {
			adopt++
		}
	}
	return float64(adopt) / float64(sims)
}

// existingNullCells are the 63 null cells of the earlier study: six superiority nulls at 16, 24 and 40 tasks and
// nine non-inferiority nulls at exactly -0.05 at 16, 20, 24, 40 and 80.
func existingNullCells() []studyCell {
	var out []studyCell
	for _, n := range []int{16, 24, 40} {
		out = append(out,
			exactCell("sup binary p=.50", n, 0, .25, .25),
			exactCell("sup binary p=.85", n, 0, .85*.15, .85*.15),
			studyCell{name: "sup uniform", n: n, gen: genUniform(0, 1, 0, 1)},
			studyCell{name: "sup deciles 0..1", n: n, gen: genLattice(0, 10, 1)},
			studyCell{name: "sup x^3 skewed", n: n, gen: genCubed(1)},
			func() studyCell { c := exactCell("sup two-point 0/10", n, 0, .25, .25); c.span = 10; return c }(),
		)
	}
	for _, n := range []int{16, 20, 24, 40, 80} {
		u1, v1 := indep(.90, .85)
		u2, v2 := indep(.70, .65)
		u3, v3 := indep(.55, .50)
		u4, v4 := indep(.95, .90)
		u5, v5 := indep(.97, .92)
		u6, v6 := indep(.99, .94)
		out = append(out,
			exactCell("ni binary .90/.85", n, -.05, u1, v1),
			exactCell("ni binary .70/.65", n, -.05, u2, v2),
			exactCell("ni binary .55/.50", n, -.05, u3, v3),
			studyCell{name: "ni continuous uniform", n: n, mu0: -.05, gen: genUniform(0, 1, 0, .9)},
			studyCell{name: "ni continuous x^3 skewed", n: n, mu0: -.05, gen: genCubed(.8)},
			exactCell("ni binary .95/.90", n, -.05, u4, v4),
			exactCell("ni binary .97/.92", n, -.05, u5, v5),
			exactCell("ni binary .99/.94", n, -.05, u6, v6),
			exactCell("ni nested loss 5%", n, -.05, 0, .05),
		)
	}
	return out
}

// sparseNullCells are the strong-incumbent nulls at the sizes where one more loss or tie moves the verdict: a
// candidate that only loses tasks and loses 5% of them, and pass rates .99 and .94, .97 and .92, at a margin of
// 0.05; and the same shapes moved to sit at a margin of 0.10 (nested loss 10%, .99 and .89, .97 and .87).
func sparseNullCells() []studyCell {
	var out []studyCell
	for _, n := range []int{16, 24, 40, 60, 61, 62, 64, 80, 100, 160, 250, 500, 1000} {
		u1, v1 := indep(.99, .94)
		u2, v2 := indep(.97, .92)
		out = append(out,
			exactCell("sparse nested loss 5%", n, -.05, 0, .05),
			exactCell("sparse binary .99/.94", n, -.05, u1, v1),
			exactCell("sparse binary .97/.92", n, -.05, u2, v2),
		)
	}
	for _, n := range []int{16, 31, 32, 40, 80} {
		u1, v1 := indep(.99, .89)
		u2, v2 := indep(.97, .87)
		out = append(out,
			exactCell("sparse m=.10 nested loss 10%", n, -.10, 0, .10),
			exactCell("sparse m=.10 binary .99/.89", n, -.10, u1, v1),
			exactCell("sparse m=.10 binary .97/.87", n, -.10, u2, v2),
		)
	}
	return out
}

// genPartialNested: pass/fail with a few half-credit tasks; the candidate only loses (a fraction of the tasks it
// scored above 0 drop to 0), true difference -0.05.
func genPartialNested(rng *rand.Rand, n int) ([]float64, []float64) {
	inc, cand := make([]float64, n), make([]float64, n)
	a := .05 / .9
	for i := 0; i < n; i++ {
		switch r := rng.Float64(); {
		case r < .85:
			inc[i] = 1
		case r < .95:
			inc[i] = .5
		}
		cand[i] = inc[i]
		if inc[i] > 0 && rng.Float64() < a {
			cand[i] = 0
		}
	}
	return inc, cand
}

// genHalfOnly: the candidate drops tasks the incumbent passed to half credit, true difference -0.05.
func genHalfOnly(rng *rand.Rand, n int) ([]float64, []float64) {
	inc, cand := make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		inc[i] = bernoulli(rng, .9, 1)
		cand[i] = inc[i]
		if inc[i] == 1 && rng.Float64() < .1/.9 {
			cand[i] = .5
		}
	}
	return inc, cand
}

// genMixedSize: half the losses are to 0 and half to 0.5, true difference -0.05.
func genMixedSize(rng *rand.Rand, n int) ([]float64, []float64) {
	inc, cand := make([]float64, n), make([]float64, n)
	q := .05 / (.9 * .75)
	for i := 0; i < n; i++ {
		inc[i] = bernoulli(rng, .9, 1)
		cand[i] = inc[i]
		if inc[i] == 1 && rng.Float64() < q {
			cand[i] = .5
			if rng.Float64() < .5 {
				cand[i] = 0
			}
		}
	}
	return inc, cand
}

// genAttemptMeans: k attempts per task on each arm, pass probabilities pi and pc, a task's score the share of passes.
func genAttemptMeans(k int, pi, pc float64) pairGen {
	return func(rng *rand.Rand, n int) ([]float64, []float64) {
		inc, cand := make([]float64, n), make([]float64, n)
		for i := 0; i < n; i++ {
			var a, b float64
			for j := 0; j < k; j++ {
				a += bernoulli(rng, pi, 1)
				b += bernoulli(rng, pc, 1)
			}
			inc[i], cand[i] = a/float64(k), b/float64(k)
		}
		return inc, cand
	}
}

// genMixedDifficultyAttempts: five attempts per task; 80% of tasks always pass and 20% pass half the time, on both arms.
func genMixedDifficultyAttempts(rng *rand.Rand, n int) ([]float64, []float64) {
	inc, cand := make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		q := 1.0
		if rng.Float64() < .2 {
			q = .5
		}
		var a, b float64
		for j := 0; j < 5; j++ {
			a += bernoulli(rng, q, 1)
			b += bernoulli(rng, q, 1)
		}
		inc[i], cand[i] = a/5, b/5
	}
	return inc, cand
}

// genJudge: independent integer judge scores on 0..10, normal around mu with spread sigma, for both arms.
func genJudge(mu, sigma float64) pairGen {
	one := func(rng *rand.Rand) float64 {
		return math.Min(10, math.Max(0, math.Round(mu+sigma*rng.NormFloat64())))
	}
	return func(rng *rand.Rand, n int) ([]float64, []float64) {
		inc, cand := make([]float64, n), make([]float64, n)
		for i := 0; i < n; i++ {
			inc[i], cand[i] = one(rng), one(rng)
		}
		return inc, cand
	}
}

// stressNullCells are nulls whose scores are not pass/fail but whose disagreements are few and coarse: the regime where
// the production interval is the augmented fallback and not Tango's.
func stressNullCells() []studyCell {
	var out []studyCell
	for _, n := range []int{16, 24, 40, 60, 61, 62, 64, 80, 100, 160, 250, 500, 1000} {
		out = append(out,
			studyCell{name: "stress partial-credit nested loss 5%", n: n, mu0: -.05, gen: genPartialNested},
			studyCell{name: "stress half-credit-only nested loss 5%", n: n, mu0: -.05, gen: genHalfOnly},
			studyCell{name: "stress mixed-size nested loss 5%", n: n, mu0: -.05, gen: genMixedSize},
		)
	}
	for _, n := range []int{24, 40, 61, 100, 250} {
		out = append(out,
			studyCell{name: "stress 5 attempts .98->.93", n: n, mu0: -.05, gen: genAttemptMeans(5, .98, .93)},
			studyCell{name: "stress 3 attempts .97->.92", n: n, mu0: -.05, gen: genAttemptMeans(3, .97, .92)},
		)
	}
	return out
}

// studyPowerCells are true effects the gate should detect: the earlier study's rows, a strong incumbent at the sizes 16, 40,
// 100 and 250, a candidate with identical outputs (every pair a tie), and the non-binary scores the fallback reads.
func studyPowerCells() []studyCell {
	var out []studyCell
	for _, n := range []int{16, 24, 40} {
		u, v := indep(.5, .6)
		out = append(out, exactCell("power sup binary .50->.60", n, 0, u, v))
	}
	for _, n := range []int{16, 24, 40, 100, 250} {
		u, v := indep(.9, .9)
		out = append(out, exactCell("power ni binary .90->.90", n, -.05, u, v))
	}
	for _, n := range []int{16, 40, 100, 250} {
		u, v := indep(.95, .95)
		out = append(out, exactCell("power ni binary .95->.95", n, -.05, u, v))
	}
	for _, n := range []int{16, 40, 73, 100, 250} {
		out = append(out, exactCell("power ni identical outputs", n, -.05, 0, 0))
	}
	out = append(out,
		studyCell{name: "power sup uniform +.10", n: 24, gen: genUniform(0, 1, .2, 1)},
		studyCell{name: "power ni uniform 0", n: 24, mu0: -.05, gen: genUniform(0, 1, 0, 1)},
	)
	for _, n := range []int{24, 40, 61, 100, 250} {
		out = append(out,
			studyCell{name: "power ni judge 0..10, equal arms", n: n, mu0: -.5, span: 10, gen: genJudge(7, 1.5)},
			studyCell{name: "power sup judge 0..10, +1 point", n: n, span: 10, gen: func(rng *rand.Rand, n int) ([]float64, []float64) {
				a, _ := genJudge(7, 1.5)(rng, n)
				_, b := genJudge(8, 1.5)(rng, n)
				return a, b
			}},
			studyCell{name: "power ni 5 attempts, mixed difficulty, equal arms", n: n, mu0: -.05, gen: genMixedDifficultyAttempts},
			studyCell{name: "power ni 5 attempts .95->.95", n: n, mu0: -.05, gen: genAttemptMeans(5, .95, .95)},
		)
	}
	return out
}

// ---- the printed study ------------------------------------------------------------------------------

func runStudyGrid(cells []studyCell, methods []studyMethod, sims int) [][]float64 {
	out := make([][]float64, len(cells))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i := range cells {
		out[i] = make([]float64, len(methods))
		for k := range methods {
			wg.Add(1)
			sem <- struct{}{}
			go func(i, k int) {
				defer wg.Done()
				out[i][k] = cells[i].rate(methods[k], sims)
				<-sem
			}(i, k)
		}
	}
	wg.Wait()
	return out
}

func printStudyGrid(title string, cells []studyCell, methods []studyMethod, rates [][]float64, null bool) {
	fmt.Printf("\n### %s\n\n| cell | n |", title)
	for _, m := range methods {
		fmt.Printf(" %s |", m.name)
	}
	fmt.Printf("\n|---|---|")
	for range methods {
		fmt.Printf("---|")
	}
	fmt.Println()
	worst := make([]float64, len(methods))
	worstAt := make([]string, len(methods))
	over := make([]int, len(methods))
	for i, c := range cells {
		fmt.Printf("| %s | %d |", c.name, c.n)
		for k := range methods {
			r := rates[i][k]
			mark := ""
			if null && r > 0.04 {
				mark = " !"
				over[k]++
			}
			if r > worst[k] {
				worst[k], worstAt[k] = r, fmt.Sprintf("%s n=%d", c.name, c.n)
			}
			fmt.Printf(" %s%s |", pct(r), mark)
		}
		fmt.Println()
	}
	if null {
		fmt.Printf("| **worst cell** | |")
		for k := range methods {
			fmt.Printf(" %s |", pct(worst[k]))
		}
		fmt.Printf("\n| **cells over 4%%** (of %d) | |", len(cells))
		for k := range methods {
			fmt.Printf(" %d |", over[k])
		}
		fmt.Println()
		for k, m := range methods {
			fmt.Printf("<!-- worst %s: %s at %s -->\n", m.name, pct(worst[k]), worstAt[k])
		}
	}
}

// envelopeWorst returns, per method, the largest exact false-ADOPT rate of pass/fail pairs whose true mean difference
// is mu0, over the sizes ns and the shares of disagreeing tasks (u + v) in shares.
func envelopeWorst(methods []studyMethod, mu0 float64, ns []int, shares []float64) []struct {
	n    int
	s    float64
	rate float64
} {
	best := make([]struct {
		n    int
		s    float64
		rate float64
	}, len(methods))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for _, n := range ns {
		for _, s := range shares {
			u, v := (s+mu0)/2, (s-mu0)/2
			if u < 0 || v < 0 || u+v > 1 {
				continue
			}
			for k := range methods {
				wg.Add(1)
				sem <- struct{}{}
				go func(n int, s, u, v float64, k int) {
					defer wg.Done()
					r := exactCell("envelope", n, mu0, u, v).rate(methods[k], 0)
					mu.Lock()
					if r > best[k].rate {
						best[k].n, best[k].s, best[k].rate = n, s, r
					}
					mu.Unlock()
					<-sem
				}(n, s, u, v, k)
			}
		}
	}
	wg.Wait()
	return best
}

func runClosedFormStudy() {
	sims := envInt("OFFLOAD_GATE_STUDY_SIMS", 20000)
	only := os.Getenv("OFFLOAD_GATE_STUDY_ONLY")
	keep := func(cs []studyCell) []studyCell {
		var out []studyCell
		for _, c := range cs {
			if only == "" || strings.Contains(c.name, only) {
				out = append(out, c)
			}
		}
		return out
	}
	methods := studyMethods()
	fmt.Printf("\n# closed-form study: pass/fail cells exact, other cells %d datasets, seeds from the cell's name and size\n", sims)
	for _, g := range []struct {
		title string
		cells []studyCell
		null  bool
	}{
		{"false ADOPT rate (%), the earlier 63-cell null grid", keep(existingNullCells()), true},
		{"false ADOPT rate (%), the sparse-regime null grid", keep(sparseNullCells()), true},
		{"false ADOPT rate (%), stress nulls: scores that are not pass/fail, few and coarse disagreements", keep(stressNullCells()), true},
		{"ADOPT rate (%) at a true effect (power)", keep(studyPowerCells()), false},
	} {
		printStudyGrid(g.title, g.cells, methods, runStudyGrid(g.cells, methods, sims), g.null)
	}
	// The envelope: the worst exact rate over every size and every share of disagreeing tasks, pass/fail pairs only.
	nmax := envInt("OFFLOAD_GATE_STUDY_ENV_NMAX", 500)
	var ns []int
	for n := 16; n <= nmax; n++ {
		ns = append(ns, n)
	}
	for _, m := range []float64{0.05, 0.10} {
		shares := []float64{m, m + .005, m + .01, m + .02, m + .03, m + .05, m + .08, m + .12, m + .2, m + .3, m + .45}
		best := envelopeWorst(methods, -m, ns, shares)
		fmt.Printf("\n### worst exact false-ADOPT rate over n = 16..%d and 11 shares of disagreeing tasks, true difference -%.2f\n\n", nmax, m)
		for k, mt := range methods {
			fmt.Printf("- %s: %s%% at n=%d, disagreeing share %.3f\n", mt.name, pct(best[k].rate), best[k].n, best[k].s)
		}
	}
	best := envelopeWorst(methods, 0, ns, []float64{.02, .04, .06, .1, .15, .2, .3, .5, .8})
	fmt.Printf("\n### worst exact false-ADOPT rate over n = 16..%d and 9 shares of disagreeing tasks, superiority, true difference 0\n\n", nmax)
	for k, mt := range methods {
		fmt.Printf("- %s: %s%% at n=%d, disagreeing share %.3f\n", mt.name, pct(best[k].rate), best[k].n, best[k].s)
	}
}

func TestSparseCalibrationStudy(t *testing.T) {
	if os.Getenv("OFFLOAD_GATE_STUDY") == "" {
		t.Skip("set OFFLOAD_GATE_STUDY=1 to run the closed-form calibration grids (minutes of CPU)")
	}
	runClosedFormStudy()
}

// ---- the study's own tests: always on ---------------------------------------------------------------

// The exact sum is the simulation's expectation: a pass/fail cell run both ways agrees to the sampling error, so the
// "exact" column measures the same thing the simulated columns do.
func TestExactSumAgreesWithSimulation(t *testing.T) {
	const sims = 40000
	c := exactCell("agreement", 24, -.05, 0, .05)
	u, v := indep(.9, .85)
	for _, tc := range []studyCell{
		c,
		exactCell("agreement", 40, -.05, u, v),
		exactCell("agreement", 20, 0, .25, .25),
	} {
		m := studyMethod{"production", ivlProduction}
		exact := tc.rate(m, 0)
		sim := tc
		sim.exact = false
		pu, pv := tc.u, tc.v
		sim.gen = func(rng *rand.Rand, n int) ([]float64, []float64) {
			inc, cand := make([]float64, n), make([]float64, n)
			for i := 0; i < n; i++ {
				switch r := rng.Float64(); {
				case r < pu:
					cand[i] = 1
				case r < pu+pv:
					inc[i] = 1
				}
			}
			return inc, cand
		}
		got := sim.rate(m, sims)
		if se := math.Sqrt(math.Max(exact, 0.01) * 1 / sims); math.Abs(got-exact) > 5*se {
			t.Errorf("n=%d mu0=%v: simulated %.4f, exact %.4f (5 standard errors %.4f)", tc.n, tc.mu0, got, exact, 5*se)
		}
	}
}

// The v2 column the study compares with is the version 2 rule: the score form of the t interval, and the rule of three
// for a sample whose differences are all equal.
func TestIntervalV2ReferenceIsTheVersionTwoRule(t *testing.T) {
	near := func(got, want float64) bool { return math.Abs(got-want) <= 1e-12*math.Max(1, math.Abs(want)) }
	d := []float64{0.5, 0, -0.5, 0, 0, 0.5, 0, 0, -0.5, 0, 0, 0, 0.5, 0, -0.5, 0, 0, 0, 0, 0.5}
	if lo, hi := intervalV2(d, 1); !near(lo, -0.13109591433944429) || !near(hi, 0.18109591433944427) {
		t.Errorf("score form, 20 tasks: [%v, %v]", lo, hi)
	}
	if lo, hi := intervalV2(make([]float64, 61), 1); !near(lo, -3.0/61) || !near(hi, 3.0/61) {
		t.Errorf("61 ties are the rule of three: [%v, %v]", lo, hi)
	}
}
