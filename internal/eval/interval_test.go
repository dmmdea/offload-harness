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
	delta, lo, hi := primaryInterval(inc, cand)
	if !near(delta, 0.025) || !near(lo, -0.13109591433944429) || !near(hi, 0.18109591433944427) {
		t.Fatalf("n=20: got %v %v %v", delta, lo, hi)
	}

	// 16 tasks of continuous scores.
	inc16 := []float64{0.805, 0.808, 0.515, 0.286, 0.054, 0.383, 0.408, 0.045, 0.049, 0.999, 0.652, 0.235, 0.435, 0.974, 0.898, 0.844}
	cand16 := []float64{0.838, 0.625, 0.439, 0.238, 0.0, 0.544, 0.445, 0.0, 0.181, 1.0, 0.373, 0.234, 0.289, 0.989, 0.69, 0.898}
	delta, lo, hi = primaryInterval(inc16, cand16)
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
		delta, lo, hi := primaryInterval(inc, cand)
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
	d0, lo0, hi0 := primaryInterval(inc, cand)

	shifted, shiftedC := plus(inc, 3.5), plus(cand, 3.5)
	d, lo, hi := primaryInterval(shifted, shiftedC)
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
	d, lo, hi = primaryInterval(scale(inc, 10), scale(cand, 10))
	if !near(d, 10*d0) || !near(lo, 10*lo0) || !near(hi, 10*hi0) {
		t.Errorf("a 0..10 scale must give ten times the 0..1 interval: %v %v %v vs %v %v %v", d, lo, hi, 10*d0, 10*lo0, 10*hi0)
	}
	d, lo, hi = primaryInterval(cand, inc)
	if !near(d, -d0) || !near(lo, -hi0) || !near(hi, -lo0) {
		t.Errorf("swapping the arms must negate the interval: %v %v %v vs %v %v %v", d, lo, hi, -d0, -hi0, -lo0)
	}
}

// With no spread to measure the interval is its one value; with nothing to conclude it is zeros; a sum
// that overflows is not finite, for the caller to refuse.
func TestPrimaryIntervalDegenerateInputs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inc, cand []float64
		want      float64
	}{
		{"every difference zero (a tie)", repeatF(0.5, 16), repeatF(0.5, 16), 0},
		{"every difference +0.25", repeatF(0.5, 16), repeatF(0.75, 16), 0.25},
		{"every difference -0.5", repeatF(8, 16), repeatF(7.5, 16), -0.5},
		{"two tasks, the same difference", []float64{1, 2}, []float64{1.5, 2.5}, 0.5},
		// Sixteen copies of -0.1 sum to -1.6000000000000003, whose sixteenth is not -0.1: the single
		// value must come from the data, not from the sum.
		{"every difference -0.1 (not a sum of floats)", repeatF(0.2, 16), repeatF(0.1, 16), -0.1},
	} {
		d, lo, hi := primaryInterval(tc.inc, tc.cand)
		if d != tc.want || lo != tc.want || hi != tc.want {
			t.Errorf("%s: got %v %v %v, want the single value %v", tc.name, d, lo, hi, tc.want)
		}
	}
	// Differences that are equal on paper but not to the last bit (0.3-0.2 against 0.2-0.1) have a spread
	// of about 1e-17: the interval is finite and about as wide as that.
	if d, lo, hi := primaryInterval([]float64{0.2, 0.1, 0.2, 0.1}, []float64{0.3, 0.2, 0.3, 0.2}); !finite(d) || !finite(lo) || !finite(hi) || hi-lo > 1e-9 {
		t.Errorf("near-equal differences: %v %v %v", d, lo, hi)
	}
	for _, tc := range []struct {
		name      string
		inc, cand []float64
	}{
		{"no pair", nil, nil},
		{"one pair", []float64{1}, []float64{0}},
		{"unaligned", []float64{1, 2, 3}, []float64{1, 2}},
	} {
		if d, lo, hi := primaryInterval(tc.inc, tc.cand); d != 0 || lo != 0 || hi != 0 {
			t.Errorf("%s: nothing to conclude is zeros, got %v %v %v", tc.name, d, lo, hi)
		}
	}
	big := repeatF(1.7e308, 16)
	if d, lo, hi := primaryInterval(repeatF(0, 16), big); finite(d) && finite(lo) && finite(hi) {
		t.Errorf("a sum that overflows must not come back finite: %v %v %v", d, lo, hi)
	}
	spread := repeatF(0, 16)
	spread[3] = 1e200
	if _, lo, hi := primaryInterval(repeatF(0, 16), spread); finite(lo) && finite(hi) {
		t.Errorf("a sum of squares that overflows must not come back finite: %v %v", lo, hi)
	}
	// The smallest sizes have no room for the form (n <= c^2 + 1): they still get a finite, wider interval.
	for n := 2; n <= 8; n++ {
		inc, cand := make([]float64, n), make([]float64, n)
		for i := range cand {
			cand[i] = float64(i % 2)
		}
		if d, lo, hi := primaryInterval(inc, cand); !finite(lo) || !finite(hi) || !(lo < d && d < hi) {
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
		lows, deg := simLowers(inc, cand, 50, 1, buf)
		_, lo, _ := primaryInterval(inc, cand)
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
	} {
		t.Run(fmt.Sprintf("%s, n=%d", tc.name, tc.n), func(t *testing.T) {
			rng := rand.New(rand.NewSource(cellSeed(tc.name, tc.n)))
			adopt := 0
			for s := 0; s < trials; s++ {
				inc, cand := tc.gen(rng, tc.n)
				if _, lo, _ := primaryInterval(inc, cand); lo > tc.mu0 {
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
