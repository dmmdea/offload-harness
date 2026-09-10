package placement

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// fakeLive is the test's machine: every reader answers from a map, and a
// reader that is not set stays nil so the fail-closed branches are reachable.
type fakeLive struct {
	seats  map[string]SeatState // "layer/role" → state
	free   map[string]float64   // device key (index or UUID prefix) → free GiB
	index  map[string]string    // UUID prefix → CUDA index
	host   float64
	hostOK bool
	pres   *Presence
}

func (f fakeLive) live() Live {
	l := Live{}
	if f.seats != nil {
		l.Seat = func(layer, role string) SeatState { return f.seats[layer+"/"+role] }
	}
	if f.free != nil {
		l.DeviceFree = func(device string) (float64, bool) { v, ok := f.free[device]; return v, ok }
	}
	if f.index != nil {
		l.DeviceIndex = func(device string) (string, bool) { v, ok := f.index[device]; return v, ok }
	}
	if f.hostOK {
		l.HostFree = func() (float64, bool) { return f.host, true }
	}
	if f.pres != nil {
		p := *f.pres
		l.Presence = func() Presence { return p }
	}
	return l
}

// admitting is a machine on which every triple guard passes: 15 GiB free on
// the display card (15 − 10.5 = 4.5 ≥ 4), 80 GiB host RAM, console locked.
func admitting() fakeLive {
	return fakeLive{
		seats:  map[string]SeatState{},
		free:   map[string]float64{"0": 2, "1": 15, "2": 3},
		index:  map[string]string{},
		host:   80,
		hostOK: true,
		pres:   &Presence{Mode: "auto", Known: true, Away: true, Locked: true, Note: "console session locked"},
	}
}

func layers() []config.LayerSpec { return config.CompositeFixture().Layers }

func agentReq(est int) Request {
	return Request{Class: ClassAgent, EstTokens: est, MaxTokens: 1024, BudgetSec: 300}
}

func TestNoLayersIsTheZeroDecision(t *testing.T) {
	d := Decide(agentReq(1000), nil, admitting().live())
	if !reflect.DeepEqual(d, Decision{}) {
		t.Fatalf("no layers must be the zero decision (non-composite box), got %+v", d)
	}
	if d := Decide(agentReq(1000), []config.LayerSpec{}, Live{}); !reflect.DeepEqual(d, Decision{}) {
		t.Fatalf("empty layers must be the zero decision, got %+v", d)
	}
}

func TestMechanicalIsTheSingleRouterAndNamesTheTimeShare(t *testing.T) {
	req := Request{Class: ClassMechanical, EstTokens: 500, RouteKey: "workhorse"}
	idle := admitting()
	d := Decide(req, layers(), idle.live())
	if d.Defer || d.Wait {
		t.Fatalf("mechanical must never defer or wait: %+v", d)
	}
	if d.Layer != "single" || d.Role != "router" || d.Tier != "blackwell-16" {
		t.Fatalf("mechanical → single/router, got %+v", d.Placed)
	}
	if d.Seat != "" {
		t.Fatalf("on the single layer the cascade keeps its rung: seat must be empty, got %q", d.Seat)
	}
	if !reflect.DeepEqual(d.Devices, []string{"0"}) {
		t.Fatalf("devices = the router seat's pin, got %v", d.Devices)
	}
	if !strings.Contains(d.Reason, "single layer (router rung on device 0)") {
		t.Fatalf("reason must name the rung's device: %q", d.Reason)
	}
	if strings.Contains(d.Reason, "time-shares") || d.Evicts != "" {
		t.Fatalf("pair not loaded: no time-share note, got %q evicts %q", d.Reason, d.Evicts)
	}

	busy := admitting()
	busy.seats["pair/agent"] = SeatState{Known: true, Loaded: true, Inflight: 3}
	d = Decide(req, layers(), busy.live())
	if d.Layer != "single" || !strings.Contains(d.Reason, "the single layer time-shares the pair's cards") {
		t.Fatalf("pair loaded: the reason must name the time-share, got %+v", d.Placed)
	}
	if d.Evicts != "agent-pool" {
		t.Fatalf("evicts must name the pair's agent seat, got %q", d.Evicts)
	}
}

