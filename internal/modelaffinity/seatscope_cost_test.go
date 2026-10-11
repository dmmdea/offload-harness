package modelaffinity

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// COST, as the docs say it (register C-86a). A seat's pins now include the serving config, which costs a
// Stat of the file per lookup, so WHO looks them up and WHEN is part of the contract:
//
//   - the fence's own readers (ScopeToModel, so SeatLease, Admit, AwaitUpstream and AwaitRunSlot) ask
//     them only under a held lease that names cards, so an idle box and a whole-node lease never pay;
//   - PinsFor, and every caller of Config.ModelPins that does not go through this package's lease
//     reads (the agent run's recorded cards, the text and vision auto routes, the run-cap line), ask on
//     every call, an idle box included. They are not a fence, and they have always needed the answer.
//
// TestSeatPinsAreNotLookedUpWhenNoLeaseNamesCards pins the first half through Admit; this pins the
// other fence readers and the second half, so that neither a comment nor a doc can claim more than the
// code does.
func TestOnlyTheFencesOwnLookupsWaitForALeaseThatNamesCards(t *testing.T) {
	m := armLease(t) // not card-scoped: the only lease it can hold is a whole-node one
	var lookups atomic.Int64
	SetSeatPins(func(model string) ([]string, bool) {
		lookups.Add(1)
		p, ok := tripleBoxPins[model]
		return p, ok
	})
	prev := readCardTable
	readCardTable = func(context.Context, string) ([]gpuprobe.Card, string, error) { return scopeCards(), "", nil }
	resetCardMemo()
	t.Cleanup(func() {
		SetSeatPins(nil)
		readCardTable = prev
		resetCardMemo()
	})

	fenceReaders := func() {
		ScopeToModel(InspectLease(gpuLeaseDir()), "seat-card0")
		SeatLease("seat-card0")
		// A whole-node lease cordons the run slot; the answer is not what this test reads.
		_ = AwaitRunSlot(context.Background(), "http://cost-fence", "seat-card0", time.Now())
	}

	fenceReaders()
	if n := lookups.Load(); n != 0 {
		t.Fatalf("on an idle box the fence's own readers looked a seat's pins up %d time(s)", n)
	}
	// The other readers ask regardless: this is the Stat per call the docs name.
	PinsFor("seat-card0")
	PinsFor("seat-card0")
	if n := lookups.Load(); n != 2 {
		t.Fatalf("PinsFor looked the pins up %d time(s) over two calls on an idle box, want 2: it is not gated on a lease", n)
	}

	before := lookups.Load()
	l := holdLease(t, m, gpulease.ClassMedia, gpulease.Options{Reason: "legacy render"})
	fenceReaders()
	if n := lookups.Load() - before; n != 0 {
		t.Fatalf("under a whole-node lease the fence's own readers looked a seat's pins up %d time(s): it touches every seat whatever its pin", n)
	}
	_ = l.Release()

	m.SetCardScoped(true)
	mediaLeaseOn(t, m, idCard2)
	before = lookups.Load()
	SeatLease("seat-card0")
	if lookups.Load() == before {
		t.Fatal("under a lease that names cards the fence's own readers must look the seat's pins up, and did not")
	}
}
