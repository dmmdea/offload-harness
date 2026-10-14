package eval

// The calibration harness.
//
// It measures how often the gate's primary interval would say ADOPT when it should not: the true
// difference is exactly 0 (superiority) or exactly -margin (non-inferiority, the worst case the
// rule still has to refuse). Every simulated comparison is one dataset of n paired scores; the
// harness reads the lower end of the primary interval on it, and "ADOPT" is "the lower end clears
// the boundary" (the efficiency metric is assumed to clearly improve, so only the primary decides).
//
// Every method is applied to the SAME dataset and the SAME resamples, so differences between
// methods are paired, not two noisy estimates. Seeds are derived from the cell's name and size and
// from the simulation index, never from goroutine order, so a run is reproducible whatever the
// number of workers.
//
// The grid is large (minutes of CPU), so TestCalibrationStudy is off unless OFFLOAD_GATE_STUDY is
// set; it prints the tables the docs and ADR quote. The small tests in this file run always: they
// pin the harness's engine and its quantile functions to independent references and to the gate.
//
//	OFFLOAD_GATE_STUDY=1 go test ./internal/eval/ -run TestCalibrationStudy -v -timeout 1h
//
// Optional: OFFLOAD_GATE_STUDY_SIMS (default 20000), OFFLOAD_GATE_STUDY_B (default "2000,10000"),
// OFFLOAD_GATE_STUDY_ONLY (a substring of the cell names to run), OFFLOAD_GATE_STUDY_N (a list of sizes),
// OFFLOAD_GATE_STUDY_WORKERS.

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ---- the data-generating setups --------------------------------------------------------------

// pairGen draws n paired scores: the incumbent's and the candidate's on the same n tasks.
type pairGen func(rng *rand.Rand, n int) (inc, cand []float64)

func bernoulli(rng *rand.Rand, p, scale float64) float64 {
	if rng.Float64() < p {
		return scale
	}
	return 0
}

// genBinary: independent pass/fail scores (0 or scale) with pass rates pInc and pCand.
func genBinary(pInc, pCand, scale float64) pairGen {
	return func(rng *rand.Rand, n int) ([]float64, []float64) {
		inc, cand := make([]float64, n), make([]float64, n)
		for i := 0; i < n; i++ {
			inc[i] = bernoulli(rng, pInc, scale)
			cand[i] = bernoulli(rng, pCand, scale)
		}
		return inc, cand
	}
}

// genUniform: independent uniform scores, the incumbent's on [incLo, incHi], the candidate's on
// [candLo, candHi].
func genUniform(incLo, incHi, candLo, candHi float64) pairGen {
	return func(rng *rand.Rand, n int) ([]float64, []float64) {
		inc, cand := make([]float64, n), make([]float64, n)
		for i := 0; i < n; i++ {
			inc[i] = incLo + (incHi-incLo)*rng.Float64()
			cand[i] = candLo + (candHi-candLo)*rng.Float64()
		}
		return inc, cand
	}
}

// genLattice: independent scores k/10 with k uniform on [kLo, kHi]; the candidate's are scaled by
// candScale (1 for a null).
func genLattice(kLo, kHi int, candScale float64) pairGen {
	return func(rng *rand.Rand, n int) ([]float64, []float64) {
		inc, cand := make([]float64, n), make([]float64, n)
		for i := 0; i < n; i++ {
			inc[i] = float64(kLo+rng.Intn(kHi-kLo+1)) / 10
			cand[i] = candScale * float64(kLo+rng.Intn(kHi-kLo+1)) / 10
		}
		return inc, cand
	}
}

// genCubed: independent right-skewed scores, u^3 for the incumbent and candScale * v^3 for the
// candidate (u, v uniform on [0, 1]): most scores are near 0 and a few are near 1.
func genCubed(candScale float64) pairGen {
	return func(rng *rand.Rand, n int) ([]float64, []float64) {
		inc, cand := make([]float64, n), make([]float64, n)
		for i := 0; i < n; i++ {
			u, v := rng.Float64(), rng.Float64()
			inc[i] = u * u * u
			cand[i] = candScale * v * v * v
		}
		return inc, cand
	}
}

// simCell is one setup: n pairs drawn by gen, and the boundary mu0 the lower end has to clear (0 for
// superiority, -margin for non-inferiority).
type simCell struct {
	name string
	n    int
	mu0  float64
	gen  pairGen
}

