package placement

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// The agent lane when a lease holds cards (plan P4, register C-86): the flagship layer is
// the one whose cards a card-2 render intersects, so the table takes the next declared
// layer whose cards are free (the single layer's card-0 seat) instead of queueing the
// contract behind a render it does not need to wait for.

// heldCards is a machine on which the leases hold the cards in held (PCI-order indices):
// a seat is on a held card when any of its pins is one of them.
func heldCards(held ...string) func([]string) (bool, string) {
	set := map[string]bool{}
	for _, h := range held {
		set[h] = true
	}
	return func(pins []string) (bool, string) {
		for _, p := range pins {
			if set[p] {
				return true, "card " + p + " is held by a media lease"
			}
		}
		return false, ""
	}
}

func flagshipLive(held ...string) Live {
	l := admitting().live()
	if len(held) > 0 {
		l.CardsHeld = heldCards(held...)
	}
	return l
}

func TestAgentHomeFallsBackToNonIntersectingLayer(t *testing.T) {
	d := Decide(agentReq(1000), flagshipLayers(), flagshipLive("2"))
	if d.Defer || d.Wait {
		t.Fatalf("a free card serves the contract: it must neither defer nor wait, got %+v", d)
	}
	if d.Layer != "single" || d.Role != "agent" || d.Seat != "gemma-4-26b-agent" {
		t.Fatalf("a card-2 lease must move the agent lane to the single layer's card-0 seat, got %+v", d.Placed)
	}
	if !reflect.DeepEqual(d.Devices, []string{"0"}) {
		t.Fatalf("devices = the fallback seat's pin, got %v", d.Devices)
	}
	for _, want := range []string{"triple", "held", "fallback", "single"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("the reason must name %q: %s", want, d.Reason)
		}
	}
}

func TestPlacementKeepsLocalSeatOnFreeCard(t *testing.T) {
	d := Decide(agentReq(1000), flagshipLayers(), flagshipLive())
	if d.Layer != "triple" || d.Seat != "agent-pool" {
		t.Fatalf("no lease: the flagship takes the contract, got %+v", d.Placed)
	}
	// A lease on a card the flagship does not use leaves it alone: with the pair as the home
	// layer a card-1 lease is the one nothing declared a seat on.
	d = Decide(agentReq(1000), config.CompositeFixture().Layers, func() Live { l := admitting().live(); l.CardsHeld = heldCards("1"); return l }())
	if d.Layer != "pair" || strings.Contains(d.Reason, "fallback") {
		t.Fatalf("a lease on a card the home seat does not use must not move the contract, got %+v", d.Placed)
	}
}

// With no reader of held cards the table is byte-identical to before: the delegate that
// builds a Live from a remote's health rows has none.
func TestNilCardsHeldReaderChangesNothing(t *testing.T) {
	l := admitting().live()
	if l.CardsHeld != nil {
		t.Fatal("fixture: the plain Live carries no CardsHeld reader")
	}
	d := Decide(agentReq(1000), flagshipLayers(), l)
	if d.Layer != "triple" || strings.Contains(d.Reason, "held") {
		t.Fatalf("no reader, no fallback: %+v", d.Placed)
	}
}

func TestNoFallbackQueuesAndRoutesRemote(t *testing.T) {
	// Cards 0 and 2 are both held: the flagship (2,1,0) and the single layer's card-0 seat
	// are both on a held card. Nothing is free, so the contract keeps the home seat and
	// QUEUES at its gate: never a defer, and the reason says why the delegator may route it
	// to another node.
	d := Decide(agentReq(1000), flagshipLayers(), flagshipLive("0", "2"))
	if d.Defer {
		t.Fatalf("a busy card is a place in line, never a refusal: %+v", d)
	}
	if d.Layer != "triple" || d.Seat != "agent-pool" {
		t.Fatalf("with every local seat held the home seat keeps the contract, got %+v", d.Placed)
	}
	for _, want := range []string{"held", "no local layer", "queue"} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("the reason must say %q: %s", want, d.Reason)
		}
	}
}

func TestFallbackNeverTakesAnOptInOrDormantLayer(t *testing.T) {
	// Only the pair (opt-in) and display (dormant, no agent seat) are left once the single
	// layer is gone: neither is a fallback, even though the pair's cards (0 and 2) are FREE
	// while the lease holds the display card (1) that the flagship spans.
	ls := flagshipLayers()
	var kept []config.LayerSpec
	for _, l := range ls {
		if l.Name != "single" {
			kept = append(kept, l)
		}
	}
	d := Decide(agentReq(1000), kept, flagshipLive("1"))
	if d.Layer != "triple" {
		t.Fatalf("an opt-in layer is never entered by fallback, got %+v", d.Placed)
	}
	if strings.Contains(d.Reason, "fallback") || !strings.Contains(d.Reason, "no local layer") {
		t.Fatalf("no fallback was possible: %s", d.Reason)
	}
}

func TestFallbackNeedsTheWindowToHoldTheContract(t *testing.T) {
	// The single seat's window is 131072: a 200k-token contract fits the flagship (262144)
	// and does not fit the fallback, so it keeps the home seat and queues.
	d := Decide(agentReq(200000), flagshipLayers(), flagshipLive("2"))
	if d.Layer != "triple" {
		t.Fatalf("a contract the fallback window cannot hold must not be moved onto it, got %+v", d.Placed)
	}
}

func TestFallbackNeverMovesAContractThatNamesALayer(t *testing.T) {
	d := DecideOnLayer(agentReq(1000), flagshipLayers(), "triple", flagshipLive("2"))
	if d.Layer != "triple" || d.Seat != "agent-pool" {
		t.Fatalf("a contract that names a layer runs there or waits there, got %+v", d.Placed)
	}
}

func TestAgentChainOrderAndWindow(t *testing.T) {
	ls := flagshipLayers()
	chain := AgentChain(ls, "", 0)
	var got []string
	for _, c := range chain {
		got = append(got, c.Layer.Name+"/"+c.Seat.Model)
	}
	want := []string{"triple/agent-pool", "single/gemma-4-26b-agent"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chain = %v, want the flagship then the single layer's agent seat %v", got, want)
	}
	if chain := AgentChain(ls, "", 200000); len(chain) != 1 || chain[0].Layer.Name != "triple" {
		t.Fatalf("a 200k-token contract fits only the flagship, got %+v", chain)
	}
	if chain := AgentChain(ls, "single", 0); len(chain) != 1 || chain[0].Layer.Name != "single" {
		t.Fatalf("a named layer is a chain of one, got %+v", chain)
	}
	if chain := AgentChain(ls, "nowhere", 0); len(chain) != 0 {
		t.Fatalf("a layer the box does not declare has no chain, got %+v", chain)
	}
	if chain := AgentChain(nil, "", 0); len(chain) != 0 {
		t.Fatalf("a plain box has no chain, got %+v", chain)
	}
	if got := RequestForContract(core.AgentContract{Goal: "x"}, 100, 50).Need(); got != 150 {
		t.Fatalf("Need() = %d, want estimate + completion budget", got)
	}
}