func TestMechanicalUsesTheDisplayLayerOnlyWhenNotDormantAndGuarded(t *testing.T) {
	req := Request{Class: ClassMechanical, EstTokens: 500, RouteKey: "workhorse"}
	busy := admitting()
	busy.seats["pair/agent"] = SeatState{Known: true, Loaded: true, Inflight: 3}

	// Dormant (the shipped default): never chosen, even with the pair busy.
	if d := Decide(req, layers(), busy.live()); d.Layer != "single" {
		t.Fatalf("a dormant display layer must never be chosen, got %+v", d.Placed)
	}

	awake := layers()
	for i := range awake {
		if awake[i].Name == "display" {
			awake[i].Dormant = false
		}
	}
	d := Decide(req, awake, busy.live())
	if d.Layer != "display" || d.Role != "router" || d.Tier != "blackwell-16" {
		t.Fatalf("pair busy + display awake + guards pass → display, got %+v", d.Placed)
	}
	if d.Seat != "gemma-4-e4b-display" {
		t.Fatalf("seat must be the route key's twin from model_map, got %q", d.Seat)
	}
	if !reflect.DeepEqual(d.Devices, []string{"1"}) {
		t.Fatalf("display devices = the twin's pin, got %v", d.Devices)
	}
	if d.Defer || d.Wait {
		t.Fatalf("admitted display must not defer/wait: %+v", d)
	}

	// Pair idle: the single router is admissible, so the display layer is not used.
	if d := Decide(req, awake, admitting().live()); d.Layer != "single" {
		t.Fatalf("pair idle → single even with the display awake, got %+v", d.Placed)
	}

	// Guards refuse (operator at the desk): fall back to single with the guard's reason appended.
	atDesk := busy
	atDesk.pres = &Presence{Mode: "present", Known: true, Away: false, Note: "operator override: present"}
	d = Decide(req, awake, atDesk.live())
	if d.Layer != "single" || d.Defer {
		t.Fatalf("display refused → single, never a defer for mechanical text, got %+v", d)
	}
	if !strings.Contains(d.Reason, "display layer refused") || !strings.Contains(d.Reason, "presence") {
		t.Fatalf("the fallback reason must carry the guard's refusal: %q", d.Reason)
	}
	if d.Seat != "" {
		t.Fatalf("fallback to single keeps the cascade rung (empty seat), got %q", d.Seat)
	}

	// An unknown route key on the display layer is not a twin: fall back to single.
	unknownRoute := req
	unknownRoute.RouteKey = "escalation"
	if d := Decide(unknownRoute, awake, busy.live()); d.Layer != "single" || !strings.Contains(d.Reason, "no twin") {
		t.Fatalf("no twin for the route key → single with the reason, got %+v", d.Placed)
	}
}

func TestOCRAndVisionFollowTheirRoles(t *testing.T) {
	d := Decide(Request{Class: ClassOCR}, layers(), admitting().live())
	if d.Layer != "single" || d.Role != "ocr" || d.Seat != "qwen3-vl-8b" || !reflect.DeepEqual(d.Devices, []string{"2"}) || d.Defer {
		t.Fatalf("ocr → single/ocr on device 2, got %+v", d)
	}
	if d.CtxTokens != 16384 {
		t.Fatalf("ctx_tokens comes from config, got %d", d.CtxTokens)
	}
	d = Decide(Request{Class: ClassVision}, layers(), admitting().live())
	if d.Layer != "pair" || d.Role != "vision" || d.Seat != "qwen3-vl-32b" || !reflect.DeepEqual(d.Devices, []string{"0", "2"}) || d.Defer {
		t.Fatalf("vision → pair/vision on 0,2, got %+v", d)
	}
	// A box without the role defers loudly as a contract problem, never a silent fallthrough.
	noOCR := []config.LayerSpec{{Name: "pair", Tier: "t", Devices: []string{"0,2"}, Seats: []config.LayerSeat{{Role: "agent", Model: "m", Device: "0,2", CtxTokens: 1000}}}}
	if d := Decide(Request{Class: ClassOCR}, noOCR, Live{}); !d.Defer || d.DeferClass != core.DeferClassContract || !strings.Contains(d.Reason, "ocr") {
		t.Fatalf("missing ocr role → defer contract naming the role, got %+v", d)
	}
}

