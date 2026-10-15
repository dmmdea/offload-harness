package compeval

import (
	"reflect"
	"testing"
)

// The expected values below were produced by CPython's `random.Random(42)` (and
// `random.Random(2**40+7)`) and are the contract newPyRand exists to honour.
func TestPyRandMatchesCPythonStream(t *testing.T) {
	r := newPyRand(42)
	if got := r.random(); got != 0.6394267984578837 {
		t.Fatalf("random() = %v", got)
	}
	if got := r.getrandbits(5); got != 0 {
		t.Fatalf("getrandbits(5) = %d", got)
	}
	if got := r.randint(4, 9); got != 9 {
		t.Fatalf("randint(4,9) = %d", got)
	}
	if got := r.choiceIndex(7); got != 2 {
		t.Fatalf("choice index = %d", got)
	}
	x := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	r.shuffleInts(x)
	if want := []int{8, 1, 7, 6, 4, 0, 5, 2, 9, 3}; !reflect.DeepEqual(x, want) {
		t.Fatalf("shuffle = %v want %v", x, want)
	}
	// 7 of 100 takes the selection-set strategy, 4 of 10 the pool strategy.
	if got, want := r.sampleIndexes(100, 7), []int{4, 3, 11, 27, 29, 64, 77}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sample(100,7) = %v want %v", got, want)
	}
	if got, want := r.sampleIndexes(10, 4), []int{0, 8, 3, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sample(10,4) = %v want %v", got, want)
	}
	var picks []int
	for i := 0; i < 5; i++ {
		picks = append(picks, r.weightedIndex([]float64{0.6, 0.25, 0.15}))
	}
	if want := []int{1, 0, 0, 0, 1}; !reflect.DeepEqual(picks, want) {
		t.Fatalf("choices = %v want %v", picks, want)
	}
}

func TestPyRandSeedLargerThan32Bits(t *testing.T) {
	if got := newPyRand(1<<40 + 7).random(); got != 0.6137037779936511 {
		t.Fatalf("random() for a 41-bit seed = %v", got)
	}
}
