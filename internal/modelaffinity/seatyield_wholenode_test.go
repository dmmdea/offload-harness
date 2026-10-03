package modelaffinity

import (
	"context"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// TestAWholeNodeLeaseNeverYieldsASeat: the seat race rule (P4) unloads a seat only for a
// lease that sits on the seat's CARDS. A whole-node lease on a host with card-scoped
// leases off fences what it always fenced and unloads nothing, as in 0.160.0 (the 0.161.0
// release recheck found the yield firing flag-independently).
func TestAWholeNodeLeaseNeverYieldsASeat(t *testing.T) {
	m := armLease(t) // card-scoped writer OFF, no seat pins armed, legacy inference OFF
	f := armYield(t, map[string]int{"seat-x": 0})
	tk, err := Admit(context.Background(), "http://whole-node-yield", "seat-x", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	l, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "render", Origin: "pipeline"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	tk.Release()
	if len(f.unloads()) != 0 {
		t.Errorf("a whole-node lease on a flag-off host made a seat unload itself: %v", f.unloads())
	}
}