func TestAgentThatFitsThePairRunsThereAndSaturationIsRecordedNotActed(t *testing.T) {
	req := agentReq(50_000)
	d := Decide(req, layers(), admitting().live())
	if d.Layer != "pair" || d.Role != "agent" || d.Seat != "agent-pool" || d.Tier != "blackwell-2x16" {
		t.Fatalf("a fitting contract runs on the pair's agent seat, got %+v", d.Placed)
	}
	if d.Wait || d.Defer || d.Evicts != "" {
		t.Fatalf("pair/agent never waits, defers or evicts: %+v", d)
	}
	if d.CtxTokens != 163840 {
		t.Fatalf("ctx_tokens = the seat's declared window, got %d", d.CtxTokens)
	}
	if strings.Contains(d.Reason, "in flight") {
		t.Fatalf("an idle pair carries no saturation note: %q", d.Reason)
	}

	sat := admitting()
	sat.seats["pair/agent"] = SeatState{Known: true, Loaded: true, Inflight: 32}
	d = Decide(req, layers(), sat.live())
	if d.Layer != "pair" || d.Seat != "agent-pool" || d.Wait || d.Defer {
		t.Fatalf("saturation is recorded, never acted on (R2): got %+v", d)
	}
	if !strings.Contains(d.Reason, "pair at 32/32 in flight — queued in the seat") {
		t.Fatalf("the saturation must be written into the reason: %q", d.Reason)
	}
	// Below the cap: no note.
	sat.seats["pair/agent"] = SeatState{Known: true, Loaded: true, Inflight: 31}
	if d := Decide(req, layers(), sat.live()); strings.Contains(d.Reason, "queued in the seat") {
		t.Fatalf("31/32 is not saturated: %q", d.Reason)
	}
}

func TestWindowOverflowUsesThePairLongSeatOnlyWhenThePairIsIdleElseWaitsNamingTheEviction(t *testing.T) {
	req := agentReq(200_000) // need 201,024 > 163,840, ≤ 262,144

	cold := admitting()
	cold.seats["pair/agent"] = SeatState{Known: true, Loaded: false}
	d := Decide(req, layers(), cold.live())
	if d.Layer != "pair" || d.Role != "long" || d.Seat != "qwen3.8-27b-262k" {
		t.Fatalf("window overflow → the pair's long seat, got %+v", d.Placed)
	}
	if d.Wait || d.Defer || d.Evicts != "" {
		t.Fatalf("pair agent cold: no wait, nothing to evict, got %+v", d)
	}
	if !strings.Contains(d.Reason, "window overflow (need ~201024 > 163840)") || !strings.Contains(d.Reason, "(cold)") {
		t.Fatalf("reason must state the overflow and the cold seat: %q", d.Reason)
	}
	if d.CtxTokens != 262144 {
		t.Fatalf("ctx_tokens = the long seat's window, got %d", d.CtxTokens)
	}

	idle := admitting()
	idle.seats["pair/agent"] = SeatState{Known: true, Loaded: true, Inflight: 0}
	d = Decide(req, layers(), idle.live())
	if d.Layer != "pair" || d.Role != "long" || d.Wait || d.Defer {
		t.Fatalf("pair agent loaded-idle: pair/long, no wait, got %+v", d)
	}
	if d.Evicts != "agent-pool" || !strings.Contains(d.Reason, "evicts agent-pool (idle)") {
		t.Fatalf("an idle loaded agent seat is named as the eviction: %+v", d.Placed)
	}

	busy := admitting()
	busy.seats["pair/agent"] = SeatState{Known: true, Loaded: true, Inflight: 5}
	d = Decide(req, layers(), busy.live())
	if d.Layer != "pair" || d.Role != "long" || !d.Wait || d.Defer {
		t.Fatalf("pair agent busy: pair/long WITH the wait, got %+v", d)
	}
	if d.Evicts != "agent-pool" {
		t.Fatalf("the wait names what it would evict, got %q", d.Evicts)
	}
	if !strings.Contains(d.Reason, "evicts agent-pool (5 in flight): waits for it to drain") {
		t.Fatalf("reason must name the in-flight count and the wait: %q", d.Reason)
	}

	// An explicit long ask does NOT go to the pair's long seat: it asks for the triple (row 4).
	long := req
	long.ContextClass = core.ContextClassLong
	if d := Decide(long, layers(), busy.live()); d.Layer != "triple" && !d.Defer {
		t.Fatalf("context_class long is the triple's row, got %+v", d)
	}
}

