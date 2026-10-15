package eval

import "math"

// The gate's primary interval.
//
// The primary is the mean of the paired task differences d = candidate - incumbent, each in [-R, R] (R is the
// width of the primary's score range: 1 for the unit interval, 10 for the 0/10 judge class), with a two-sided
// 95% interval. Two constructions read it, and the data alone decides which.
//
// Pass/fail pairs. When every difference is a tie (0), a win of the whole range (+R) or a loss of it (-R), the
// dataset is three counts and the interval is Tango's score interval for a paired difference (Tango 1998,
// Statistics in Medicine 17:891). It is the set of differences mu whose score statistic
//
//	(wins - losses - n mu) / sqrt(n * (u + v - mu^2)),
//
// stays within +-1.96, where u and v, the probabilities of a win and a loss, are the maximum-likelihood values
// under the constraint u - v = mu. The variance is the one the hypothesised difference would give, not the one
// the sample happened to show. That is the point of the construction: when the arms disagree on a handful of
// tasks the observed variance is small and noisy, and an interval built on it certifies a candidate that loses
// 5% of the incumbent's passes at a margin of 0.05 in up to 18% of samples (one loss in 62 tasks cleared the
// margin of the gate's version 2). With nothing disagreeing at all the ends are +-1.96^2 / (n + 1.96^2) of the
// range: a margin of 0.05 needs 73 ties, the chance of which at a true loss rate of 5% is 2.4%.
//
// Every other pair of scores (partial credit, a mean of several attempts, a judge's integer scale) has no
// such lattice to constrain the variance with. Its interval is the score form of the t interval
//
//	delta = mean(d)    sn = sqrt(sum((d - m)^2) / N)    c = t(N-1, .975)    half = c * sn / sqrt(N - c^2)
//
// taken on the sample with two pseudo-pairs added at each end of the difference range, +R and -R (N = n + 4 and
// m the mean of the augmented sample), centred on the observed mean and clipped to [-R, R]. It is the
// Agresti-Caffo / Agresti-Min idea of adding pseudo-observations to a small count (Agresti and Caffo 2000 add
// two successes and two failures), generalised to bounded scores: the pseudo-pairs inflate the variance when
// the differences are few and vanish as n grows, and they leave a sample whose differences are all equal with
// a standard error, so no special case remains. The weights are the smallest of the variants tried that keeps
// every sparse stress null at or under 4%, and not the published form: half a pair at each of the four cells
// of the 2x2 table (Agresti-Min) would read a candidate that loses 5% of the incumbent's passes at 8.6% over
// 80 tasks, and one pair at each end leaves a mostly pass/fail score set with a few half-credit tasks at 4.4%.
//
// The measured false-ADOPT rate of these and of the other intervals considered, on the same datasets, is in the
// ADR and reproduces with TestCalibrationStudy (gate_sim_test.go and gate_sparse_sim_test.go).
//
// BootstrapDeltaMean (the percentile bootstrap, B and seed from the call site) is unchanged for its legacy
// callers; the gate reads the efficiency metrics with it, not the primary.

// primaryInterval returns the mean paired difference and the lower and upper ends of its 95% interval over
// aligned per-task scores, which lie in a range of width span (the primary's score range). With fewer than two
// tasks, or unaligned input, there is nothing to conclude and it returns zeros, as BootstrapDeltaMean does. A
// sum that overflows gives a number that is not finite, which the caller refuses, and so does a span that is
// not a positive number (there is no range to scale a pseudo-pair by).
func primaryInterval(inc, cand []float64, span float64) (delta, lo, hi float64) {
	n := len(inc)
	if n != len(cand) || n < 2 {
		return 0, 0, 0
	}
	fn := float64(n)
	var sum float64
	for i := range inc {
		sum += cand[i] - inc[i]
	}
	delta = sum / fn
	if !finite(delta) {
		return delta, delta, delta
	}
	if !(span > 0) || !finite(span) {
		return delta, math.NaN(), math.NaN()
	}
	if wins, losses, ok := winsAndLosses(inc, cand, span); ok {
		// The delta is read from the counts: a sum of floats such as 0.1 is not its sixteenth part.
		lo, hi = tangoInterval(n, wins, losses)
		return float64(wins-losses) * span / fn, lo * span, hi * span
	}
	lo, hi = augmentedInterval(inc, cand, sum, span)
	return delta, lo, hi
}

