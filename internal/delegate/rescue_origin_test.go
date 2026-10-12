package delegate

import (
	"context"
	"errors"
	"strings"
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
	// The node's re-pack was never sent: the time left could not buy it (class budget). Its seat
	// did nothing wrong and holds the answer hot.
	wire.SchemaMiss, wire.DeferClass = true, core.DeferClassBudget
	wire.Reason = "structured re-pack skipped: 4 s left to the call's deadline buys 403 tokens < the answer's 1064"
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

// originHanded runs one rescue over a node whose defer is wire and reports the origin the rescue was
// handed (ok false: none).
func originHanded(t *testing.T, wire core.AgentWireResult) (RescueOrigin, bool) {
	t.Helper()
	var seen atomic.Value // RescueOrigin
	var found atomic.Bool
	rescue := func(ctx context.Context, c core.AgentContract, output string, budget time.Duration) (Rescued, error) {
		o, ok := RescueOriginFrom(ctx)
		found.Store(ok)
		seen.Store(o)
		return Rescued{}, errors.New("not under test")
	}
	// An abstention is re-placed after the rescue fails: the retry may go local, and here it fails.
	local := func(context.Context, core.AgentContract, LocalOptions) (core.AgentWireResult, error) {
		return core.AgentWireResult{}, errors.New("no local seat in this test")
	}
	runRescuedWith(t, rescueNode(t, wire), rescueContract(), rescue, local)
	o, _ := seen.Load().(RescueOrigin)
	return o, found.Load()
}

// The node's own re-pack failed because its SEAT did: it stalled, was unreachable, or was busy past
// the wait (class infrastructure; the node sets schema_miss on every failure arm of its re-pack, this
// one included). The seat that just wedged is the last place to send the rescue first: the origin
// call would spend the rescue's whole budget (its allowance is at least the 120 s flat bound, and
// the budget floors at 120 s) and leave nothing for this box's own seat. Every other class keeps the
// origin: the seat answered, or was never asked.
func TestRescueIsNotHandedTheNodeWhoseSeatFailedTheRepack(t *testing.T) {
	for _, tc := range []struct {
		name, class, reason string
		wantOrigin          bool
	}{
		{"the re-pack stalled", core.DeferClassInfrastructure, "structured re-pack unreachable: stalled: no progress for 120s in repack (allowed 120s; 661 tok so far)", false},
		{"the seat went down", core.DeferClassInfrastructure, "structured re-pack unreachable: Post \"http://seat/v1/chat/completions\": connection refused", false},
		{"the time left could not buy it", core.DeferClassBudget, "structured re-pack skipped: 4 s left to the call's deadline buys 403 tokens < the answer's 1064", true},
		{"the seat answered and the object failed the schema", core.DeferClassAbstention, "output failed schema: stale: expected array", true},
		{"an older node that names no class", "", "structured re-pack unreachable: stalled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := legacyRepackStall()
			wire.SchemaMiss, wire.DeferClass, wire.Reason = true, tc.class, tc.reason
			o, ok := originHanded(t, wire)
			if ok != tc.wantOrigin {
				t.Fatalf("class %q: origin handed = %v (%+v), want %v", tc.class, ok, o, tc.wantOrigin)
			}
		})
	}
}

// A wire result that names no seat is placed on the node's advertised agent seat (run.go fills
// PlacedResult.Seat before the result is finished), and the rescue's origin reads the same one: a
// result with an empty seat used to skip the origin rung with no word.
func TestRescueOriginFallsBackToThePlacedSeatWhenTheWireNamesNone(t *testing.T) {
	wire := legacyRepackStall()
	wire.SchemaMiss, wire.DeferClass = true, core.DeferClassBudget
	wire.Reason = "structured re-pack skipped: 4 s left to the call's deadline buys 403 tokens < the answer's 1064"
	wire.Seat = ""
	o, ok := originHanded(t, wire)
	if !ok {
		t.Fatalf("a result with no seat of its own was handed no origin: the rescue skips the node that wrote the answer")
	}
	if o.Seat != "remote-seat" {
		t.Fatalf("origin seat = %q, want the node's advertised agent seat %q", o.Seat, "remote-seat")
	}
}

// A contract whose output_schema does not parse cannot be read against its answer, and used to
// say nothing: the fidelity check was skipped and the result went on as if it had passed.
func TestListFidelityFailureSaysWhenTheSchemaWillNotParse(t *testing.T) {
	logged := captureLog(t)
	c := rescueContract()
	c.OutputSchema = []byte(`{"properties": {"stale": [`)
	if got := listFidelityFailure(c, []byte(`{"stale":[]}`), `{"outdated":["a","b"]}`); got != "" {
		t.Fatalf("finding = %q, want none: an unreadable schema judges nothing", got)
	}
	if !strings.Contains(logged.String(), "output_schema") || !strings.Contains(logged.String(), "not read against its answer") {
		t.Fatalf("log = %q, want the skipped check named", logged.String())
	}
}