func TestExplicitLongAsksForTheTripleUnderFeasibilityAndGuards(t *testing.T) {
	req := Request{Class: ClassAgent, EstTokens: 10_000, MaxTokens: 1024, BudgetSec: 300, ContextClass: core.ContextClassLong}
	d := Decide(req, layers(), admitting().live())
	if d.Defer || d.Wait {
		t.Fatalf("feasible + guards pass → admitted, got %+v", d)
	}
	if d.Layer != "triple" || d.Role != "long" || d.Seat != "qwen3.8-flash-next-262k" || d.Tier != "blackwell-3x16" {
		t.Fatalf("explicit long → triple/long, got %+v", d.Placed)
	}
	if !reflect.DeepEqual(d.Devices, []string{"0", "1", "2"}) {
		t.Fatalf("devices = the seat pin, got %v", d.Devices)
	}
	for _, want := range []string{"11024 tokens", "69 t/s", "display_floor", "15.0", "10.5", "host_ram", "80.0", "presence", "locked"} {
		if !strings.Contains(d.Reason, want) {
			t.Fatalf("an admission carries the readings; missing %q in %q", want, d.Reason)
		}
	}

	// Infeasible: 200k tokens at 69 t/s ≈ 48 min against a 900 s budget → contract defer.
	slow := req
	slow.EstTokens, slow.BudgetSec = 200_000, 900
	d = Decide(slow, layers(), admitting().live())
	if !d.Defer || d.DeferClass != core.DeferClassContract || d.Guard != "" {
		t.Fatalf("infeasible prefill is a CONTRACT defer without a guard, got %+v", d)
	}
	for _, want := range []string{"201024", "69 t/s", "900 s"} {
		if !strings.Contains(d.Reason, want) {
			t.Fatalf("the feasibility defer names tokens, rate, seconds and budget; missing %q in %q", want, d.Reason)
		}
	}
	if d.Layer != "triple" {
		t.Fatalf("the defer names the layer it tried, got %q", d.Layer)
	}

	// Feasible but a guard refuses → capacity defer naming the guard.
	atDesk := admitting()
	atDesk.pres = &Presence{Mode: "auto", Known: true, Away: false, IdleSec: 30, Note: "idle 30 s < threshold"}
	d = Decide(req, layers(), atDesk.live())
	if !d.Defer || d.DeferClass != core.DeferClassCapacity || d.Guard != "presence" || d.Layer != "triple" {
		t.Fatalf("guard refusal is a CAPACITY defer naming the guard, got %+v", d)
	}
	if !strings.Contains(d.Reason, "operator at the desk") {
		t.Fatalf("reason must carry the guard's own words: %q", d.Reason)
	}
	// A budget of 0 reads as the default 300 s.
	noBudget := req
	noBudget.BudgetSec = 0
	if d := Decide(noBudget, layers(), admitting().live()); d.Defer || !strings.Contains(d.Reason, "300 s") {
		t.Fatalf("budget 0 → 300 s default, got %+v", d)
	}
}

