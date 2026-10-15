package eval

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// The augmented fallback is the score form of the t interval on the sample with two pseudo-pairs at each end of
// the range, centred on the observed mean: rebuilt here by appending the pseudo-pairs to the differences and
// reading the divisor-N standard deviation of the whole, a different path from the closed form in the code.
func TestPrimaryIntervalOnOtherScoresMatchesAnExplicitlyAugmentedSample(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for trial := 0; trial < 300; trial++ {
		n := 16 + rng.Intn(60)
		inc, cand := make([]float64, n), make([]float64, n)
		for i := range inc {
			inc[i], cand[i] = rng.Float64(), rng.Float64()*0.9
		}
		delta, lo, hi := primaryInterval(inc, cand, 1)
		aug := make([]float64, 0, n+4)
		var mean float64
		for i := range inc {
			aug = append(aug, cand[i]-inc[i])
			mean += cand[i] - inc[i]
		}
		mean /= float64(n)
		aug = append(aug, 1, 1, -1, -1)
		big := float64(len(aug))
		var m float64
		for _, v := range aug {
			m += v
		}
		m /= big
		var ss float64
		for _, v := range aug {
			ss += (v - m) * (v - m)
		}
		c := tCritical975(len(aug) - 1)
		half := c * math.Sqrt(ss/big) / math.Sqrt(big-c*c)
		if math.Abs(delta-mean) > 1e-12 || math.Abs(lo-(mean-half)) > 1e-12 || math.Abs(hi-(mean+half)) > 1e-12 {
			t.Fatalf("n=%d: got %v [%v, %v], want %v [%v, %v]", n, delta, lo, hi, mean, mean-half, mean+half)
		}
		if !(lo < delta && delta < hi) {
			t.Fatalf("n=%d: the interval %v %v must contain the delta %v", n, lo, hi, delta)
		}
	}
}

// Tango's statistic is the one the interval inverts: at either end it is +-1.96, and the interval contains the
// sample's own mean. Checked over many (n, wins, losses), the sparse ones included. An end at the edge of the
// range (all losses, or all wins) has a nil variance there and is the range's end.
func TestTangoIntervalEndsAreWhereTheStatisticIsPlusOrMinusZ(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	for trial := 0; trial < 3000; trial++ {
		n := 2 + rng.Intn(300)
		// Mostly sparse disagreement: the regime this interval exists for.
		var w, l int
		switch trial % 3 {
		case 0:
			w, l = rng.Intn(n/10+1), rng.Intn(n/10+1)
		case 1:
			w, l = rng.Intn(n+1), 0
		default:
			w = rng.Intn(n + 1)
			l = rng.Intn(n - w + 1)
		}
		lo, hi := tangoInterval(n, w, l)
		mean := float64(w-l) / float64(n)
		if !(lo <= mean && mean <= hi) || lo < -1 || hi > 1 {
			t.Fatalf("n=%d w=%d l=%d: [%v, %v] must contain the mean %v and stay in [-1, 1]", n, w, l, lo, hi, mean)
		}
		if lo > -1 {
			if z := tangoZ(n, w, l, lo); math.Abs(z-normal975) > 1e-8 && !(w == n) {
				t.Fatalf("n=%d w=%d l=%d: the statistic at lo=%v is %v, want %v", n, w, l, lo, z, normal975)
			}
		}
		if hi < 1 {
			if z := tangoZ(n, w, l, hi); math.Abs(z+normal975) > 1e-8 && !(l == n) {
				t.Fatalf("n=%d w=%d l=%d: the statistic at hi=%v is %v, want %v", n, w, l, hi, z, -normal975)
			}
		}
	}
}

