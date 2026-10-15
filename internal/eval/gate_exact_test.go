package eval

// Exact reads of the primary interval on pass/fail pairs.
//
// A pass/fail pair is a win (+R), a loss (-R) or a tie (0), so a dataset of n pairs is three counts
// (x wins, y losses, n - x - y ties) and anything the interval does with a dataset is a function of
// those counts. The false-ADOPT rate of the gate on a null whose win and loss probabilities are u and v is
// then the multinomial sum of the probabilities of the (x, y) whose interval clears the boundary: exact,
// with no sampling error, and cheap enough to run on every test run.

import (
	"math"
	"testing"
)

// trinaryMass calls visit(x, y, p) for every (wins, losses) whose multinomial probability p is at least
// cut, the ties being n - x - y. It returns the probability mass it did not visit (every (x, y) below the
// cut), so a caller can state the error of the sum instead of discarding it.
func trinaryMass(n int, u, v, cut float64, visit func(x, y int, p float64)) (skipped float64) {
	w := 1 - u - v
	lg := func(k int) float64 { r, _ := math.Lgamma(float64(k) + 1); return r }
	ln := func(p float64) float64 {
		if p <= 0 {
			return math.Inf(-1)
		}
		return math.Log(p)
	}
	lu, lv, lw := ln(u), ln(v), ln(w)
	for x := 0; x <= n; x++ {
		if x > 0 && u <= 0 {
			break
		}
		for y := 0; x+y <= n; y++ {
			if y > 0 && v <= 0 {
				break
			}
			z := n - x - y
			if z > 0 && w <= 0 {
				continue
			}
			lp := lg(n) - lg(x) - lg(y) - lg(z)
			if x > 0 {
				lp += float64(x) * lu
			}
			if y > 0 {
				lp += float64(y) * lv
			}
			if z > 0 {
				lp += float64(z) * lw
			}
			p := math.Exp(lp)
			if p < cut {
				skipped += p
				continue
			}
			visit(x, y, p)
		}
	}
	return skipped
}

// trinaryData builds the n paired differences of x wins and y losses (on a score range of width span) and
// the rest ties.
func trinaryData(n, x, y int, span float64) []float64 {
	d := make([]float64, n)
	for i := 0; i < x; i++ {
		d[i] = span
	}
	for i := x; i < x+y; i++ {
		d[i] = -span
	}
	return d
}

// exactAdoptRate is the exact probability that the lower end of primaryInterval clears mu0 (to within the
// gate's tolerance) on n pass/fail pairs whose probabilities of a win and of a loss are u and v. The mass
// of the (x, y) below 1e-14 is not visited; it is returned too, and is under 1e-9 in every cell below.
func exactAdoptRate(n int, u, v, mu0, span float64) (rate, skipped float64) {
	zero := make([]float64, n)
	skipped = trinaryMass(n, u, v, 1e-14, func(x, y int, p float64) {
		if _, lo, _ := primaryInterval(zero, trinaryData(n, x, y, span), span); lo > mu0+equalTolerance*span {
			rate += p
		}
	})
	return rate, skipped
}

// indep returns the win and loss probabilities of independent pass/fail arms with pass rates pi and pc.
func indep(pi, pc float64) (u, v float64) { return (1 - pi) * pc, pi * (1 - pc) }