// supNullCells are the superiority nulls: both arms score alike, so the true difference is 0.
func supNullCells(sizes ...int) []simCell {
	var out []simCell
	for _, n := range sizes {
		for _, c := range []struct {
			name string
			gen  pairGen
		}{
			{"sup binary p=.50", genBinary(.5, .5, 1)},
			{"sup binary p=.85", genBinary(.85, .85, 1)},
			{"sup uniform", genUniform(0, 1, 0, 1)},
			{"sup deciles 0..1", genLattice(0, 10, 1)},
			{"sup x^3 skewed", genCubed(1)},
			{"sup two-point 0/10", genBinary(.5, .5, 10)},
		} {
			out = append(out, simCell{name: c.name, n: n, mu0: 0, gen: c.gen})
		}
	}
	return out
}

// niNullCells are the non-inferiority nulls at exactly -margin (0.05 on a unit interval): the true
// candidate-minus-incumbent difference is -0.05, the worst case the rule must still refuse.
func niNullCells(sizes ...int) []simCell {
	var out []simCell
	for _, n := range sizes {
		for _, c := range []struct {
			name string
			gen  pairGen
		}{
			{"ni binary .90/.85", genBinary(.90, .85, 1)},
			{"ni binary .70/.65", genBinary(.70, .65, 1)},
			{"ni binary .55/.50", genBinary(.55, .50, 1)},
			{"ni continuous uniform", genUniform(0, 1, 0, .9)},
			{"ni continuous x^3 skewed", genCubed(.8)},
		} {
			out = append(out, simCell{name: c.name, n: n, mu0: -0.05, gen: c.gen})
		}
	}
	return out
}

// genZeroInflated: independent scores that are 0 with probability pZero and uniform on [0, 1] otherwise.
func genZeroInflated(pZeroInc, pZeroCand float64) pairGen {
	return func(rng *rand.Rand, n int) ([]float64, []float64) {
		inc, cand := make([]float64, n), make([]float64, n)
		for i := 0; i < n; i++ {
			if rng.Float64() >= pZeroInc {
				inc[i] = rng.Float64()
			}
			if rng.Float64() >= pZeroCand {
				cand[i] = rng.Float64()
			}
		}
		return inc, cand
	}
}

// genRareLarge: independent scores, uniform on [0, .1] with probability .9 and on [.9, 1] otherwise.
func genRareLarge(rng *rand.Rand, n int) ([]float64, []float64) {
	one := func() float64 {
		if rng.Float64() < 0.9 {
			return 0.1 * rng.Float64()
		}
		return 0.9 + 0.1*rng.Float64()
	}
	inc, cand := make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		inc[i], cand[i] = one(), one()
	}
	return inc, cand
}

// genNested: the candidate never wins a task the incumbent loses. The incumbent passes with
// probability p and the candidate fails a fraction loss of those passes, so the true difference is
// -p * loss and every non-zero pair difference is -1.
func genNested(p, loss float64) pairGen {
	return func(rng *rand.Rand, n int) ([]float64, []float64) {
		inc, cand := make([]float64, n), make([]float64, n)
		for i := 0; i < n; i++ {
			inc[i] = bernoulli(rng, p, 1)
			cand[i] = inc[i]
			if inc[i] == 1 && rng.Float64() < loss {
				cand[i] = 0
			}
		}
		return inc, cand
	}
}

// stressCells are nulls beyond the stated ones, kept to show where each method stops being safe.
func stressCells() []simCell {
	var out []simCell
	for _, n := range []int{16, 24} {
		out = append(out,
			simCell{name: "stress sup zero-inflated", n: n, mu0: 0, gen: genZeroInflated(.8, .8)},
			simCell{name: "stress sup rare-large", n: n, mu0: 0, gen: genRareLarge},
			simCell{name: "stress ni zero-inflated", n: n, mu0: -0.05, gen: genZeroInflated(.8, .9)},
		)
	}
	for _, n := range []int{20, 40, 80} {
		out = append(out, simCell{name: "stress ni nested loss 5%", n: n, mu0: -0.05, gen: genNested(.9, .05/.9)})
	}
	return out
}