func TestExplicitLongWithoutATripleLayerDefersLoudly(t *testing.T) {
	var noTriple []config.LayerSpec
	for _, l := range layers() {
		if l.Name != "triple" {
			noTriple = append(noTriple, l)
		}
	}
	req := Request{Class: ClassAgent, EstTokens: 1000, MaxTokens: 100, ContextClass: core.ContextClassLong}
	d := Decide(req, noTriple, admitting().live())
	if !d.Defer || d.DeferClass != core.DeferClassContract {
		t.Fatalf("no triple layer → contract defer, never a fallthrough to the pair, got %+v", d)
	}
	if !strings.Contains(d.Reason, "no layer serves context_class long on this box") {
		t.Fatalf("reason: %q", d.Reason)
	}
}

func TestTripleIsRefusedByNameForEachGuard(t *testing.T) {
	req := Request{Class: ClassAgent, EstTokens: 10_000, MaxTokens: 1024, BudgetSec: 300, ContextClass: core.ContextClassLong}
	cases := []struct {
		name   string
		live   func() fakeLive
		layers func() []config.LayerSpec
		guard  string
		reason string
	}{
		{"floor with footprint arithmetic: 14.4 − 10.5 = 3.9 < 4", func() fakeLive { f := admitting(); f.free["1"] = 14.4; return f }, layers, "display_floor", "3.9"},
		{"undeclared footprint on the UUID-pinned display device", func() fakeLive {
			f := admitting()
			f.free["GPU-2a44210f"] = 15
			f.index["GPU-2a44210f"] = "1"
			return f
		}, func() []config.LayerSpec {
			ls := layers()
			for i := range ls {
				if ls[i].Name == "triple" {
					ls[i].DisplayDevice = "GPU-2a44210f"
					ls[i].Seats[0].DisplayFootprintGiB = 0
				}
			}
			return ls
		}, "display_floor", "display footprint undeclared"},
		{"host RAM 60 < 70", func() fakeLive { f := admitting(); f.host = 60; return f }, layers, "host_ram", "60.0"},
		{"presence present (the default)", func() fakeLive {
			f := admitting()
			f.pres = &Presence{Mode: "present", Known: true, Away: false, Note: "operator override: present"}
			return f
		}, layers, "presence", "present"},
		{"presence auto, operator at the desk", func() fakeLive {
			f := admitting()
			f.pres = &Presence{Mode: "auto", Known: true, Away: false, IdleSec: 12, Note: "idle 12 s < threshold"}
			return f
		}, layers, "presence", "operator at the desk"},
		{"presence auto, probe unknown", func() fakeLive {
			f := admitting()
			f.pres = &Presence{Mode: "auto", Known: false, Note: "no console session"}
			return f
		}, layers, "presence", "presence unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(req, tc.layers(), tc.live().live())
			if !d.Defer || d.DeferClass != core.DeferClassCapacity {
				t.Fatalf("want capacity defer, got %+v", d)
			}
			if d.Guard != tc.guard {
				t.Fatalf("guard: want %q got %q (%q)", tc.guard, d.Guard, d.Reason)
			}
			if !strings.Contains(d.Reason, tc.reason) {
				t.Fatalf("reason must contain %q: %q", tc.reason, d.Reason)
			}
		})
	}
	// The UUID-pinned display device with a DECLARED footprint admits — the
	// arithmetic runs on the resolved card.
	f := admitting()
	f.free["GPU-2a44210f"] = 15
	f.index["GPU-2a44210f"] = "1"
	ls := layers()
	for i := range ls {
		if ls[i].Name == "triple" {
			ls[i].DisplayDevice = "GPU-2a44210f"
		}
	}
	if d := Decide(req, ls, f.live()); d.Defer {
		t.Fatalf("UUID pin + declared footprint + 15 free must admit, got %+v", d)
	}
	// Locked admits.
	locked := admitting()
	locked.pres = &Presence{Mode: "auto", Known: true, Away: true, Locked: true, IdleSec: 3, Note: "console session locked"}
	if d := Decide(req, layers(), locked.live()); d.Defer || !strings.Contains(d.Reason, "locked") {
		t.Fatalf("a locked console admits regardless of idle time, got %+v", d)
	}
	// Away override admits.
	away := admitting()
	away.pres = &Presence{Mode: "away", Known: true, Away: true, Note: "operator override: away"}
	if d := Decide(req, layers(), away.live()); d.Defer || !strings.Contains(d.Reason, "operator override") {
		t.Fatalf("mode away admits with the override named, got %+v", d)
	}
}