// Pass/fail pairs that are off by float rounding are still pass/fail pairs: a tie that is 5.6e-17 from 0
// (0.1 + 0.2 against 0.3), a win of the whole range that is a hair short of it, a loss likewise, get the interval of
// exact ties, wins and losses, not the (much wider) one of scores that are not pass/fail.
func TestPassFailPairsOffByRoundingAreStillPassFail(t *testing.T) {
	x, y := 0.1, 0.2 // not constants: a constant 0.1 + 0.2 is exactly 0.3
	const n = 60
	inc, cand := make([]float64, n), make([]float64, n)
	exactInc, exactCand := make([]float64, n), make([]float64, n)
	rounded := 0
	for i := range inc {
		switch {
		case i < 4: // wins
			inc[i], cand[i] = 0.25, math.Nextafter(1.25, 0)
			exactInc[i], exactCand[i] = 0, 1
		case i < 9: // losses
			inc[i], cand[i] = 1.25, math.Nextafter(math.Nextafter(0.25, 1), 1)
			exactInc[i], exactCand[i] = 1, 0
		default: // ties that are not bit-equal
			inc[i], cand[i] = 0.3, x+y
			exactInc[i], exactCand[i] = 0.5, 0.5
		}
		if d := cand[i] - inc[i]; d != 1 && d != -1 && d != 0 {
			rounded++
		}
	}
	if rounded != n {
		t.Fatalf("setup: every pair must be off by rounding, %d of %d are", rounded, n)
	}
	d, lo, hi := primaryInterval(inc, cand, 1)
	wd, wlo, whi := primaryInterval(exactInc, exactCand, 1)
	if math.Abs(d-wd) > 1e-12 || math.Abs(lo-wlo) > 1e-12 || math.Abs(hi-whi) > 1e-12 {
		t.Errorf("pairs off by rounding: %v [%v, %v], the exact pass/fail pairs give %v [%v, %v]", d, lo, hi, wd, wlo, whi)
	}
}

// The variance the statistic uses is the restricted maximum-likelihood one: the likelihood of w wins, l losses and
// the rest ties is maximised over the probability v of a loss under u - v = delta (u the probability of a win),
// by a ternary search of the concave log-likelihood, a path that shares nothing with the quadratic the code solves.
func TestTangoVarianceIsTheRestrictedMaximumLikelihood(t *testing.T) {
	rng := rand.New(rand.NewSource(17))
	for trial := 0; trial < 2000; trial++ {
		n := 2 + rng.Intn(200)
		w := rng.Intn(n + 1)
		l := rng.Intn(n - w + 1)
		if trial%2 == 0 {
			w, l = rng.Intn(n/8+1), rng.Intn(n/8+1)
		}
		delta := -0.6 + 1.2*rng.Float64()
		ties := n - w - l
		loglik := func(v float64) float64 {
			u, z := v+delta, 1-2*v-delta
			var s float64
			for _, c := range []struct {
				cnt int
				p   float64
			}{{w, u}, {l, v}, {ties, z}} {
				if c.cnt > 0 {
					if c.p <= 0 {
						return math.Inf(-1)
					}
					s += float64(c.cnt) * math.Log(c.p)
				}
			}
			return s
		}
		lo, hi := math.Max(0, -delta), (1-delta)/2
		for i := 0; i < 300; i++ {
			a, b := lo+(hi-lo)/3, hi-(hi-lo)/3
			if loglik(a) < loglik(b) {
				lo = a
			} else {
				hi = b
			}
		}
		// The search converges to the interior optimum or to the edge u = 0 (v = -delta), whichever the likelihood prefers.
		v := (lo + hi) / 2
		if edge := math.Max(0, -delta); loglik(edge) > loglik(v) {
			v = edge
		}
		want := 2*v + delta - delta*delta
		if got := tangoVar0(n, w, l, delta); math.Abs(got-want) > 1e-7 {
			t.Fatalf("n=%d w=%d l=%d delta=%v: variance %v, the maximum-likelihood one is %v", n, w, l, delta, got, want)
		}
	}
}

