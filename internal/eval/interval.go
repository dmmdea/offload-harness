package eval

import "math"

// The gate's primary interval.
//
// The primary is the mean of the paired task differences (candidate - incumbent) with a two-sided
// 95% interval, read in the SCORE FORM of the Student-t interval:
//
//	delta = mean(d)                  sn = sqrt(sum((d - delta)^2) / n)
//	c     = t(n-1, .975)             half = c * sn / sqrt(n - c^2)
//	lo, hi = delta -+ half
//
// It is the confidence set of the statistic (delta - mu) / sqrt(mean((d - mu)^2) / n), whose
// variance is taken at the boundary mu instead of at the estimate. The plain t interval takes the
// standard error at the estimate, and on the data the gate sees most (pass/fail scores where the
// two arms differ on a handful of tasks) that standard error is both small and noisy: a record of
// "two wins and no losses in 20 tasks" gives a lower end above -0.05 although the true loss rate
// was 13.5%. The score form is the t interval with its critical value multiplied by
// sqrt((n-1) / (n-c^2)), 1.14 at 16 tasks, 1.10 at 20, 1.08 at 24, 1.04 at 40 and 1.02 at 80. The
// measured false-ADOPT rates of this and of the four other candidate intervals, on the same
// datasets, are in the ADR and reproduce with TestCalibrationStudy (gate_sim_test.go). The form
// has nothing to read when every difference is equal, and zeroSpreadInterval takes that case.
//
// Where it is not calibrated: a strong incumbent and a candidate at exactly the margin whose
// disagreements are a handful of tasks in 60 or more (pass rates .99 and .94 at 80 tasks measure
// 4.8%, a candidate that only loses 5% of the incumbent's passes 8.2% at 80). The ADR has the
// figures and the interval that does clear them.
//
// BootstrapDeltaMean (the percentile bootstrap, B and seed from the call site) is unchanged for
// its legacy callers; the gate reads the efficiency metrics with it, not the primary.

// primaryInterval returns the mean paired difference and the lower and upper ends of its 95%
// interval over aligned per-task scores, which lie in a range of width span (the primary's score
// range). With fewer than two tasks, or unaligned input, there is nothing to conclude and it
// returns zeros, as BootstrapDeltaMean does. A sum that overflows gives a number that is not
// finite, which the caller refuses.
//
// When every difference is equal (to within a rounding error of the scores) there is no spread to
// read a standard error from, and a point interval would certify anything: sixteen ties in a row
// are what a candidate that loses 5% of the incumbent's passes produces 44% of the time. The
// interval is then the rule of three, see zeroSpreadInterval.
func primaryInterval(inc, cand []float64, span float64) (delta, lo, hi float64) {
	n := len(inc)
	if n != len(cand) || n < 2 {
		return 0, 0, 0
	}
	fn := float64(n)
	var tol float64
	if span > 0 && finite(span) {
		tol = equalTolerance * span
	}
	var sum float64
	first := cand[0] - inc[0]
	same := true
	for i := range inc {
		d := cand[i] - inc[i]
		sum += d
		if math.Abs(d-first) > tol {
			same = false
		}
	}
	delta = sum / fn
	if !finite(delta) {
		return delta, delta, delta
	}
	if same {
		lo, hi = zeroSpreadInterval(first, n, span)
		return first, lo, hi
	}
	var ss float64
	for i := range inc {
		e := cand[i] - inc[i] - delta
		ss += e * e
	}
	c := tCritical975(n - 1)
	// For n <= c^2 + 1 (six tasks or fewer) the form has no room: the denominator is held at 1.
	half := c * math.Sqrt(ss/fn) / math.Sqrt(math.Max(fn-c*c, 1))
	return delta, delta - half, delta + half
}

// equalTolerance is how far apart, as a share of the score range, two paired differences can be and
// still count as the same one: the rounding of float scores (0.3 - 0.2 against 0.2 - 0.1) is not a
// spread.
const equalTolerance = 1e-9

// ruleOfThree is the numerator of the rule of three: with no event in n trials, the one-sided 95%
// upper bound on the rate of events is 1 - 0.05^(1/n), which is 3/n to within 1% from 60 trials.
const ruleOfThree = 3.0

// zeroSpreadInterval is the interval of n paired differences that all equal c, each lying in
// [-span, span], where span is the width of the score range: [c - 3 span / n, c + 3 span / n],
// clipped to [-span, span], the range the mean of such differences cannot leave.
//
// It is the rule of three. For pass/fail scores with no discordant pair among n, 3/n is the
// one-sided 95% upper bound on the rate of discordant pairs, so the mean difference can be as low
// as -3/n (every discordant pair a loss) or as high as +3/n. For any other score the same width is
// the usual allowance for a sample that cannot show its own spread, scaled by the range. A tie
// (c = 0) at 16 pairs is therefore [-0.1875, 0.1875] on a unit range, [-0.075, 0.075] at 40 and
// [-0.0375, 0.0375] at 80: a margin of 0.05 is certified from 61 ties and not from 60, whose end
// sits exactly on it. Both ends move, so a superiority tie (c = 0) is INCONCLUSIVE at any size and
// the incumbent keeps every tie, while a constant difference far from zero still decides: every
// pair a win of the whole range (c = span) is [span - 3 span / n, span] and adopts, every pair a
// loss of it blocks. A span that is not a positive number leaves the single value.
func zeroSpreadInterval(c float64, n int, span float64) (lo, hi float64) {
	if !(span > 0) || !finite(span) || !finite(c) || n < 1 {
		return c, c
	}
	half := ruleOfThree * span / float64(n)
	return math.Min(c, math.Max(-span, c-half)), math.Max(c, math.Min(span, c+half))
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