// powerCells are moderate true effects: how often a method adopts when it should.
func powerCells() []simCell {
	var out []simCell
	for _, n := range []int{16, 24, 40} {
		out = append(out,
			simCell{name: "power sup binary .50->.60", n: n, mu0: 0, gen: genBinary(.5, .6, 1)},
			simCell{name: "power ni binary .90->.90", n: n, mu0: -0.05, gen: genBinary(.9, .9, 1)},
		)
	}
	out = append(out,
		simCell{name: "power sup uniform +.10", n: 24, mu0: 0, gen: genUniform(0, 1, .2, 1)},
		simCell{name: "power ni uniform 0", n: 24, mu0: -0.05, gen: genUniform(0, 1, 0, 1)},
	)
	return out
}

// ---- normal and Student-t quantiles ----------------------------------------------------------

func normCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }

// normQuantile is the inverse of the standard normal CDF, p in (0, 1).
func normQuantile(p float64) float64 { return -math.Sqrt2 * math.Erfcinv(2*p) }

func lgam(x float64) float64 { v, _ := math.Lgamma(x); return v }

// betacf is the continued fraction of the regularized incomplete beta function (modified Lentz).
func betacf(a, b, x float64) float64 {
	const maxIt, eps, tiny = 600, 1e-16, 1e-300
	qab, qap, qam := a+b, a+1, a-1
	c := 1.0
	d := 1 - qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIt; m++ {
		fm := float64(m)
		m2 := 2 * fm
		aa := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c
		aa = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	return h
}

// betaReg is the regularized incomplete beta function I_x(a, b).
func betaReg(a, b, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	bt := math.Exp(lgam(a+b) - lgam(a) - lgam(b) + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return bt * betacf(a, b, x) / a
	}
	return 1 - bt*betacf(b, a, 1-x)/b
}

// tCDF is the Student-t CDF with df degrees of freedom.
func tCDF(t, df float64) float64 {
	tail := 0.5 * betaReg(df/2, 0.5, df/(df+t*t))
	if t > 0 {
		return 1 - tail
	}
	return tail
}

// tQuantile is the inverse Student-t CDF for p in (0.5, 1), by bisection on tCDF.
func tQuantile(p, df float64) float64 {
	lo, hi := 0.0, 1.0
	for tCDF(hi, df) < p {
		hi *= 2
		if hi > 1e12 {
			return math.Inf(1)
		}
	}
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		if tCDF(mid, df) < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// ---- the methods -----------------------------------------------------------------------------

// The base methods read one lower end per dataset. The composites take the lower of two bases.
const (
	mPct   = iota // (current) the percentile of the bootstrap replicates: BootstrapDeltaMean's lower end
	mStud         // (i) studentized (bootstrap-t)
	mBCa          // (ii) bias-corrected and accelerated
	mPctW         // (iii) the percentile widened about the point estimate by t(n-1, .975) / z(.975)
	mPctW2        // (iii') the same, times sqrt(n / (n-1)): the factor the bootstrap's own variance lacks
	mT            // (iv) the paired t interval
	mScore        // (vi) the score form of the t interval: the standard error taken at the boundary
	nBase
)

var baseNames = [nBase]string{
	mPct:   "percentile (current)",
	mStud:  "(i) studentized",
	mBCa:   "(ii) BCa",
	mPctW:  "(iii) pctl x t/z",
	mPctW2: "(iii') pctl x t/z x sqrt(n/(n-1))",
	mT:     "(iv) paired t",
	mScore: "(vi) score-form t",
}

// composites are (v): the lower of two bases (so the verdict needs both to clear the boundary).
var composites = []struct {
	name string
	a, b int
}{
	{"(v) min(studentized, t)", mStud, mT},
	{"(v) min(studentized, BCa)", mStud, mBCa},
	{"(v) min(BCa, t)", mBCa, mT},
	{"(v) min(iii', BCa)", mPctW2, mBCa},
	{"(v) min(iii', t)", mPctW2, mT},
	{"(v) min(studentized, iii')", mStud, mPctW2},
	{"(v) min(percentile, vi)", mPct, mScore},
	{"(v) min(BCa, vi)", mBCa, mScore},
	{"(v) min(iii', vi)", mPctW2, mScore},
}

func simMethodNames() []string {
	out := append([]string{}, baseNames[:]...)
	for _, c := range composites {
		out = append(out, c.name)
	}
	return out
}

// simBuf holds one worker's buffers.
type simBuf struct {
	m, num, sdv, tt, sorted []float64
	zero                    []bool
}

func newSimBuf(B int) *simBuf {
	return &simBuf{
		m: make([]float64, B), num: make([]float64, B), sdv: make([]float64, B),
		tt: make([]float64, B), sorted: make([]float64, B), zero: make([]bool, B),
	}
}

// simLowers returns the lower end of every method (bases, then composites) on one dataset, with B
// resamples drawn from seed in the order bootstrapReps draws them. degenerate reports a dataset
// whose paired differences are all equal: its interval is the point, for every method.
func simLowers(inc, cand []float64, B int, seed int64, buf *simBuf) (lows []float64, degenerate bool) {
	n := len(inc)
	fn := float64(n)
	d := make([]float64, n)
	var sum float64
	for i := range d {
		d[i] = cand[i] - inc[i]
		sum += d[i]
	}
	dbar := sum / fn
	lows = make([]float64, nBase+len(composites))
	degenerate = true
	for i := 1; i < n; i++ {
		if d[i] != d[0] {
			degenerate = false
			break
		}
	}
	if degenerate {
		for k := range lows {
			lows[k] = dbar
		}
		return lows, true
	}
	e, e2 := make([]float64, n), make([]float64, n)
	var s2, s3 float64
	for i := range e {
		e[i] = d[i] - dbar
		e2[i] = e[i] * e[i]
		s2 += e2[i]
		s3 += e2[i] * e[i]
	}
	sd := math.Sqrt(s2 / (fn - 1))
	se := sd / math.Sqrt(fn)

	// B resamples. The replicate mean is computed exactly as bootstrapReps computes it.
	rng := rand.New(rand.NewSource(seed))
	for b := 0; b < B; b++ {
		var si, sc, sE, sQ float64
		same := true
		var first float64
		for i := 0; i < n; i++ {
			idx := rng.Intn(n)
			si += inc[idx]
			sc += cand[idx]
			sE += e[idx]
			sQ += e2[idx]
			if i == 0 {
				first = d[idx]
			} else if d[idx] != first {
				same = false
			}
		}
		buf.m[b] = (sc - si) / fn
		buf.num[b] = sE / fn
		v := (sQ - sE*sE/fn) / (fn - 1)
		if v < 0 {
			v = 0
		}
		buf.sdv[b] = math.Sqrt(v)
		buf.zero[b] = same
	}

	// (current) percentile.
	copy(buf.sorted, buf.m[:B])
	sort.Float64s(buf.sorted[:B])
	pctLo := percentile(buf.sorted[:B], 0.025)
	lows[mPct] = pctLo

	// (i) studentized. A resample whose differences are all equal has no spread of its own: its
	// statistic is 0 if it sits on the sample mean and +-Inf by the side it sits on otherwise.
	sqn := math.Sqrt(fn)
	for b := 0; b < B; b++ {
		switch {
		case buf.zero[b] && buf.num[b] == 0:
			buf.tt[b] = 0
		case buf.zero[b] && buf.num[b] > 0:
			buf.tt[b] = math.Inf(1)
		case buf.zero[b]:
			buf.tt[b] = math.Inf(-1)
		default:
			buf.tt[b] = buf.num[b] / (buf.sdv[b] / sqn)
		}
	}
	tsorted := buf.tt[:B]
	sort.Float64s(tsorted)
	lows[mStud] = dbar - percentile(tsorted, 0.975)*se

	// (ii) BCa. The bias correction counts the replicates below the sample mean (ties count half);
	// the acceleration is the jackknife one, which for a mean is the skewness of the differences.
	tol := 1e-12 * math.Max(1, maxAbs(d))
	var less, ties float64
	for _, v := range buf.sorted[:B] {
		switch {
		case v < dbar-tol:
			less++
		case v <= dbar+tol:
			ties++
		}
	}
	prop := (less + 0.5*ties) / float64(B)
	prop = math.Min(math.Max(prop, 0.5/float64(B)), 1-0.5/float64(B))
	z0 := normQuantile(prop)
	acc := s3 / (6 * math.Pow(s2, 1.5))
	lows[mBCa] = bcaEnd(buf.sorted[:B], z0, acc, normQuantile(0.025))

	// (iii) and (iii') the percentile widened about the point estimate.
	tq := tQuantile(0.975, fn-1)
	z := normQuantile(0.975)
	f := tq / z
	f2 := f * math.Sqrt(fn/(fn-1))
	lows[mPctW] = math.Min(pctLo, dbar-f*(dbar-pctLo))
	lows[mPctW2] = math.Min(pctLo, dbar-f2*(dbar-pctLo))

	// (iv) the paired t interval.
	lows[mT] = dbar - tq*se

	// (vi) the score form: the t interval inverted from a statistic whose variance is taken at the
	// boundary, (dbar - mu) / sqrt(mean((d - mu)^2) / n), which is dbar -+ c * sn / sqrt(n - c^2) for
	// c = t(n-1, .975) and sn the divisor-n standard deviation. For n too small for the form (n <= c^2 + 1)
	// the denominator is held at 1.
	sn := math.Sqrt(s2 / fn)
	den := math.Max(fn-tq*tq, 1)
	lows[mScore] = dbar - tq*sn/math.Sqrt(den)

	for i, c := range composites {
		lows[nBase+i] = math.Min(lows[c.a], lows[c.b])
	}
	return lows, false
}

func maxAbs(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		m = math.Max(m, math.Abs(x))
	}
	return m
}

// bcaEnd reads the BCa end for the tail at normal quantile zAlpha from the sorted replicates.
func bcaEnd(sorted []float64, z0, acc, zAlpha float64) float64 {
	num := z0 + zAlpha
	den := 1 - acc*num
	var level float64
	switch {
	case den <= 0 && num > 0:
		level = 1
	case den <= 0:
		level = 0
	default:
		level = normCDF(z0 + num/den)
	}
	return percentile(sorted, level)
}

// simRun is the result of one cell at one B: how many of sims datasets each method adopted on.
type simRun struct {
	cell       simCell
	B, sims    int
	adopt      []int
	degenerate int
}

func (r simRun) rate(k int) float64 { return float64(r.adopt[k]) / float64(r.sims) }

// cellSeed derives a cell's data seed from its name and size alone.
func cellSeed(name string, n int) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name + "|" + strconv.Itoa(n)))
	return int64(h.Sum32()%2000000000) + 1000
}

