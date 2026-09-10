package placement

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

func tripleLayer(t *testing.T) (config.LayerSpec, config.LayerSeat) {
	t.Helper()
	cfg := config.CompositeFixture()
	l, ok := cfg.Layer("triple")
	if !ok {
		t.Fatal("fixture has no triple layer")
	}
	s, ok := cfg.LayerSeat("triple", "long")
	if !ok {
		t.Fatal("fixture triple has no long seat")
	}
	return l, s
}

func TestLayerAdmissibleRunsGuardsInDeclaredOrderAndFirstRefusalWins(t *testing.T) {
	l, s := tripleLayer(t)
	// Every guard would refuse; the FIRST declared one (display_floor) is named.
	f := admitting()
	f.free["1"] = 1
	f.host = 1
	f.pres = &Presence{Mode: "present", Known: true}
	ok, reason, guard := LayerAdmissible(l, s, f.live(), nil)
	if ok || guard != "display_floor" {
		t.Fatalf("first declared refusal wins: ok=%v guard=%q reason=%q", ok, guard, reason)
	}
	// Reorder the guards: presence first now wins.
	l.Guards = []string{"presence", "host_ram", "display_floor"}
	if ok, _, guard := LayerAdmissible(l, s, f.live(), nil); ok || guard != "presence" {
		t.Fatalf("declared order decides which guard is named, got ok=%v guard=%q", ok, guard)
	}
	// All pass: the reason carries every guard's reading, in order.
	l.Guards = []string{"display_floor", "host_ram", "presence"}
	ok, reason, guard = LayerAdmissible(l, s, admitting().live(), nil)
	if !ok || guard != "" {
		t.Fatalf("all pass: ok=%v guard=%q reason=%q", ok, guard, reason)
	}
	i, j, k := strings.Index(reason, "display_floor"), strings.Index(reason, "host_ram"), strings.Index(reason, "presence")
	if i < 0 || j < i || k < j {
		t.Fatalf("readings must be listed in declared order: %q", reason)
	}
	// No guards: admissible, and the reason says so.
	l.Guards = nil
	if ok, reason, _ := LayerAdmissible(l, s, Live{}, nil); !ok || !strings.Contains(reason, "no guards") {
		t.Fatalf("an unguarded layer is admissible without readers, got ok=%v %q", ok, reason)
	}
	// An unknown guard name refuses (config rejects it at load; here it is defended).
	l.Guards = []string{"moon_phase"}
	if ok, _, guard := LayerAdmissible(l, s, admitting().live(), nil); ok || guard != "moon_phase" {
		t.Fatalf("an unknown guard fails closed, got ok=%v guard=%q", ok, guard)
	}
}

func TestRowVerdictStandsInOnlyForANilReader(t *testing.T) {
	l, s := tripleLayer(t)
	yes, no := true, false
	// No readers + the node's verdict true → admitted on the verdict.
	ok, reason, _ := LayerAdmissible(l, s, Live{}, &yes)
	if !ok || !strings.Contains(reason, "node's own verdict") {
		t.Fatalf("nil readers defer to the row verdict, got ok=%v %q", ok, reason)
	}
	// No readers + verdict false → refused, naming the first guard.
	ok, _, guard := LayerAdmissible(l, s, Live{}, &no)
	if ok || guard != "display_floor" {
		t.Fatalf("a false verdict refuses on the first declared guard, got ok=%v guard=%q", ok, guard)
	}
	// A LIVE reader that refuses beats a true verdict: local readings always win.
	f := admitting()
	f.free["1"] = 1
	if ok, _, guard := LayerAdmissible(l, s, f.live(), &yes); ok || guard != "display_floor" {
		t.Fatalf("a live refusal is never overridden by a row verdict, got ok=%v guard=%q", ok, guard)
	}
	// Mixed: only the display reader is live and passes; host/presence fall to the verdict.
	partial := fakeLive{free: map[string]float64{"1": 15}, index: map[string]string{}}
	if ok, reason, _ := LayerAdmissible(l, s, partial.live(), &yes); !ok || !strings.Contains(reason, "15.0") || !strings.Contains(reason, "node's own verdict") {
		t.Fatalf("per-guard: live where readable, verdict where not, got ok=%v %q", ok, reason)
	}
	// No readers, no verdict → refuse (fail closed).
	if ok, reason, _ := LayerAdmissible(l, s, Live{}, nil); ok || !strings.Contains(reason, "unreadable") {
		t.Fatalf("no reader and no verdict must refuse, got ok=%v %q", ok, reason)
	}
}

func TestASeatOffTheDisplayCardPassesTheFloorWithoutAFootprint(t *testing.T) {
	l, _ := tripleLayer(t)
	l.Guards = []string{"display_floor"}
	// A seat pinned to 0,2 on a display_floor-guarded layer never touches the
	// display card: free − 0 ≥ floor is the honest arithmetic for it.
	s := config.LayerSeat{Role: "agent", Model: "m", Device: "0,2", CtxTokens: 1000}
	f := admitting()
	f.free["1"] = 4.5
	ok, reason, guard := LayerAdmissible(l, s, f.live(), nil)
	if !ok {
		t.Fatalf("off-display seat with 4.5 free ≥ 4 must pass, got guard=%q %q", guard, reason)
	}
	// But a UUID display pin that cannot be resolved refuses — the seat may be on it.
	l.DisplayDevice = "GPU-2a44210f"
	f.free["GPU-2a44210f"] = 4.5
	f.index = nil // no resolver
	if ok, reason, guard := LayerAdmissible(l, s, f.live(), nil); ok || guard != "display_floor" || !strings.Contains(reason, "resolve") {
		t.Fatalf("unresolvable UUID pin fails closed, got ok=%v guard=%q %q", ok, guard, reason)
	}
}

func TestHostRAMGuardRefusesAnUndeclaredNeed(t *testing.T) {
	l, s := tripleLayer(t)
	s.HostRAMGiB = 0
	if ok, reason, guard := LayerAdmissible(l, s, admitting().live(), nil); ok || guard != "host_ram" || !strings.Contains(reason, "undeclared") {
		t.Fatalf("free ≥ 0 is not a guard: an undeclared need refuses, got ok=%v guard=%q %q", ok, guard, reason)
	}
}
