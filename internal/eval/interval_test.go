package eval

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// The reference values below were computed outside the package (numpy and scipy) from the formula in
// interval.go, so they pin the arithmetic and the t table, not the code against itself.
func TestPrimaryIntervalMatchesAnIndependentComputation(t *testing.T) {
	near := func(got, want float64) bool { return math.Abs(got-want) <= 1e-12*math.Max(1, math.Abs(want)) }

	// 20 tasks, differences +-0.5 on 8 of them and 0 on 12: mean 0.025.
	d := []float64{0.5, 0, -0.5, 0, 0, 0.5, 0, 0, -0.5, 0, 0, 0, 0.5, 0, -0.5, 0, 0, 0, 0, 0.5}
	inc, cand := repeatF(0.25, len(d)), make([]float64, len(d))
	for i := range d {
		cand[i] = 0.25 + d[i]
	}
	delta, lo, hi := primaryInterval(inc, cand, 1)
	if !near(delta, 0.025) || !near(lo, -0.13109591433944429) || !near(hi, 0.18109591433944427) {
		t.Fatalf("n=20: got %v %v %v", delta, lo, hi)
	}

	// 16 tasks of continuous scores.
	inc16 := []float64{0.805, 0.808, 0.515, 0.286, 0.054, 0.383, 0.408, 0.045, 0.049, 0.999, 0.652, 0.235, 0.435, 0.974, 0.898, 0.844}
	cand16 := []float64{0.838, 0.625, 0.439, 0.238, 0.0, 0.544, 0.445, 0.0, 0.181, 1.0, 0.373, 0.234, 0.289, 0.989, 0.69, 0.898}
	delta, lo, hi = primaryInterval(inc16, cand16, 1)
	if !near(delta, -0.037937500000000006) || !near(lo, -0.1109080595568126) || !near(hi, 0.03503305955681258) {
		t.Fatalf("n=16: got %v %v %v", delta, lo, hi)
	}
}