// Shifting both arms moves nothing, scaling them scales the interval, and swapping the arms negates it.
func TestPrimaryIntervalInvariances(t *testing.T) {
	rng := rand.New(rand.NewSource(12))
	n := 24
	inc, cand := make([]float64, n), make([]float64, n)
	for i := range inc {
		inc[i], cand[i] = float64(rng.Intn(11))/10, float64(rng.Intn(11))/10
	}
	near := func(a, b float64) bool { return math.Abs(a-b) <= 1e-12*math.Max(1, math.Abs(b)) }
	d0, lo0, hi0 := primaryInterval(inc, cand, 1)

	shifted, shiftedC := plus(inc, 3.5), plus(cand, 3.5)
	d, lo, hi := primaryInterval(shifted, shiftedC, 1)
	if !near(d, d0) || !near(lo, lo0) || !near(hi, hi0) {
		t.Errorf("shifting both arms moved the interval: %v %v %v vs %v %v %v", d, lo, hi, d0, lo0, hi0)
	}
	scale := func(v []float64, k float64) []float64 {
		out := make([]float64, len(v))
		for i := range v {
			out[i] = v[i] * k
		}
		return out
	}
	d, lo, hi = primaryInterval(scale(inc, 10), scale(cand, 10), 10)
	if !near(d, 10*d0) || !near(lo, 10*lo0) || !near(hi, 10*hi0) {
		t.Errorf("a 0..10 scale must give ten times the 0..1 interval: %v %v %v vs %v %v %v", d, lo, hi, 10*d0, 10*lo0, 10*hi0)
	}
	d, lo, hi = primaryInterval(cand, inc, 1)
	if !near(d, -d0) || !near(lo, -hi0) || !near(hi, -lo0) {
		t.Errorf("swapping the arms must negate the interval: %v %v %v vs %v %v %v", d, lo, hi, -d0, -hi0, -lo0)
	}
	// The same for pass/fail pairs (Tango's interval): shifting by a constant that keeps them pass/fail is the
	// identity, the range scales the interval, and swapping the arms negates it.
	pinc, pcand := make([]float64, 40), make([]float64, 40)
	for i := range pinc {
		pinc[i], pcand[i] = float64(rng.Intn(2)), float64(rng.Intn(2))
	}
	d0, lo0, hi0 = primaryInterval(pinc, pcand, 1)
	d, lo, hi = primaryInterval(scale(pinc, 10), scale(pcand, 10), 10)
	if !near(d, 10*d0) || !near(lo, 10*lo0) || !near(hi, 10*hi0) {
		t.Errorf("pass/fail on a 0..10 scale must give ten times the 0..1 interval: %v %v %v vs %v %v %v", d, lo, hi, 10*d0, 10*lo0, 10*hi0)
	}
	d, lo, hi = primaryInterval(pcand, pinc, 1)
	if !near(d, -d0) || !near(lo, -hi0) || !near(hi, -lo0) {
		t.Errorf("pass/fail: swapping the arms must negate the interval: %v %v %v vs %v %v %v", d, lo, hi, -d0, -hi0, -lo0)
	}
}