func TestPresenceOverridesReplaceTheProbe(t *testing.T) {
	p := ProbePresence("present", 0)
	if p.Mode != "present" || !p.Known || p.Away || !strings.Contains(p.Note, "override") {
		t.Fatalf("present override: %+v", p)
	}
	p = ProbePresence("away", 0)
	if p.Mode != "away" || !p.Known || !p.Away || !strings.Contains(p.Note, "override") {
		t.Fatalf("away override: %+v", p)
	}
	p = ProbePresence("", 0)
	if p.Mode != "present" || p.Away {
		t.Fatalf("empty mode is present (fail closed): %+v", p)
	}
	p = ProbePresence("bogus", 0)
	if p.Known || p.Away {
		t.Fatalf("an unknown mode is unknown, never away: %+v", p)
	}
	p = ProbePresence("auto", 0)
	if p.Mode != "auto" {
		t.Fatalf("auto runs the probe and keeps its mode: %+v", p)
	}
}

func TestUnreadableReadersFailClosedForGuardsAndNeutralForOccupancy(t *testing.T) {
	none := Live{}
	long := Request{Class: ClassAgent, EstTokens: 10_000, MaxTokens: 1024, BudgetSec: 300, ContextClass: core.ContextClassLong}
	d := Decide(long, layers(), none)
	if !d.Defer || d.DeferClass != core.DeferClassCapacity || d.Guard != "display_floor" {
		t.Fatalf("no readers: the first declared guard refuses, got %+v", d)
	}
	if !strings.Contains(d.Reason, "unreadable") {
		t.Fatalf("reason must say the reader is missing: %q", d.Reason)
	}
	// A reader that is present but cannot see the card refuses too.
	blind := admitting()
	delete(blind.free, "1")
	if d := Decide(long, layers(), blind.live()); !d.Defer || d.Guard != "display_floor" || !strings.Contains(d.Reason, "unreadable") {
		t.Fatalf("display card absent from the probe → refuse, got %+v", d)
	}
	// Occupancy unknown is neutral: the pair still takes the contract, no note.
	d = Decide(agentReq(50_000), layers(), none)
	if d.Defer || d.Wait || d.Layer != "pair" || d.Role != "agent" || strings.Contains(d.Reason, "in flight") {
		t.Fatalf("unknown occupancy is neutral for the pair's agent seat, got %+v", d)
	}
	d = Decide(agentReq(200_000), layers(), none)
	if d.Defer || d.Wait || d.Layer != "pair" || d.Role != "long" || d.Evicts != "" {
		t.Fatalf("unknown occupancy never causes a wait, got %+v", d)
	}
	if !strings.Contains(d.Reason, "occupancy unknown") {
		t.Fatalf("the reason says the occupancy was unreadable: %q", d.Reason)
	}
	d = Decide(Request{Class: ClassMechanical, RouteKey: "workhorse"}, layers(), none)
	if d.Layer != "single" || strings.Contains(d.Reason, "time-shares") {
		t.Fatalf("unknown occupancy: no time-share note, got %+v", d.Placed)
	}
}