// runCell draws sims datasets for the cell and counts, per method, those whose lower end clears the
// boundary. Dataset s uses the data seed cellSeed+s and the bootstrap seed bootSeed+s; the cell's
// results do not depend on the number of workers.
func runCell(c simCell, B, sims, workers int) simRun {
	base := cellSeed(c.name, c.n)
	nm := nBase + len(composites)
	res := simRun{cell: c, B: B, sims: sims, adopt: make([]int, nm)}
	var mu sync.Mutex
	var wg sync.WaitGroup
	next := make(chan int, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := newSimBuf(B)
			local := make([]int, nm)
			ldeg := 0
			for s := range next {
				rng := rand.New(rand.NewSource(base + int64(s)))
				inc, cand := c.gen(rng, c.n)
				lows, deg := simLowers(inc, cand, B, 5000000+int64(s), buf)
				if deg {
					ldeg++
				}
				for k, lo := range lows {
					if lo > c.mu0 {
						local[k]++
					}
				}
			}
			mu.Lock()
			for k := range local {
				res.adopt[k] += local[k]
			}
			res.degenerate += ldeg
			mu.Unlock()
		}()
	}
	for s := 0; s < sims; s++ {
		next <- s
	}
	close(next)
	wg.Wait()
	return res
}

// ---- the study -------------------------------------------------------------------------------

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func pct(x float64) string { return fmt.Sprintf("%.2f", 100*x) }