// The interval is the confidence set of the self-normalized statistic: at either end the statistic
// (delta - mu) / sqrt(mean((d - mu)^2) / n) is +-c, the critical value of the t distribution.
func TestPrimaryIntervalIsTheScoreFormOfTheTInterval(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for trial := 0; trial < 200; trial++ {
		n := 16 + rng.Intn(60)
		inc, cand := make([]float64, n), make([]float64, n)
		for i := range inc {
			inc[i], cand[i] = rng.Float64(), rng.Float64()*0.9
		}
		delta, lo, hi := primaryInterval(inc, cand, 1)
		c := tCritical975(n - 1)
		stat := func(mu float64) float64 {
			var m2 float64
			for i := range inc {
				e := cand[i] - inc[i] - mu
				m2 += e * e
			}
			return (delta - mu) * math.Sqrt(float64(n)) / math.Sqrt(m2/float64(n))
		}
		if got := stat(lo); math.Abs(got-c) > 1e-9 {
			t.Fatalf("n=%d: the statistic at lo is %v, want c=%v", n, got, c)
		}
		if got := stat(hi); math.Abs(got+c) > 1e-9 {
			t.Fatalf("n=%d: the statistic at hi is %v, want -c=%v", n, got, -c)
		}
		if !(lo < delta && delta < hi) {
			t.Fatalf("n=%d: the interval %v %v must contain the delta %v", n, lo, hi, delta)
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
}

// With no spread to measure the delta is the one value, taken from the data and not from a sum, and the
// interval is the value plus and minus 3 R / n (the rule of three, R the width of the score range), clipped to
// [-R, R] (see zeroSpreadInterval). The reference ends were computed outside the package. With nothing to
// conclude the result is zeros; a sum that overflows is not finite, for the caller to refuse.
func TestPrimaryIntervalDegenerateInputs(t *testing.T) {
	near := func(got, want float64) bool { return math.Abs(got-want) <= 1e-12*math.Max(1, math.Abs(want)) }
	for _, tc := range []struct {
		name      string
		inc, cand []float64
		span      float64
		delta     float64 // the one value
		lo, hi    float64
	}{
		{"every difference zero (a tie), 16 tasks", repeatF(0.5, 16), repeatF(0.5, 16), 1, 0, -0.1875, 0.1875},
		{"a tie, 40 tasks", repeatF(0.5, 40), repeatF(0.5, 40), 1, 0, -0.075, 0.075},
		{"a tie, 60 tasks: 3/60 is the margin 0.05", repeatF(0.5, 60), repeatF(0.5, 60), 1, 0, -0.05, 0.05},
		{"a tie, 80 tasks", repeatF(0.5, 80), repeatF(0.5, 80), 1, 0, -0.0375, 0.0375},
		{"a tie on a 0..10 range, 24 tasks", repeatF(5, 24), repeatF(5, 24), 10, 0, -1.25, 1.25},
		{"every difference +0.25", repeatF(0.5, 16), repeatF(0.75, 16), 1, 0.25, 0.0625, 0.4375},
		{"every difference -0.5 on a 0..10 range", repeatF(8, 16), repeatF(7.5, 16), 10, -0.5, -2.375, 1.375},
		// The mean of a difference cannot leave [-R, R]: the interval stops at the end of the range.
		{"two tasks, the same difference: clipped at both ends", []float64{0.5, 0.25}, []float64{1, 0.75}, 1, 0.5, -1, 1},
		// Sixteen copies of -0.1 sum to -1.6000000000000003, whose sixteenth is not -0.1: the delta
		// must come from the data, not from the sum.
		{"every difference -0.1 (not a sum of floats)", repeatF(0.2, 16), repeatF(0.1, 16), 1, -0.1, -0.2875, 0.0875},
		// The largest possible difference on every task is a win to the end of the range, and it is
		// clipped there.
		{"every candidate score perfect, every incumbent score 0", repeatF(0, 16), repeatF(1, 16), 1, 1, 0.8125, 1},
		{"every candidate score zero, every incumbent score perfect", repeatF(1, 16), repeatF(0, 16), 1, -1, -1, -0.8125},
	} {
		d, lo, hi := primaryInterval(tc.inc, tc.cand, tc.span)
		if d != tc.delta || !near(lo, tc.lo) || !near(hi, tc.hi) {
			t.Errorf("%s: got %v %v %v, want %v %v %v", tc.name, d, lo, hi, tc.delta, tc.lo, tc.hi)
		}
	}
	// Differences that are equal on paper but not to the last bit (0.3-0.2 against 0.2-0.1) are not a
	// spread: they get the interval of a sample with none.
	if d, lo, hi := primaryInterval([]float64{0.2, 0.1, 0.2, 0.1}, []float64{0.3, 0.2, 0.3, 0.2}, 1); !finite(d) || math.Abs(d-0.1) > 1e-9 || math.Abs(lo+0.65) > 1e-8 || math.Abs(hi-0.85) > 1e-8 {
		t.Errorf("near-equal differences: %v %v %v", d, lo, hi)
	}
	// A difference that is not rounding is a spread, however small a share of the range it is.
	if _, lo, hi := primaryInterval(repeatF(0.5, 16), append(repeatF(0.5, 15), 0.51), 1); hi-lo > 0.01 || hi <= 0 {
		t.Errorf("a difference of 0.01 on one task is a spread, and a small one: %v %v", lo, hi)
	}
	// A range that is not a positive number leaves the single value.
	for _, span := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if d, lo, hi := primaryInterval(repeatF(0.5, 16), repeatF(0.25, 16), span); d != -0.25 || lo != d || hi != d {
			t.Errorf("span %v: got %v %v %v, want the single value", span, d, lo, hi)
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
	// The smallest sizes have no room for the form (n <= c^2 + 1): they still get a finite, wider interval.
	for n := 2; n <= 8; n++ {
		inc, cand := make([]float64, n), make([]float64, n)
		for i := range cand {
			cand[i] = float64(i % 2)
		}
		if d, lo, hi := primaryInterval(inc, cand, 1); !finite(lo) || !finite(hi) || !(lo < d && d < hi) {
			t.Errorf("n=%d: %v %v %v", n, d, lo, hi)
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

// The harness's score-form method is the production interval (its lower end), so the figures the study
// prints are the gate's.
func TestSimHarnessScoreFormIsThePrimaryInterval(t *testing.T) {
	buf := newSimBuf(50)
	for s := 0; s < 300; s++ {
		rng := rand.New(rand.NewSource(int64(500 + s)))
		n := 16 + rng.Intn(30)
		inc, cand := genBinary(.8, .75, 1)(rng, n)
		if s%3 == 1 {
			inc, cand = genUniform(0, 1, 0, .9)(rng, n)
		}
		lows, deg := simLowers(inc, cand, 50, 1, buf, 1)
		_, lo, _ := primaryInterval(inc, cand, 1)
		if math.Abs(lows[mScore]-lo) > 1e-9 {
			t.Fatalf("dataset %d (n=%d, degenerate %v): harness %v, production %v", s, n, deg, lows[mScore], lo)
		}
	}
}

// The interval itself, with many more trials than the end-to-end cells can afford (it needs no
// bootstrap, so 50000 datasets take well under a second): the share of simulated comparisons whose lower
// end clears the boundary when the truth is the boundary (0 for superiority, -0.05 for non-inferiority).
// Each cell must stay at or under 4%, the nominal being 2.5%. The plain t interval measures 4.6% on the
// third cell, so this test, unlike the end-to-end ones, also refuses a simplification back to it.
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
// The false-ADOPT rate is the sum of the probabilities of those K. These are the figures the docs and the ADR give
// for the regime the score form is not calibrated in (a strong incumbent and a handful of disagreeing tasks, from
// 61 tasks): the all-tied sample is the rule of three from 61 tasks, and one loss clears the margin from 62, so
// the rate jumps from 0 at 60 to 4.4% at 61 and 17.7% at 62 and falls back as the sample grows until two losses
// clear it. The expected values were computed outside the package (scipy.stats.t.ppf and exact binomial sums).
// At 16 to 60 tasks no K clears the margin, which is why the always-on cells there read 0.0%.
func TestNestedLossFalseAdoptRateIsTheBinomialSum(t *testing.T) {
	const loss, margin = 0.05, 0.05
	for _, tc := range []struct {
		n    int
		want float64
	}{
		{16, 0}, {24, 0}, {40, 0}, {60, 0},
		{61, 0.043766}, {62, 0.177254}, {64, 0.163921}, {70, 0.129208}, {80, 0.086054}, {90, 0.056728},
		{100, 0.118263}, {120, 0.057505}, {160, 0.093854}, {200, 0.062342},
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
	}
}
