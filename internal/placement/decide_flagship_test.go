package placement

import (
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
)

// flagshipLayers is the Qube as the operator ordered it on 2026-09-19: "the 3 card tier as the agent
// seat now and the 2 card tier to be the opt in one". The triple layer carries the agent seat and is
// NOT opt-in; the pair keeps its seats but is entered by name only.
func flagshipLayers() []config.LayerSpec {
	ls := config.CompositeFixture().Layers
	out := make([]config.LayerSpec, 0, len(ls))
	for _, l := range ls {
		l.Seats = append([]config.LayerSeat(nil), l.Seats...)
		switch l.Name {
		case LayerPair:
			l.OptIn = true
			l.Seats[0].Model = "agent-pool-2card"
		case LayerTriple:
			l.OptIn = false
			l.DisplayDevice, l.DisplayFloorGiB, l.Guards = "", 0, nil
			l.Seats = []config.LayerSeat{{Role: RoleAgent, Model: "agent-pool", Device: "0,1,2", CtxTokens: 262144, MaxInflight: 32, FootprintGiB: 13.5}}
		}
		out = append(out, l)
	}
	return out
}

func TestFreeChoiceLandsOnTheFlagshipWhenTheTripleCarriesTheAgentSeat(t *testing.T) {
	d := Decide(agentReq(1000), flagshipLayers(), admitting().live())
	if d.Defer || d.Wait {
		t.Fatalf("the flagship must place the free choice, got defer=%v reason=%q", d.Defer, d.Reason)
	}
	if d.Layer != LayerTriple || d.Seat != "agent-pool" || d.CtxTokens != 262144 {
		t.Fatalf("placed = %+v, want the triple layer's agent seat", d.Placed)
	}
	if !strings.Contains(d.Reason, "the triple's agent seat") {
		t.Fatalf("reason must name the flagship layer: %q", d.Reason)
	}
}

func TestTheOptInPairIsReachableByNameBesideTheFlagship(t *testing.T) {
	d := DecideOnLayer(agentReq(1000), flagshipLayers(), LayerPair, admitting().live())
	if d.Defer || d.Layer != LayerPair || d.Seat != "agent-pool-2card" {
		t.Fatalf("a contract naming the pair must run on the pair's agent seat, got %+v defer=%v", d.Placed, d.Defer)
	}
	for i := 0; i < 3; i++ {
		if d := Decide(agentReq(1000), flagshipLayers(), admitting().live()); d.Layer == LayerPair {
			t.Fatalf("the free choice drifted onto the opt-in pair: %+v", d.Placed)
		}
	}
}

// TestAnOptInTripleNeverTakesTheFreeChoice: a box that still declares the triple opt-in (the
// fixture) keeps the pair as its default — the flagship is a declaration, never a guess.
func TestAnOptInTripleNeverTakesTheFreeChoice(t *testing.T) {
	ls := flagshipLayers()
	for i := range ls {
		if ls[i].Name == LayerTriple {
			ls[i].OptIn = true
		}
		if ls[i].Name == LayerPair {
			ls[i].OptIn = false
		}
	}
	if d := Decide(agentReq(1000), ls, admitting().live()); d.Layer != LayerPair {
		t.Fatalf("an opt-in triple must leave the pair as the default, got %+v", d.Placed)
	}
}

func TestMechanicalTimeShareNamesTheFlagshipWhenItHoldsTheCards(t *testing.T) {
	req := Request{Class: ClassMechanical, EstTokens: 500, RouteKey: "workhorse"}
	busy := admitting()
	busy.seats["triple/agent"] = SeatState{Known: true, Loaded: true, Inflight: 2}
	d := Decide(req, flagshipLayers(), busy.live())
	if d.Layer != LayerSingle || d.Evicts != "agent-pool" || !strings.Contains(d.Reason, "time-shares the triple's cards") {
		t.Fatalf("the flagship loaded: the rung must name the time-share with it, got %+v", d.Placed)
	}
}