func printStudy(title string, runs []simRun, names []string, nullOnly bool) {
	fmt.Printf("\n### %s\n\n| cell | n | degenerate |", title)
	for _, nm := range names {
		fmt.Printf(" %s |", nm)
	}
	fmt.Printf("\n|---|---|---|")
	for range names {
		fmt.Printf("---|")
	}
	fmt.Println()
	worst := make([]float64, len(names))
	worstAt := make([]string, len(names))
	over := make([]int, len(names))
	cells := 0
	for _, r := range runs {
		fmt.Printf("| %s | %d | %s |", r.cell.name, r.cell.n, pct(float64(r.degenerate)/float64(r.sims)))
		for k := range names {
			rate := r.rate(k)
			mark := ""
			if nullOnly && rate > 0.04 {
				mark = " !"
				over[k]++
			}
			if rate > worst[k] {
				worst[k], worstAt[k] = rate, fmt.Sprintf("%s n=%d", r.cell.name, r.cell.n)
			}
			fmt.Printf(" %s%s |", pct(rate), mark)
		}
		fmt.Println()
		cells++
	}
	if nullOnly {
		fmt.Printf("| **worst cell** | | |")
		for k := range names {
			fmt.Printf(" %s |", pct(worst[k]))
		}
		fmt.Printf("\n| **cells over 4%%** (of %d) | | |", cells)
		for k := range names {
			fmt.Printf(" %d |", over[k])
		}
		fmt.Println()
		for k, nm := range names {
			fmt.Printf("<!-- worst %s: %s at %s -->\n", nm, pct(worst[k]), worstAt[k])
		}
	}
}