// The false-ADOPT rate of the gate's primary on pass/fail data, exactly, at the sizes where the sparse
// regime lives. Each row is a null whose true difference is the boundary (0, or -margin): a candidate that
// only ever loses tasks and loses 5% of them (every non-zero difference is -1), and independent arms with a
// strong incumbent (pass rates .99 and .94, .97 and .92), at a margin of 0.05; the same shapes moved to sit at
// a margin of 0.10; and three of the dense cells for reference. The figures were computed outside the package
// (scipy: the restricted maximum-likelihood estimate by numerical maximisation of the multinomial likelihood,
// no closed form, and the multinomial sums), so the expected values do not come from the code under test.
// Every row must be at most 4% (the nominal being 2.5%), and the version 2 interval, which read these cells at
// 4.4% (61 tasks), 17.7% (62), 16.4% (64), 11.8% (100) and 6.5% (250) for the first shape, fails the test.
func TestPassFailFalseAdoptRateIsExactlyAtMostFourPercentWhereTheArmsBarelyDisagree(t *testing.T) {
	type row struct {
		shape string
		n     int
		u, v  float64
		mu0   float64
		want  float64
	}
	var rows []row
	add := func(shape string, n int, u, v, mu0, want float64) {
		rows = append(rows, row{shape, n, u, v, mu0, want})
	}
	u1, v1 := indep(.99, .94)
	u2, v2 := indep(.97, .92)
	for _, c := range []struct {
		n            int
		nested, a, b float64
	}{
		{61, 0, .013045, .019288}, {62, 0, .012516, .018573}, {64, 0, .011520, .017287},
		{100, .005921, .014502, .018929}, {250, .013086, .021872, .023073},
	} {
		add("nested loss 5%", c.n, 0, .05, -.05, c.nested)
		add("pass rates .99 and .94", c.n, u1, v1, -.05, c.a)
		add("pass rates .97 and .92", c.n, u2, v2, -.05, c.b)
	}
	u3, v3 := indep(.99, .89)
	u4, v4 := indep(.97, .87)
	for _, c := range []struct {
		n            int
		nested, a, b float64
	}{
		{31, 0, .007850, .013642}, {32, 0, .007221, .012551}, {40, .014781, .012811, .015165}, {80, .010684, .015563, .019594},
	} {
		add("margin 0.10, nested loss 10%", c.n, 0, .10, -.10, c.nested)
		add("margin 0.10, pass rates .99 and .89", c.n, u3, v3, -.10, c.a)
		add("margin 0.10, pass rates .97 and .87", c.n, u4, v4, -.10, c.b)
	}
	u5, v5 := indep(.90, .85)
	add("superiority, p=.50", 16, .25, .25, 0, .021587)
	add("superiority, p=.50", 24, .25, .25, 0, .021507)
	add("pass rates .90 and .85", 20, u5, v5, -.05, .023149)
	for _, r := range rows {
		got, skipped := exactAdoptRate(r.n, r.u, r.v, r.mu0, 1)
		if skipped > 1e-9 {
			t.Errorf("%s, n=%d: the sum skipped %g of the probability", r.shape, r.n, skipped)
		}
		if got > 0.04 {
			t.Errorf("%s, n=%d: false-ADOPT rate %.6f is above 0.04", r.shape, r.n, got)
		}
		if math.Abs(got-r.want) > 2e-6 {
			t.Errorf("%s, n=%d: false-ADOPT rate %.6f, the independent computation gives %.6f", r.shape, r.n, got, r.want)
		}
	}
}

// The interval of pass/fail pairs is Tango's score interval for a paired difference, with the reference ends
// computed outside the package from the likelihood by numerical maximisation (scipy) and the statistic
// inverted by Brent's method; the ends of the same counts on a 0..10 range are ten times as wide. Nothing
// is a sum of floats: the delta is (wins - losses) / n of the range.
func TestPrimaryIntervalOnPassFailPairsIsTangosScoreInterval(t *testing.T) {
	for _, tc := range []struct {
		n, x, y int
		lo, hi  float64
	}{
		{16, 0, 0, -0.19360768053443647, 0.19360768053443647},
		{24, 1, 3, -0.28039896719845026, 0.1060026119517067},
		{61, 0, 0, -0.05924386789811283, 0.05924386789811283},
		{72, 0, 0, -0.050651172596457815, 0.050651172596457815},
		{73, 0, 0, -0.0499920079557311, 0.0499920079557311},
		{100, 2, 9, -0.14588916412747804, -0.005955714451517082},
		{20, 3, 3, -0.25789623700772873, 0.2578962359734034},
		{40, 5, 0, 0.0264256975086979, 0.26112119838851094},
		{16, 0, 16, -1.0, -0.612784638931127},
		{16, 16, 0, 0.612784638931127, 1.0},
		{62, 0, 1, -0.08586720260689992, 0.04315607310178111},
		{250, 0, 5, -0.045955643131568594, -0.004564033767815265},
		{250, 3, 12, -0.07171096003628373, -0.006460043861607652},
		{500, 10, 70, -0.15541455028630236, -0.08788585561167905},
		{30, 30, 0, 0.7729732136520625, 1.0},
		{40, 0, 7, -0.3194999033178771, -0.07204461630195888},
		{61, 1, 1, -0.073056014100728, 0.07305601449943849},
	} {
		for _, span := range []float64{1, 10} {
			d, lo, hi := primaryInterval(make([]float64, tc.n), trinaryData(tc.n, tc.x, tc.y, span), span)
			if want := float64(tc.x-tc.y) * span / float64(tc.n); d != want {
				t.Errorf("n=%d x=%d y=%d span %v: delta %v, want %v", tc.n, tc.x, tc.y, span, d, want)
			}
			if tol := 5e-8 * span; math.Abs(lo-tc.lo*span) > tol || math.Abs(hi-tc.hi*span) > tol {
				t.Errorf("n=%d x=%d y=%d span %v: got [%v, %v], want [%v, %v]", tc.n, tc.x, tc.y, span, lo, hi, tc.lo*span, tc.hi*span)
			}
		}
	}
}

