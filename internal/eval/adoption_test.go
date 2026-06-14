package eval

import (
	"testing"
)

// --- AUGRC ------------------------------------------------------------------

func TestAUGRC_PerfectBeatsReversed(t *testing.T) {
	perfect := []RCPoint{
		{Confidence: 0.9, Correct: true}, {Confidence: 0.8, Correct: true},
		{Confidence: 0.4, Correct: false}, {Confidence: 0.3, Correct: false},
	}
	reversed := []RCPoint{
		{Confidence: 0.9, Correct: false}, {Confidence: 0.8, Correct: false},
		{Confidence: 0.4, Correct: true}, {Confidence: 0.3, Correct: true},
	}
	a := AUGRC(perfect)
	b := AUGRC(reversed)
	if !(a < b) {
		t.Fatalf("perfect ranking should have lower AUGRC than reversed: perfect=%v reversed=%v", a, b)
	}
}

func TestAUGRC_MonotoneInErrorRate(t *testing.T) {
	// Confidence is constant so ranking is fixed; only the error rate changes.
	mk := func(nWrong int) []RCPoint {
		pts := make([]RCPoint, 4)
		for i := range pts {
			pts[i] = RCPoint{Confidence: 0.5, Correct: i >= nWrong}
		}
		return pts
	}
	a0 := AUGRC(mk(0))   // 0% error
	a50 := AUGRC(mk(2))  // 50% error
	a100 := AUGRC(mk(4)) // 100% error
	if !(a0 < a50 && a50 < a100) {
		t.Fatalf("AUGRC must rise with error rate: 0%%=%v 50%%=%v 100%%=%v", a0, a50, a100)
	}
	if a0 != 0 {
		t.Fatalf("0%% error => AUGRC 0, got %v", a0)
	}
}

func TestAUGRC_Empty(t *testing.T) {
	if AUGRC(nil) != 0 {
		t.Fatal("empty AUGRC should be 0")
	}
}

// --- BootstrapDeltaAURC -----------------------------------------------------

func TestBootstrapDeltaAURC_HeadStrictlyBetter(t *testing.T) {
	// headConf perfectly ranks correctness; incConf is constant (no skill).
	n := 40
	inc := make([]float64, n)
	head := make([]float64, n)
	correct := make([]bool, n)
	for i := 0; i < n; i++ {
		inc[i] = 0.5
		correct[i] = i < n/2
		if correct[i] {
			head[i] = 0.9 // correct ranked high
		} else {
			head[i] = 0.1
		}
	}
	delta, lo, hi := BootstrapDeltaAURC(inc, head, correct, 1000, 1)
	if !(delta > 0) {
		t.Fatalf("head better => delta>0, got %v", delta)
	}
	if !(lo > 0) {
		t.Fatalf("head provably better => lo>0 (CI excludes zero), got lo=%v hi=%v", lo, hi)
	}
}

func TestBootstrapDeltaAURC_IdenticalSpansZero(t *testing.T) {
	n := 40
	inc := make([]float64, n)
	head := make([]float64, n)
	correct := make([]bool, n)
	for i := 0; i < n; i++ {
		inc[i] = float64(i) / float64(n)
		head[i] = inc[i] // identical
		correct[i] = i%2 == 0
	}
	_, lo, hi := BootstrapDeltaAURC(inc, head, correct, 1000, 1)
	if !(lo <= 0 && 0 <= hi) {
		t.Fatalf("identical head => CI spans 0, got lo=%v hi=%v", lo, hi)
	}
}

func TestBootstrapDeltaAURC_Deterministic(t *testing.T) {
	n := 30
	inc := make([]float64, n)
	head := make([]float64, n)
	correct := make([]bool, n)
	for i := 0; i < n; i++ {
		inc[i] = 0.5
		head[i] = float64(i) / float64(n)
		correct[i] = i%3 == 0
	}
	d1, lo1, hi1 := BootstrapDeltaAURC(inc, head, correct, 500, 7)
	d2, lo2, hi2 := BootstrapDeltaAURC(inc, head, correct, 500, 7)
	if d1 != d2 || lo1 != lo2 || hi1 != hi2 {
		t.Fatalf("same seed must be deterministic: (%v,%v,%v) vs (%v,%v,%v)", d1, lo1, hi1, d2, lo2, hi2)
	}
}

func TestBootstrapDeltaAURC_Guards(t *testing.T) {
	d, lo, hi := BootstrapDeltaAURC([]float64{0.5}, []float64{0.5}, []bool{true}, 100, 1)
	if d != 0 || lo != 0 || hi != 0 {
		t.Fatalf("N<2 must return zeros, got %v %v %v", d, lo, hi)
	}
	d, lo, hi = BootstrapDeltaAURC([]float64{0.5, 0.5}, []float64{0.5, 0.5}, []bool{true, false}, 0, 1)
	if d != 0 || lo != 0 || hi != 0 {
		t.Fatalf("B<1 must return zeros, got %v %v %v", d, lo, hi)
	}
}

// --- KFoldOOF ---------------------------------------------------------------

func TestKFoldOOF_NoSelfLeakAndAllScored(t *testing.T) {
	n := 20
	labels := make([]float64, n)
	for i := range labels {
		labels[i] = float64(i)
	}
	// fit returns the mean label of the TRAINING indices; score returns it.
	// Because training never includes the held-out item, the OOF score for item i
	// must differ from training on all-but-i only via the mean — and crucially the
	// fit must never have seen i's own label. We assert that by checking the model
	// (the train mean) excludes i: a leak would make the mean include labels[i].
	type model struct {
		mean      float64
		trainHasI map[int]bool
	}
	scores := KFoldOOF(n, 5, 42,
		func(train []int) model {
			has := map[int]bool{}
			sum := 0.0
			for _, t := range train {
				sum += labels[t]
				has[t] = true
			}
			return model{mean: sum / float64(len(train)), trainHasI: has}
		},
		func(m model, i int) float64 {
			if m.trainHasI[i] {
				t.Fatalf("LEAK: item %d was in its own training fold", i)
			}
			return m.mean
		},
	)
	if len(scores) != n {
		t.Fatalf("want %d OOF scores, got %d", n, len(scores))
	}
	for i, s := range scores {
		if s == 0 && i != 0 { // every item gets a real (non-default) score
			// mean of a non-trivial subset is effectively never exactly 0 here
			t.Fatalf("item %d got no OOF score (0)", i)
		}
	}
}

func TestKFoldOOF_KClamp(t *testing.T) {
	// k>n is clamped to n (leave-one-out); still scores all items.
	scores := KFoldOOF(3, 10, 1,
		func(train []int) int { return len(train) },
		func(m int, i int) float64 { return float64(m) },
	)
	if len(scores) != 3 {
		t.Fatalf("want 3 scores, got %d", len(scores))
	}
	for i, s := range scores {
		if s != 2 { // k=n => leave-one-out => each train has n-1=2 items
			t.Fatalf("item %d: leave-one-out train size should be 2, got %v", i, s)
		}
	}
}