func TestCalibrationStudy(t *testing.T) {
	if os.Getenv("OFFLOAD_GATE_STUDY") == "" {
		t.Skip("set OFFLOAD_GATE_STUDY=1 to run the calibration grid (minutes of CPU)")
	}
	sims := envInt("OFFLOAD_GATE_STUDY_SIMS", 20000)
	workers := envInt("OFFLOAD_GATE_STUDY_WORKERS", runtime.NumCPU())
	var bs []int
	for _, f := range strings.Split(envOr("OFFLOAD_GATE_STUDY_B", "2000,10000"), ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && v > 0 {
			bs = append(bs, v)
		}
	}
	only := os.Getenv("OFFLOAD_GATE_STUDY_ONLY")
	sizes := map[int]bool{}
	for _, f := range strings.Split(os.Getenv("OFFLOAD_GATE_STUDY_N"), ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && v > 0 {
			sizes[v] = true
		}
	}
	keep := func(cs []simCell) []simCell {
		var out []simCell
		for _, c := range cs {
			if (only == "" || strings.Contains(c.name, only)) && (len(sizes) == 0 || sizes[c.n]) {
				out = append(out, c)
			}
		}
		return out
	}
	nulls := keep(append(supNullCells(16, 24, 40), niNullCells(16, 20, 24, 40, 80)...))
	powers := keep(powerCells())
	stress := keep(stressCells())
	names := simMethodNames()
	fmt.Printf("# calibration study: %d datasets per cell, workers %d, B in %v\n", sims, workers, bs)
	for _, B := range bs {
		var runs []simRun
		for _, c := range nulls {
			runs = append(runs, runCell(c, B, sims, workers))
			fmt.Fprintf(os.Stderr, "null %s n=%d B=%d done\n", c.name, c.n, B)
		}
		printStudy(fmt.Sprintf("false ADOPT rate (%%), B = %d", B), runs, names, true)
		var sruns []simRun
		for _, c := range stress {
			sruns = append(sruns, runCell(c, B, sims, workers))
			fmt.Fprintf(os.Stderr, "stress %s n=%d B=%d done\n", c.name, c.n, B)
		}
		printStudy(fmt.Sprintf("stress nulls, outside the bar: false ADOPT rate (%%), B = %d", B), sruns, names, false)
		var pruns []simRun
		for _, c := range powers {
			pruns = append(pruns, runCell(c, B, sims, workers))
			fmt.Fprintf(os.Stderr, "power %s n=%d B=%d done\n", c.name, c.n, B)
		}
		printStudy(fmt.Sprintf("power: ADOPT rate (%%) at a moderate true effect, B = %d", B), pruns, names, false)
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// ---- the harness's own tests -----------------------------------------------------------------

// The engine draws resamples in the order bootstrapReps draws them and computes the replicate mean
// the same way, so its percentile method IS BootstrapDeltaMean's lower end, to the bit.
func TestSimHarnessPercentileMethodIsBootstrapDeltaMean(t *testing.T) {
	for _, tc := range []struct {
		cell simCell
		B    int
		seed int64
	}{
		{simCell{name: "binary", n: 16, gen: genBinary(.5, .5, 1)}, 2000, 1},
		{simCell{name: "uniform", n: 20, gen: genUniform(0, 1, 0, 1)}, 2000, 7},
		{simCell{name: "deciles", n: 24, gen: genLattice(0, 10, 1)}, 500, 42},
	} {
		buf := newSimBuf(tc.B)
		for s := 0; s < 20; s++ {
			rng := rand.New(rand.NewSource(int64(100 + s)))
			inc, cand := tc.cell.gen(rng, tc.cell.n)
			lows, deg := simLowers(inc, cand, tc.B, tc.seed, buf)
			_, lo, _ := BootstrapDeltaMean(inc, cand, tc.B, tc.seed)
			if deg {
				continue
			}
			if lows[mPct] != lo {
				t.Fatalf("%s sim %d: harness percentile %v != BootstrapDeltaMean %v", tc.cell.name, s, lows[mPct], lo)
			}
		}
	}
}

// The quantile functions against scipy.stats (t.ppf at .975 and at the Bonferroni levels, norm.ppf).
func TestSimHarnessQuantilesMatchScipy(t *testing.T) {
	for _, tc := range []struct {
		p, df, want float64
	}{
		{0.975, 1, 12.706204736174694},
		{0.975, 5, 2.5705818356363146},
		{0.975, 15, 2.1314495455597755},
		{0.975, 19, 2.0930240544083087},
		{0.975, 23, 2.0686576104190486},
		{0.975, 39, 2.022690920036761},
		{0.975, 79, 1.9904502102301285},
		{0.975, 999, 1.9623414611334493},
		{0.9875, 15, 2.489879703479893},
		{0.9916666666666667, 19, 2.625105913222787},
		{0.996875, 39, 2.8907801951969136},
	} {
		if got := tQuantile(tc.p, tc.df); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("tQuantile(%v, %v) = %.12f, want %.12f", tc.p, tc.df, got, tc.want)
		}
	}
	if got := normQuantile(0.975); math.Abs(got-1.959963984540054) > 1e-12 {
		t.Errorf("normQuantile(.975) = %.15f", got)
	}
	if got := normCDF(1.959963984540054); math.Abs(got-0.975) > 1e-12 {
		t.Errorf("normCDF(1.96) = %.15f", got)
	}
}

// The generators have the moments they are named for (a wrong null would measure the wrong thing).
func TestSimHarnessGeneratorsHaveTheirMoments(t *testing.T) {
	const n = 400000
	rng := rand.New(rand.NewSource(3))
	mean := func(v []float64) float64 {
		var s float64
		for _, x := range v {
			s += x
		}
		return s / float64(len(v))
	}
	for _, tc := range []struct {
		name      string
		gen       pairGen
		inc, cand float64
	}{
		{"binary .90/.85", genBinary(.9, .85, 1), .9, .85},
		{"two-point 0/10", genBinary(.5, .5, 10), 5, 5},
		{"uniform 0..1 vs 0..0.9", genUniform(0, 1, 0, .9), .5, .45},
		{"deciles 0..1", genLattice(0, 10, 1), .5, .5},
		{"x^3 skewed .8", genCubed(.8), .25, .2},
		{"zero-inflated .8/.9", genZeroInflated(.8, .9), .1, .05},
		{"nested loss 5%", genNested(.9, .05/.9), .9, .85},
	} {
		inc, cand := tc.gen(rng, n)
		if mi, mc := mean(inc), mean(cand); math.Abs(mi-tc.inc) > 0.01*math.Max(1, tc.inc) || math.Abs(mc-tc.cand) > 0.01*math.Max(1, tc.cand) {
			t.Errorf("%s: means %.4f / %.4f, want %.4f / %.4f", tc.name, mi, mc, tc.inc, tc.cand)
		}
	}
}

// ---- the efficiency read ---------------------------------------------------------------------

// effCell is a null for a cost metric: the candidate costs the same as the incumbent on average, so
// "improves" (the candidate's cost interval lies wholly below 0) is a false improvement.
type effCell struct {
	name string
	n    int
	gen  pairGen // inc = the incumbent's cost, cand = the candidate's cost
}

func genLogNormalCost(sigma float64) pairGen {
	return func(rng *rand.Rand, n int) ([]float64, []float64) {
		inc, cand := make([]float64, n), make([]float64, n)
		for i := 0; i < n; i++ {
			inc[i] = math.Exp(7 + sigma*rng.NormFloat64())
			cand[i] = math.Exp(7 + sigma*rng.NormFloat64())
		}
		return inc, cand
	}
}

// genMultiplicativeCost: the candidate costs the incumbent's cost times exp(noise), a paired cost with a
// right-skewed difference.
func genMultiplicativeCost(noise float64) pairGen {
	return func(rng *rand.Rand, n int) ([]float64, []float64) {
		inc, cand := make([]float64, n), make([]float64, n)
		for i := 0; i < n; i++ {
			inc[i] = math.Exp(7 + rng.NormFloat64())
			cand[i] = inc[i] * math.Exp(noise*rng.NormFloat64())
		}
		return inc, cand
	}
}

func genExponentialCost(rng *rand.Rand, n int) ([]float64, []float64) {
	inc, cand := make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		inc[i] = 1000 * rng.ExpFloat64()
		cand[i] = 1000 * rng.ExpFloat64()
	}
	return inc, cand
}