// Samples with no spread, and the edges. A sample of ties, wins and losses of the whole range has no standard
// error to read, and its interval is Tango's: with no pair that differs it is +-z^2 / (n + z^2) of the range
// (computed here from the formula). Any other constant difference is not pass/fail data, and the pseudo-pairs give
// it a spread of its own; the reference ends of those were computed outside the package. With nothing to
// conclude the result is zeros; a sum that overflows is not finite, for the caller to refuse; a range that is not
// a positive number has no pseudo-pair to scale, so no interval.
func TestPrimaryIntervalDegenerateInputs(t *testing.T) {
	const z2 = 1.959963984540054 * 1.959963984540054
	near := func(got, want float64) bool { return math.Abs(got-want) <= 1e-9*math.Max(1, math.Abs(want)) }
	tie := func(n int, span float64) float64 { return span * z2 / (float64(n) + z2) }
	for _, tc := range []struct {
		name          string
		inc, cand     []float64
		span          float64
		delta, lo, hi float64
	}{
		{"every difference zero (a tie), 16 tasks", repeatF(0.5, 16), repeatF(0.5, 16), 1, 0, -tie(16, 1), tie(16, 1)},
		{"a tie, 40 tasks", repeatF(0.5, 40), repeatF(0.5, 40), 1, 0, -tie(40, 1), tie(40, 1)},
		{"a tie, 73 tasks: the end is -0.04999", repeatF(0.5, 73), repeatF(0.5, 73), 1, 0, -tie(73, 1), tie(73, 1)},
		{"a tie on a 0..10 range, 24 tasks", repeatF(5, 24), repeatF(5, 24), 10, 0, -tie(24, 10), tie(24, 10)},
		{"every candidate score perfect, every incumbent score 0", repeatF(0, 16), repeatF(1, 16), 1, 1, 0.612784638931127, 1},
		{"every candidate score zero, every incumbent score perfect", repeatF(1, 16), repeatF(0, 16), 1, -1, -1, -0.612784638931127},
		// Not pass/fail: the pseudo-pairs spread the interval about the one value.
		{"every difference +0.25", repeatF(0.5, 16), repeatF(0.75, 16), 1, 0.25, 0.007308942236025517, 0.4926910577639745},
		{"every difference -0.5 on a 0..10 range", repeatF(8, 16), repeatF(7.5, 16), 10, -0.5, -2.8707894489395795, 1.8707894489395795},
		// Sixteen copies of -0.1 sum to -1.6000000000000003 (the sum's rounding is not a spread): the delta is -0.1 to 1e-12.
		{"every difference -0.1 (not a sum of floats)", repeatF(0.2, 16), repeatF(0.1, 16), 1, -0.1, -0.3377877026632221, 0.13778770266322207},
		// The mean of a difference cannot leave [-R, R]: the interval stops at the end of the range.
		{"two tasks, the same difference: clipped at both ends", []float64{0.5, 0.25}, []float64{1, 0.75}, 1, 0.5, -1, 1},
	} {
		d, lo, hi := primaryInterval(tc.inc, tc.cand, tc.span)
		if math.Abs(d-tc.delta) > 1e-12 || !near(lo, tc.lo) || !near(hi, tc.hi) {
			t.Errorf("%s: got %v %v %v, want %v %v %v", tc.name, d, lo, hi, tc.delta, tc.lo, tc.hi)
		}
	}
	// Differences that are equal on paper but not to the last bit (0.3-0.2 against 0.2-0.1) are the same
	// difference: the interval is the one of exactly equal differences, to within the rounding.
	_, wantLo, wantHi := primaryInterval(repeatF(0.2, 4), repeatF(0.3, 4), 1)
	if d, lo, hi := primaryInterval([]float64{0.2, 0.1, 0.2, 0.1}, []float64{0.3, 0.2, 0.3, 0.2}, 1); !finite(d) || math.Abs(d-0.1) > 1e-9 || math.Abs(lo-wantLo) > 1e-9 || math.Abs(hi-wantHi) > 1e-9 {
		t.Errorf("near-equal differences: %v %v %v, want [%v, %v]", d, lo, hi, wantLo, wantHi)
	}
	// A difference that is not rounding is not a tie, however small a share of the range it is: sixteen pairs, fifteen
	// of them ties and one of 0.01, are not pass/fail data, and their interval is the augmented one (wider than the
	// interval of sixteen ties, which is Tango's: the price of the two constructions meeting, stated in the docs).
	if d, lo, hi := primaryInterval(repeatF(0.5, 16), append(repeatF(0.5, 15), 0.51), 1); math.Abs(d-0.000625) > 1e-12 || !near(lo, -0.23622003346049442) || !near(hi, 0.2374700334604944) {
		t.Errorf("a difference of 0.01 on one task: %v %v %v", d, lo, hi)
	}
	// A range that is not a positive number has no interval.
	for _, span := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if d, lo, hi := primaryInterval(repeatF(0.5, 16), repeatF(0.25, 16), span); d != -0.25 || finite(lo) || finite(hi) {
			t.Errorf("span %v: got %v %v %v, want the delta and no interval", span, d, lo, hi)
		}
	}
	for _, tc := range []struct {
		name      string
		inc, cand []float64
	}{
		{"no pair", nil, nil},
		{"one pair", []float64{1}, []float64{0}},
		{"unaligned", []float64{1, 2, 3}, []float64{1, 2}},
	} {
		if d, lo, hi := primaryInterval(tc.inc, tc.cand, 1); d != 0 || lo != 0 || hi != 0 {
			t.Errorf("%s: nothing to conclude is zeros, got %v %v %v", tc.name, d, lo, hi)
		}
	}
	big := repeatF(1.7e308, 16)
	if d, lo, hi := primaryInterval(repeatF(0, 16), big, 1); finite(d) && finite(lo) && finite(hi) {
		t.Errorf("a sum that overflows must not come back finite: %v %v %v", d, lo, hi)
	}
	spread := repeatF(0, 16)
	spread[3] = 1e200
	if _, lo, hi := primaryInterval(repeatF(0, 16), spread, 1); finite(lo) && finite(hi) {
		t.Errorf("a sum of squares that overflows must not come back finite: %v %v", lo, hi)
	}
	// The smallest sizes have no room for the form (N <= c^2 + 1): they still get a finite, wider interval, for
	// pass/fail pairs (Tango's) and for scores that are not.
	for n := 2; n <= 8; n++ {
		inc, cand := make([]float64, n), make([]float64, n)
		for i := range cand {
			cand[i] = float64(i % 2)
		}
		if d, lo, hi := primaryInterval(inc, cand, 1); !finite(lo) || !finite(hi) || !(lo < d && d < hi) {
			t.Errorf("n=%d, pass/fail: %v %v %v", n, d, lo, hi)
		}
		for i := range cand {
			cand[i] = 0.5 * float64(i%2)
		}
		if d, lo, hi := primaryInterval(inc, cand, 1); !finite(lo) || !finite(hi) || !(lo < d && d < hi) {
			t.Errorf("n=%d, half-credit: %v %v %v", n, d, lo, hi)
		}
	}
}

