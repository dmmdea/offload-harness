package eval

import "testing"

func rep(v float64, n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = v
	}
	return s
}

func TestBootstrapDeltaMean(t *testing.T) {
	// Candidate always reaches, incumbent never => delta ~ +1, CI strictly > 0 (ADOPT).
	d, lo, hi := BootstrapDeltaMean(rep(0, 20), rep(1, 20), 2000, 1)
	if d < 0.99 || lo <= 0 {
		t.Errorf("all-better candidate should ADOPT (delta~1, lo>0); got delta=%.3f lo=%.3f hi=%.3f", d, lo, hi)
	}
	// Candidate strictly worse => delta ~ -1, hi < 0 (BLOCK).
	d, lo, hi = BootstrapDeltaMean(rep(1, 20), rep(0, 20), 2000, 1)
	if d > -0.99 || hi >= 0 {
		t.Errorf("all-worse candidate should BLOCK (delta~-1, hi<0); got delta=%.3f lo=%.3f hi=%.3f", d, lo, hi)
	}
	// Identical => delta 0, CI spans 0 (INCONCLUSIVE — no adopt).
	d, lo, hi = BootstrapDeltaMean(rep(1, 20), rep(1, 20), 2000, 1)
	if d != 0 || lo > 0 || hi < 0 {
		t.Errorf("identical systems should be inconclusive (CI spans 0); got delta=%.3f lo=%.3f hi=%.3f", d, lo, hi)
	}
	// Degenerate n<2 => zeros.
	if d, lo, hi = BootstrapDeltaMean([]float64{1}, []float64{0}, 2000, 1); d != 0 || lo != 0 || hi != 0 {
		t.Errorf("n<2 must return zeros; got %.3f %.3f %.3f", d, lo, hi)
	}
}