// equalTolerance is how far apart, as a share of the score range, two paired differences can be and still
// count as the same one (a tie, a win of the whole range): the rounding of float scores (0.3 - 0.2 against
// 0.2 - 0.1) is not a difference.
const equalTolerance = 1e-9

// winsAndLosses counts the pairs that are a win of the whole range (+span) and a loss of it (-span), to within
// equalTolerance of the range, and reports whether every pair is one of those or a tie (0).
func winsAndLosses(inc, cand []float64, span float64) (wins, losses int, ok bool) {
	tol := equalTolerance * span
	for i := range inc {
		switch d := cand[i] - inc[i]; {
		case math.Abs(d) <= tol:
		case math.Abs(d-span) <= tol:
			wins++
		case math.Abs(d+span) <= tol:
			losses++
		default:
			return 0, 0, false
		}
	}
	return wins, losses, true
}

// augmentedInterval is the score form of the t interval on the n differences and two pseudo-pairs at each of
// +span and -span, centred on the observed mean and clipped to [-span, span], the range the mean of such
// differences cannot leave. sum is the sum of the n differences, which the pseudo-pairs leave unchanged. An
// interval whose sum of squares overflows is returned as (-Inf, +Inf), which the caller refuses.
func augmentedInterval(inc, cand []float64, sum, span float64) (lo, hi float64) {
	n := len(inc)
	big := float64(n + pseudoPairs)
	m := sum / big
	ss := float64(pseudoPairs/2) * ((span-m)*(span-m) + (span+m)*(span+m))
	for i := range inc {
		e := cand[i] - inc[i] - m
		ss += e * e
	}
	if !finite(ss) {
		return math.Inf(-1), math.Inf(1)
	}
	c := tCritical975(n + pseudoPairs - 1)
	// For N <= c^2 + 1 (the smallest samples) the form has no room: the denominator is held at 1.
	half := c * math.Sqrt(ss/big) / math.Sqrt(math.Max(big-c*c, 1))
	delta := sum / float64(n)
	return math.Max(-span, delta-half), math.Min(span, delta+half)
}

// pseudoPairs is the number of pseudo-pairs the augmented interval adds: half at +R and half at -R.
const pseudoPairs = 4

// normal975 is the 97.5% point of the standard normal distribution.
const normal975 = 1.959963984540054

// tangoVar0 is the variance of one pair difference, in units of the range, when the true mean difference is
// delta: u + v - delta^2, with u and v the probabilities of a win and a loss at the maximum-likelihood values
// under u - v = delta (Tango 1998). With v the loss probability the likelihood's stationary point solves
// 2 n v^2 - B v - C = 0, B = (w + l)(1 - delta) - 2 delta (l + t), C = l delta (1 - delta) for w wins, l
// losses and t ties; the likelihood is concave, so its maximum is the larger root, or the edge u = 0 (v =
// -delta for a negative delta) when the root lies beyond it.
func tangoVar0(n, wins, losses int, delta float64) float64 {
	fn := float64(n)
	ties := fn - float64(wins) - float64(losses)
	b := float64(wins+losses)*(1-delta) - 2*delta*(float64(losses)+ties)
	c := float64(losses) * delta * (1 - delta)
	disc := b*b + 8*fn*c
	if disc < 0 {
		disc = 0
	}
	v := (b + math.Sqrt(disc)) / (4 * fn)
	if edge := math.Max(0, -delta); v < edge {
		v = edge
	}
	return 2*v + delta - delta*delta
}