// The critical values against scipy.stats.t.ppf(0.975, df), and the table against the bisection the
// harness uses for any level.
func TestTCritical975(t *testing.T) {
	for _, tc := range []struct {
		df   int
		want float64
		tol  float64
	}{
		{1, 12.706204736174694, 1e-12},
		{2, 4.302652729749462, 1e-12},
		{5, 2.5705818356363146, 1e-12},
		{15, 2.1314495455597755, 1e-12},
		{19, 2.0930240544083087, 1e-12},
		{23, 2.0686576104190486, 1e-12},
		{39, 2.022690920036761, 1e-12},
		{79, 1.9904502102301285, 1e-12},
		{99, 1.984216951586417, 1e-12},
		{199, 1.9719565442517533, 1e-12},
		{200, 1.9718962236339088, 1e-12},
		{201, 1.971836506779859, 2e-9}, // the first df past the table: the expansion
		{260, 1.9691300033601737, 2e-9},
		{999, 1.9623414611334493, 1e-9},
	} {
		if got := tCritical975(tc.df); math.Abs(got-tc.want) > tc.tol {
			t.Errorf("tCritical975(%d) = %.15f, want %.15f", tc.df, got, tc.want)
		}
	}
	for df := 1; df <= 260; df++ {
		if got, ref := tCritical975(df), tQuantile(0.975, float64(df)); math.Abs(got-ref) > 2e-9 {
			t.Errorf("tCritical975(%d) = %.12f, the bisection gives %.12f", df, got, ref)
		}
	}
	// The values fall with df, to the normal point from above, and df < 1 is held at 1.
	prev := math.Inf(1)
	for df := 1; df <= 5000; df += 7 {
		v := tCritical975(df)
		if !(v < prev) || v < 1.959963984540054 {
			t.Fatalf("df=%d: %v after %v is not a decreasing sequence above the normal point", df, v, prev)
		}
		prev = v
	}
	if tCritical975(0) != tCritical975(1) || tCritical975(-3) != tCritical975(1) {
		t.Error("df below 1 is held at 1")
	}
}

