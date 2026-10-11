package router

import (
	"math/rand"
	"path/filepath"
	"testing"
)

// Register F48: the router is trained on the traffic it serves. A bake's rows (a fixture's features and
// escalations) must not teach it. The control shows the model prefers the larger entry when the rows are real.
func TestRouterTrainsOnProductionRowsOnly(t *testing.T) {
	train := func(class string) *Model {
		rng := rand.New(rand.NewSource(42))
		entries := syntheticEntries("classify", 300, 800, rng)
		for i := range entries {
			entries[i].RunClass = class
		}
		op := filepath.Join(t.TempDir(), "w.json")
		if _, err := Train(writeLedger(t, entries), "", op, testEntryTier); err != nil {
			t.Fatalf("Train: %v", err)
		}
		return Load(op)
	}
	long := makeFeat(1900)
	if m := train(""); m == nil || !m.PreferLargerEntry("classify", long) {
		t.Fatal("control: a router trained on real rows must prefer the larger entry for a long input")
	}
	if m := train("bake"); m != nil && m.PreferLargerEntry("classify", long) {
		t.Fatal("a router trained on a bake's rows learned from them")
	}
}