// tangoZ is Tango's score statistic for the null difference delta (in units of the range). It falls as delta
// rises. A null whose variance is nil (delta = +-1) is infinitely far from a sample that differs from it.
func tangoZ(n, wins, losses int, delta float64) float64 {
	num := float64(wins-losses) - float64(n)*delta
	v0 := tangoVar0(n, wins, losses, delta)
	if !(v0 > 0) {
		switch {
		case num > 0:
			return math.Inf(1)
		case num < 0:
			return math.Inf(-1)
		}
		return 0
	}
	return num / math.Sqrt(float64(n)*v0)
}

// tangoInterval inverts the score test by bisection: the lower end is the delta at which the statistic is
// +1.96 and the upper end the one at which it is -1.96, in units of the range. The statistic is monotone in
// delta on [-1, 1], so 80 halvings of the two-unit range land on the end to the last bit of a float64, and the
// result depends on nothing but IEEE arithmetic and a square root.
func tangoInterval(n, wins, losses int) (lo, hi float64) {
	mean := float64(wins-losses) / float64(n)
	a, b := -1.0, mean
	for i := 0; i < 80; i++ {
		if m := (a + b) / 2; tangoZ(n, wins, losses, m) > normal975 {
			a = m
		} else {
			b = m
		}
	}
	lo = (a + b) / 2
	a, b = mean, 1.0
	for i := 0; i < 80; i++ {
		if m := (a + b) / 2; tangoZ(n, wins, losses, m) < -normal975 {
			b = m
		} else {
			a = m
		}
	}
	return lo, (a + b) / 2
}

// tCritical975 is t(df, .975), the two-sided 95% critical value of Student's t with df degrees of
// freedom. Exact to float64 for df up to 200 (a table, so the gate needs no special function and
// its output does not move with the platform's math library); beyond it the three-term
// Cornish-Fisher expansion of the normal quantile, whose error is under 1e-9 there and shrinks.
func tCritical975(df int) float64 {
	if df < 1 {
		df = 1
	}
	if df <= len(tCritical975Table) {
		return tCritical975Table[df-1]
	}
	const z = 1.959963984540054 // the normal 97.5% point
	z3, z5, z7 := z*z*z, z*z*z*z*z, z*z*z*z*z*z*z
	d := float64(df)
	return z + (z3+z)/(4*d) + (5*z5+16*z3+3*z)/(96*d*d) + (3*z7+19*z5+17*z3-15*z)/(384*d*d*d)
}