func effCells() []effCell {
	var out []effCell
	for _, n := range []int{16, 24, 40} {
		out = append(out,
			effCell{"cost lognormal(7,1) independent", n, genLogNormalCost(1)},
			effCell{"cost lognormal(7,2) independent", n, genLogNormalCost(2)},
			effCell{"cost times exp(N(0,.25)) paired", n, genMultiplicativeCost(.25)},
			effCell{"cost exponential independent", n, genExponentialCost},
		)
	}
	return out
}

// effMethods are the reads of one cost metric at one tail mass: the percentile bootstrap (what the gate does now),
// the paired t, and the score form of the t.
var effMethodNames = []string{"percentile (current)", "paired t", "score-form t"}

// runEffCell counts the datasets on which each read says "the candidate's cost improves" (its upper end below 0)
// at the tail mass tail on each side, with B resamples for the percentile read.
func runEffCell(c effCell, tail float64, B, sims, workers int) []int {
	base := cellSeed("eff|"+c.name, c.n)
	counts := make([]int, len(effMethodNames))
	var mu sync.Mutex
	var wg sync.WaitGroup
	next := make(chan int, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]int, len(counts))
			reps := make([]float64, B)
			for s := range next {
				rng := rand.New(rand.NewSource(base + int64(s)))
				inc, cand := c.gen(rng, c.n)
				n := c.n
				fn := float64(n)
				var sum float64
				d := make([]float64, n)
				for i := range d {
					d[i] = cand[i] - inc[i]
					sum += d[i]
				}
				dbar := sum / fn
				var s2 float64
				for _, v := range d {
					s2 += (v - dbar) * (v - dbar)
				}
				brng := rand.New(rand.NewSource(7000000 + int64(s)))
				for b := 0; b < B; b++ {
					var si, sc float64
					for i := 0; i < n; i++ {
						idx := brng.Intn(n)
						si += inc[idx]
						sc += cand[idx]
					}
					reps[b] = (sc - si) / fn
				}
				sort.Float64s(reps)
				if percentile(reps, 1-tail) < 0 {
					local[0]++
				}
				tq := tQuantile(1-tail, fn-1)
				if dbar+tq*math.Sqrt(s2/(fn-1)/fn) < 0 {
					local[1]++
				}
				den := math.Max(fn-tq*tq, 1)
				if dbar+tq*math.Sqrt(s2/fn)/math.Sqrt(den) < 0 {
					local[2]++
				}
			}
			mu.Lock()
			for k := range local {
				counts[k] += local[k]
			}
			mu.Unlock()
		}()
	}
	for s := 0; s < sims; s++ {
		next <- s
	}
	close(next)
	wg.Wait()
	return counts
}