// The harness's production column is the production interval (its lower end), so the figures the study prints are
// the gate's, and its score-form column is the interval gate version 2 read, kept as the column the study compares with.
func TestSimHarnessProductionColumnIsThePrimaryInterval(t *testing.T) {
	buf := newSimBuf(50)
	for s := 0; s < 300; s++ {
		rng := rand.New(rand.NewSource(int64(500 + s)))
		n := 16 + rng.Intn(30)
		inc, cand := genBinary(.8, .75, 1)(rng, n)
		if s%3 == 1 {
			inc, cand = genUniform(0, 1, 0, .9)(rng, n)
		}
		if s%3 == 2 {
			inc, cand = genNested(.9, .05/.9)(rng, n)
		}
		lows, deg := simLowers(inc, cand, 50, 1, buf, 1)
		_, lo, _ := primaryInterval(inc, cand, 1)
		if lows[mProd] != lo {
			t.Fatalf("dataset %d (n=%d, degenerate %v): harness %v, production %v", s, n, deg, lows[mProd], lo)
		}
		d := make([]float64, n)
		for i := range d {
			d[i] = cand[i] - inc[i]
		}
		if v2, _ := intervalV2(d, 1); math.Abs(lows[mScore]-v2) > 1e-9 {
			t.Fatalf("dataset %d (n=%d, degenerate %v): the harness's version 2 column %v, the reference %v", s, n, deg, lows[mScore], v2)
		}
	}
}

// The interval itself, with many more trials than the end-to-end cells can afford (it needs no
// bootstrap, so 50000 datasets take well under a second): the share of simulated comparisons whose lower
// end clears the boundary when the truth is the boundary (0 for superiority, -0.05 for non-inferiority).
// Each cell must stay at or under 4%, the nominal being 2.5%. The plain t interval measures 4.6% on the
// third cell, so this test, unlike the end-to-end ones, also refuses a simplification back to it. The binomial
// sampling error of a rate p over 50000 datasets is sqrt(p (1 - p) / 50000): 0.07 points at 2.5%.
func TestPrimaryIntervalFalseAdoptRateOnTheStatedNulls(t *testing.T) {
	const trials = 50000
	for _, tc := range []struct {
		name string
		n    int
		mu0  float64
		gen  pairGen
	}{
		{"superiority, pass/fail p=.5", 16, 0, genBinary(.5, .5, 1)},
		{"superiority, x^3-skewed scores", 16, 0, genCubed(1)},
		{"non-inferiority at exactly -margin, pass rates .90 and .85", 20, -0.05, genBinary(.90, .85, 1)},
		{"non-inferiority at exactly -margin, pass rates .90 and .85", 16, -0.05, genBinary(.90, .85, 1)},
		{"non-inferiority at exactly -margin, pass rates .90 and .85", 24, -0.05, genBinary(.90, .85, 1)},
		{"non-inferiority at exactly -margin, pass rates .70 and .65", 24, -0.05, genBinary(.70, .65, 1)},
		{"non-inferiority at exactly -margin, uniform scores", 24, -0.05, genUniform(0, 1, 0, .9)},
		{"non-inferiority at exactly -margin, x^3-skewed scores", 16, -0.05, genCubed(.8)},
		{"non-inferiority at exactly -margin, pass rates .90 and .85", 80, -0.05, genBinary(.90, .85, 1)},
		// A strong incumbent, where most samples have few or no pairs that differ.
		{"non-inferiority at exactly -margin, pass rates .95 and .90", 16, -0.05, genBinary(.95, .90, 1)},
		{"non-inferiority at exactly -margin, pass rates .95 and .90", 24, -0.05, genBinary(.95, .90, 1)},
		{"non-inferiority at exactly -margin, pass rates .99 and .94", 16, -0.05, genBinary(.99, .94, 1)},
		{"non-inferiority at exactly -margin, pass rates .99 and .94", 24, -0.05, genBinary(.99, .94, 1)},
		{"non-inferiority at exactly -margin, a candidate that only loses 5% of the incumbent's passes", 20, -0.05, genNested(.9, .05/.9)},
		{"non-inferiority at exactly -margin, a candidate that only loses 5% of the incumbent's passes", 40, -0.05, genNested(.9, .05/.9)},
	} {
		t.Run(fmt.Sprintf("%s, n=%d", tc.name, tc.n), func(t *testing.T) {
			rng := rand.New(rand.NewSource(cellSeed(tc.name, tc.n)))
			adopt := 0
			for s := 0; s < trials; s++ {
				inc, cand := tc.gen(rng, tc.n)
				if _, lo, _ := primaryInterval(inc, cand, 1); lo > tc.mu0 {
					adopt++
				}
			}
			rate := float64(adopt) / trials
			t.Logf("false ADOPT rate over %d datasets: %.4f", trials, rate)
			if rate > 0.04 {
				t.Fatalf("false ADOPT rate %.4f is above 0.04", rate)
			}
		})
	}
}