func TestTooBigForEveryWindowDefersAsAContractProblem(t *testing.T) {
	d := Decide(agentReq(300_000), layers(), admitting().live())
	if !d.Defer || d.DeferClass != core.DeferClassContract || d.Guard != "" {
		t.Fatalf("too big for every window → contract defer, got %+v", d)
	}
	if !strings.Contains(d.Reason, "contract needs ~301024 tokens; no layer window holds it (largest 262144)") {
		t.Fatalf("reason: %q", d.Reason)
	}
}

func TestPlacedDevicesAreTheSeatPinNotTheLayerList(t *testing.T) {
	d := Decide(agentReq(1000), layers(), admitting().live())
	if !reflect.DeepEqual(d.Devices, []string{"0", "2"}) {
		t.Fatalf("pair/agent devices = the seat pin split, got %v", d.Devices)
	}
	long := Request{Class: ClassAgent, EstTokens: 1000, MaxTokens: 100, BudgetSec: 300, ContextClass: core.ContextClassLong}
	if d := Decide(long, layers(), admitting().live()); !reflect.DeepEqual(d.Devices, []string{"0", "1", "2"}) {
		t.Fatalf("triple/long devices = the seat pin, got %v", d.Devices)
	}
	// A layer that lists two device SETS still reports the seat's own pin.
	ls := layers()
	ls[1].Devices = []string{"0,2", "0,1"}
	if d := Decide(agentReq(1000), ls, admitting().live()); !reflect.DeepEqual(d.Devices, []string{"0", "2"}) {
		t.Fatalf("the layer's alternatives list never leaks into placed.devices, got %v", d.Devices)
	}
}

func TestDecideOnLayerRefusesADormantLayerByName(t *testing.T) {
	req := Request{Class: ClassMechanical, RouteKey: "workhorse"}
	d := DecideOnLayer(req, layers(), "display", admitting().live())
	if !d.Defer || d.DeferClass != core.DeferClassCapacity || d.Layer != "display" {
		t.Fatalf("a dormant layer named explicitly is a capacity defer, got %+v", d)
	}
	if d.Reason != "layer display is dormant (operator decision)" {
		t.Fatalf("reason: %q", d.Reason)
	}
	// An undeclared layer is a contract problem.
	if d := DecideOnLayer(req, layers(), "quad", admitting().live()); !d.Defer || d.DeferClass != core.DeferClassContract || !strings.Contains(d.Reason, "quad") {
		t.Fatalf("undeclared layer → contract defer naming it, got %+v", d)
	}
	// Restricting to a layer keeps the table's rows for that layer.
	busy := admitting()
	busy.seats["pair/agent"] = SeatState{Known: true, Loaded: true, Inflight: 2}
	if d := DecideOnLayer(agentReq(200_000), layers(), "pair", busy.live()); d.Layer != "pair" || d.Role != "long" || !d.Wait {
		t.Fatalf("on the pair, overflow follows the pair-long wait rule, got %+v", d)
	}
	// 51,024 tokens at the triple's measured 69 t/s ≈ 739 s: feasible only inside a 900 s budget.
	fits := Request{Class: ClassAgent, EstTokens: 50_000, MaxTokens: 1024, BudgetSec: 900}
	if d := DecideOnLayer(fits, layers(), "triple", admitting().live()); d.Layer != "triple" || d.Role != "long" || d.Defer {
		t.Fatalf("restricted to the triple, a fitting contract runs on its long seat under the guards, got %+v", d)
	}
	if d := DecideOnLayer(agentReq(50_000), layers(), "single", admitting().live()); !d.Defer || d.DeferClass != core.DeferClassContract {
		t.Fatalf("the single layer serves no agent contract (R2: the overflow branch is not built), got %+v", d)
	}
	// Restricted to single with the pair loaded, the time-share note still reads the pair's occupancy.
	if d := DecideOnLayer(req, layers(), "single", busy.live()); d.Layer != "single" || d.Evicts != "agent-pool" {
		t.Fatalf("restriction never hides another layer's occupancy, got %+v", d.Placed)
	}
	// Restricted to the display layer while awake: the guards decide, no single fallback.
	awake := layers()
	for i := range awake {
		if awake[i].Name == "display" {
			awake[i].Dormant = false
		}
	}
	atDesk := admitting()
	atDesk.pres = &Presence{Mode: "present", Known: true}
	if d := DecideOnLayer(req, awake, "display", atDesk.live()); !d.Defer || d.Guard != "presence" || d.Layer != "display" {
		t.Fatalf("named display + guard refusal → capacity defer on the display layer, got %+v", d)
	}
	if d := DecideOnLayer(req, awake, "display", busy.live()); d.Defer || d.Layer != "display" || d.Seat != "gemma-4-e4b-display" {
		t.Fatalf("named display + guards pass → the twin, got %+v", d)
	}
}