// tCritical975Table[df-1] is t(df, .975) for df = 1 to 200 (scipy.stats.t.ppf, printed to the shortest
// string that round-trips).
var tCritical975Table = [200]float64{
	12.706204736174694, 4.302652729749462, 3.1824463052837078, 2.7764451051977934,
	2.5705818356363146, 2.4469118511449786, 2.364624251592784, 2.306004135204166,
	2.262157162798205, 2.2281388519862744, 2.200985160091639, 2.1788128296672284,
	2.1603686564627913, 2.144786687917804, 2.1314495455597755, 2.1199052992212546,
	2.1098155778333156, 2.1009220402410382, 2.0930240544083087, 2.085963447265864,
	2.0796138447276795, 2.0738730679040254, 2.0686576104190486, 2.0638985616280245,
	2.0595385527532977, 2.0555294386428735, 2.0518305164802846, 2.0484071417952454,
	2.045229642132704, 2.0422724563012378, 2.0395134463964086, 2.0369333434601016,
	2.0345152974493383, 2.0322445093177186, 2.030107928250343, 2.0280940009804502,
	2.0261924630291093, 2.0243941639119694, 2.022690920036761, 2.021075390306273,
	2.019540970441376, 2.018081702818444, 2.016692199227824, 2.0153675744437636,
	2.014103388880846, 2.012895598919429, 2.0117405137297655, 2.010634757624232,
	2.0095752371292392, 2.0085591121007607, 2.007583770315836, 2.006646805061688,
	2.0057459953178687, 2.0048792881880564, 2.0040447832891455, 2.003240718847872,
	2.002465459291007, 2.0017174841452356, 2.000995378088267, 2.00029782201426,
	1.999623584994939, 1.9989715170333788, 1.998340542520741, 1.997729654317693,
	1.9971379083920036, 1.9965644189523117, 1.996008354025296, 1.9954689314298435,
	1.9949454151072379, 1.994437111771186, 1.9939433678456255, 1.9934635666618719,
	1.9929971258898547, 1.9925434951809318, 1.9921021540022417, 1.9916726096446642,
	1.9912543953883846, 1.9908470688116906, 1.9904502102301285, 1.990063421254446,
	1.9896863234569024, 1.989318557136572, 1.9889597801751624, 1.9886096669757085,
	1.9882679074772216, 1.98793420623902, 1.9876082815890708, 1.9872898648311692,
	1.986978699506281, 1.9866745407037683, 1.9863771544186177, 1.98608631695113,
	1.985801814345823, 1.9855234418666037, 1.9852510035054978, 1.984984311522457,
	1.9847231860139845, 1.9844674545084815, 1.984216951586417, 1.983971518523552,
	1.983731002955606, 1.9834952585628793, 1.9832641447734567, 1.9830375264837257,
	1.9828152737950475, 1.9825972617655006, 1.9823833701756908, 1.9821734833077271,
	1.9819674897364823, 1.981765282132372, 1.9815667570749007, 1.9813718148763053,
	1.981180359414661, 1.9809922979758567, 1.9808075411039094, 1.9806260024590892,
	1.9804475986834025, 1.9802722492729743, 1.9800998764569397, 1.9799304050824402,
	1.9797637625053868, 1.9795998784866387, 1.9794386850933035, 1.979280116604855,
	1.9791241094237977, 1.9789706019906281, 1.9788195347028539, 1.978670849837835,
	1.9785244914792575, 1.9783804054470222, 1.9782385392303798, 1.9780988419241303,
	1.977961264167726, 1.9778257580871244, 1.9776922772392527, 1.977560776558935,
	1.9774312123081748, 1.9773035420276506, 1.9771777244903335, 1.9770537196570985,
	1.9769314886342528, 1.9768109936328597, 1.976692197929798, 1.9765750658304433,
	1.976459562632918, 1.9763456545938127, 1.976233308895327, 1.9761224936137443,
	1.9760131776891923, 1.9759053308966201, 1.9757989238179392, 1.97569392781527,
	1.9755903150052492, 1.9754880582343404, 1.9753871310551152, 1.9752875077034486,
	1.9751891630765912, 1.9750920727120846, 1.9749962127674756, 1.9749015600007986,
	1.974808091751787, 1.9747157859237912, 1.974624620966361, 1.9745345758584756,
	1.9744456300923825, 1.9743577636580294, 1.9742709570280557, 1.9741851911433248,
	1.9741004473989767, 1.9740167076309703, 1.973933954103107, 1.973852169494506,
	1.973771336887522, 1.9736914397560734, 1.973612461954384, 1.9735343877061042,
	1.9734572015938032, 1.9733808885488238, 1.9733054338414733, 1.9732308230715456,
	1.9731570421591593, 1.973084077335903, 1.9730119151362668, 1.9729405423893598,
	1.9728699462108963, 1.9728001139954414, 1.9727310334089097, 1.9726626923813,
	1.9725950790996682, 1.972528182001318, 1.9724619897672109, 1.9723964913155805,
	1.97233167579575, 1.9722675325821353, 1.9722040512684433, 1.9721412216620415,
	1.972079033778502, 1.9720174778363146, 1.9719565442517533, 1.9718962236339088,
}