// The sparse regime, exactly, with no simulation. A candidate that only ever loses tasks (every non-zero difference
// is -1) and loses 5% of them sits exactly on a margin of 0.05: the number of losses K in n tasks is
// binomial(n, 0.05), and the gate adopts when the interval of K losses and n - K ties has its lower end above -0.05.
// The false-ADOPT rate is the sum of the probabilities of those K. With Tango's interval that is the lower tail of
// a binomial proportion read with its null variance: no K clears the margin before 73 tasks (the chance of 73 ties at
// a true loss rate of 5% is 2.4%), and from there the rate sits between 0.6% and 2.4%, a sawtooth of the binomial's
// discreteness. The version 2 interval read the same cells at 0 up to 60 tasks, 4.4% at 61, 17.7% at 62, 16.4% at
// 64, 8.6% at 80, 11.8% at 100, 6.5% at 250, 5.6% at 500 and 4.3% at 1000. The expected values were computed outside
// the package (scipy: the restricted maximum-likelihood estimate by numerical maximisation, exact binomial sums).
func TestNestedLossFalseAdoptRateIsTheBinomialSum(t *testing.T) {
	const loss, margin = 0.05, 0.05
	for _, tc := range []struct {
		n    int
		want float64
	}{
		{16, 0}, {24, 0}, {40, 0}, {60, 0}, {61, 0}, {62, 0}, {64, 0}, {70, 0}, {72, 0},
		{73, 0.023650}, {74, 0.022467}, {80, 0.016515}, {90, 0.009888}, {100, 0.005921}, {120, 0.015527},
		{160, 0.012180}, {200, 0.009048}, {250, 0.013086}, {500, 0.019858}, {1000, 0.021155},
	} {
		var rate float64
		for k := 0; k <= tc.n; k++ {
			inc, cand := repeatF(1, tc.n), repeatF(1, tc.n)
			for i := 0; i < k; i++ {
				cand[i] = 0
			}
			if _, lo, _ := primaryInterval(inc, cand, 1); lo > -margin+equalTolerance {
				lg := func(x float64) float64 { v, _ := math.Lgamma(x); return v }
				logP := lg(float64(tc.n+1)) - lg(float64(k+1)) - lg(float64(tc.n-k+1)) +
					float64(k)*math.Log(loss) + float64(tc.n-k)*math.Log(1-loss)
				rate += math.Exp(logP)
			}
		}
		if math.Abs(rate-tc.want) > 2e-6 {
			t.Errorf("n=%d: the false-ADOPT rate of a candidate that loses 5%% of the tasks is %.6f, want %.6f", tc.n, rate, tc.want)
		}
		if rate > 0.04 {
			t.Errorf("n=%d: the false-ADOPT rate %.6f is above 0.04", tc.n, rate)
		}
	}
}