func TestRowsRoundTripThroughFromRowsAndDecideAgrees(t *testing.T) {
	cfg := config.CompositeFixture()
	local := admitting()
	local.seats["pair/agent"] = SeatState{Known: true, Loaded: true, Inflight: 4}
	rows := RowsFromConfig(cfg, local.live())
	remoteLayers, remote := FromRows(rows)
	if !reflect.DeepEqual(remoteLayers, cfg.Layers) {
		t.Fatalf("FromRows must rebuild the declared layers exactly:\n got %+v\nwant %+v", remoteLayers, cfg.Layers)
	}
	reqs := map[string]Request{
		"fits":     agentReq(50_000),
		"overflow": agentReq(200_000),
		"long":     {Class: ClassAgent, EstTokens: 10_000, MaxTokens: 1024, BudgetSec: 300, ContextClass: core.ContextClassLong},
		"too big":  agentReq(300_000),
	}
	for name, req := range reqs {
		a := Decide(req, cfg.Layers, local.live())
		b := Decide(req, remoteLayers, remote)
		same := a.Layer == b.Layer && a.Role == b.Role && a.Seat == b.Seat && reflect.DeepEqual(a.Devices, b.Devices) &&
			a.Wait == b.Wait && a.Defer == b.Defer && a.DeferClass == b.DeferClass && a.Guard == b.Guard && a.Evicts == b.Evicts && a.CtxTokens == b.CtxTokens
		if !same {
			t.Fatalf("%s: Decide over rows must agree with Decide over config\nlocal  %+v\nremote %+v", name, a, b)
		}
	}
	// The remote decision for the triple stands on the node's own verdict.
	d := Decide(reqs["long"], remoteLayers, remote)
	if d.Defer || !strings.Contains(d.Reason, "node's own verdict") {
		t.Fatalf("remote triple admission cites the node's verdict, got %+v", d)
	}
	// Flip the node's verdict: the delegator refuses on it, no local reader consulted.
	for i := range rows {
		if rows[i].Name == "triple" {
			rows[i].Admissible, rows[i].Reason = false, "display_floor: free 3.0 GiB − footprint 10.5 < floor 4"
		}
	}
	remoteLayers, remote = FromRows(rows)
	d = Decide(reqs["long"], remoteLayers, remote)
	if !d.Defer || d.DeferClass != core.DeferClassCapacity || d.Guard != "display_floor" || !strings.Contains(d.Reason, "free 3.0 GiB") {
		t.Fatalf("a remote row's refusal is honoured with its reason, got %+v", d)
	}
}