func TestEfficiencyCalibrationStudy(t *testing.T) {
	if os.Getenv("OFFLOAD_GATE_STUDY") == "" {
		t.Skip("set OFFLOAD_GATE_STUDY=1 to run the efficiency grid")
	}
	sims := envInt("OFFLOAD_GATE_STUDY_SIMS", 20000)
	workers := envInt("OFFLOAD_GATE_STUDY_WORKERS", runtime.NumCPU())
	for _, tc := range []struct {
		label string
		tail  float64
		B     int
	}{
		{"one metric (95% interval), B = 2000", 0.025, 2000},
		{"three metrics (Bonferroni, tail 0.025/3), B = 2000", 0.025 / 3, 2000},
		{"one metric, B = 10", 0.025, 10},
		{"one metric, B = 1", 0.025, 1},
	} {
		fmt.Printf("\n### false improvement of a cost metric (%%): %s\n\n| cell | n |", tc.label)
		for _, m := range effMethodNames {
			fmt.Printf(" %s |", m)
		}
		fmt.Printf("\n|---|---|")
		for range effMethodNames {
			fmt.Printf("---|")
		}
		fmt.Println()
		for _, c := range effCells() {
			if tc.B < 2000 && c.n != 16 {
				continue
			}
			counts := runEffCell(c, tc.tail, tc.B, sims, workers)
			fmt.Printf("| %s | %d |", c.name, c.n)
			for _, k := range counts {
				fmt.Printf(" %s |", pct(float64(k)/float64(sims)))
			}
			fmt.Println()
		}
	}
}
