package router

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// The inner `agent` row of a local job its caller pinned for measurement names its parent and carries nothing of
// the pin (see ledger.ProductionRows). The router is trained on the traffic it serves, so those rows must not
// teach it; a reader that judges each row alone read them as real.
func TestRouterTrainsWithoutTheInnerRowsOfALabelledJob(t *testing.T) {
	train := func(pin string) *Model {
		rng := rand.New(rand.NewSource(42))
		inner := syntheticEntries("classify", 300, 800, rng)
		var entries []ledger.Entry
		for i := range inner {
			id := fmt.Sprintf("job-%d", i)
			inner[i].ParentJobID = id
			entries = append(entries, ledger.Entry{JobID: id, Task: "classify", PinReason: pin}, inner[i])
		}
		op := filepath.Join(t.TempDir(), "w.json")
		if _, err := Train(writeLedger(t, entries), "", op, testEntryTier); err != nil {
			t.Fatalf("Train: %v", err)
		}
		return Load(op)
	}
	long := makeFeat(1900)
	if m := train(""); m == nil || !m.PreferLargerEntry("classify", long) {
		t.Fatal("control: a router trained on the inner rows of unpinned jobs must prefer the larger entry for a long input")
	}
	if m := train("measurement"); m != nil && m.PreferLargerEntry("classify", long) {
		t.Fatal("a router trained on the inner rows of measurement-pinned jobs learned from them")
	}
}
