package delegate

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The delegator's rescue of a finished answer used to know only the answer. The node that wrote it
// holds its seat hot, and a rescue that can re-pack there does not depend on this box's own seat
// being resident or its cards being free (the recorded rescue found its seat cold under another
// session's lease and was cut at its 120 s allowance). The rescue is handed that node and seat in
// its context, so the RescueFunc signature and every door that wires one stay as they are.
func TestRescueIsHandedTheNodeThatWroteTheAnswer(t *testing.T) {
	wire := legacyRepackStall()
	base := rescueNode(t, wire)
	var seen atomic.Value // RescueOrigin
	var found atomic.Bool
	rescue := func(ctx context.Context, c core.AgentContract, output string, budget time.Duration) (Rescued, error) {
		o, ok := RescueOriginFrom(ctx)
		found.Store(ok)
		seen.Store(o)
		return Rescued{}, errors.New("not under test")
	}
	runRescued(t, base, rescueContract(), rescue)
	if !found.Load() {
		t.Fatalf("the rescue was handed no origin: it cannot re-pack on the node that wrote the answer")
	}
	o, _ := seen.Load().(RescueOrigin)
	if o.Base != base || o.Seat != wire.Seat || o.Node != wire.NodeID {
		t.Fatalf("origin = %+v, want the node %q, its seat %q and id %q", o, base, wire.Seat, wire.NodeID)
	}
}

// A context with no origin, or an origin that names nothing, reads as none.
func TestRescueOriginFromReadsNothingWithoutAnOrigin(t *testing.T) {
	if _, ok := RescueOriginFrom(context.Background()); ok {
		t.Fatal("a bare context carries an origin")
	}
	if _, ok := RescueOriginFrom(WithRescueOrigin(context.Background(), RescueOrigin{Base: "http://127.0.0.1:1"})); ok {
		t.Fatal("an origin with no seat was read as one")
	}
	if o, ok := RescueOriginFrom(WithRescueOrigin(context.Background(), RescueOrigin{Base: "http://127.0.0.1:1", Seat: "s", Node: "n"})); !ok || o.Seat != "s" {
		t.Fatalf("origin = %+v ok=%v", o, ok)
	}
}