// Any other pair of scores gets the score form of the t interval on the sample with one pseudo-pair at each
// end of the difference range added, centred on the observed mean and clipped to the range. The reference
// ends were computed outside the package (numpy and scipy) from that sentence.
func TestPrimaryIntervalOnOtherScoresIsTheAugmentedScoreForm(t *testing.T) {
	near := func(got, want float64) bool { return math.Abs(got-want) <= 1e-12*math.Max(1, math.Abs(want)) }
	d20 := []float64{0.5, 0, -0.5, 0, 0, 0.5, 0, 0, -0.5, 0, 0, 0, 0.5, 0, -0.5, 0, 0, 0, 0, 0.5}
	inc16 := []float64{0.805, 0.808, 0.515, 0.286, 0.054, 0.383, 0.408, 0.045, 0.049, 0.999, 0.652, 0.235, 0.435, 0.974, 0.898, 0.844}
	cand16 := []float64{0.838, 0.625, 0.439, 0.238, 0.0, 0.544, 0.445, 0.0, 0.181, 1.0, 0.373, 0.234, 0.289, 0.989, 0.69, 0.898}
	dp := append(append(repeatF(-1, 3), repeatF(-0.5, 2)...), repeatF(0, 55)...)
	for _, tc := range []struct {
		name          string
		inc, cand     []float64
		span          float64
		delta, lo, hi float64
	}{
		{"20 tasks, +-0.5 on 8 of them", repeatF(0.25, 20), plus(d20, 0.25), 1, 0.025, -0.17891338182693925, 0.22891338182693924},
		{"16 tasks of continuous scores", inc16, cand16, 1, -0.037937500000000006, -0.23911645349106925, 0.16324145349106922},
		{"every difference +0.25", repeatF(0.5, 16), repeatF(0.75, 16), 1, 0.25, 0.05370224917831759, 0.44629775082168244},
		{"every difference -0.5 on a 0..10 range", repeatF(8, 16), repeatF(7.5, 16), 10, -0.5, -2.4127437414364654, 1.4127437414364654},
		{"every difference -0.05, 80 tasks", repeatF(0.95, 80), repeatF(0.90, 80), 1, -0.05, -0.08521760704790185, -0.014782392952098149},
		{"two tasks, the same difference: clipped at both ends", []float64{0.5, 0.25}, []float64{1, 0.75}, 1, 0.5, -1, 1},
		{"3 losses, 2 half-credit losses and 55 ties", repeatF(0, 60), dp, 1, -0.06666666666666667, -0.143011316633242, 0.009677983299908696},
	} {
		d, lo, hi := primaryInterval(tc.inc, tc.cand, tc.span)
		if math.Abs(d-tc.delta) > 1e-12 || !near(lo, tc.lo) || !near(hi, tc.hi) {
			t.Errorf("%s: got %v %v %v, want %v %v %v", tc.name, d, lo, hi, tc.delta, tc.lo, tc.hi)
		}
	}
}
