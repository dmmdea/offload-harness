package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// Fleet per-card lease truth, the delegator's half (plan P7, register C-86). A remote node
// publishes its live leases with the cards each sits on (health leases[]) and the cards of
// each of its seats (layers[].seats[].device_ids). The delegator fences the node only for a
// contract whose seats ALL sit on a card some fencing lease holds, because the node's
// placement table falls back to a seat whose cards are free. A node that publishes no
// leases[] is read exactly as before: its one lease block is the whole node.

const (
	remCardA = "gpu-aaaa0000"
	remCardB = "gpu-bbbb0000"
	remCardC = "gpu-cccc0000"
)

func remoteCards() []gpuprobe.Device {
	return []gpuprobe.Device{
		{Index: 0, UUID: "GPU-AAAA0000", Name: "A", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
		{Index: 1, UUID: "GPU-BBBB0000", Name: "B", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
		{Index: 2, UUID: "GPU-CCCC0000", Name: "C", TotalGiB: 16, FreeGiB: 15, UtilKnown: true},
	}
}

// flagshipRemote is a composite node on the flagship layout: the triple agent seat spans all
// three cards, the single layer's agent seat sits on card 0, and a "fast" layer adds a second
// card-0 agent seat a contract can name (the table refuses to name the single layer on a box
// that has a home layer). It publishes seat ids.
func flagshipRemote(t *testing.T) NodeView {
	t.Helper()
	live := placetable.Live{
		Seat:        func(string, string) placetable.SeatState { return placetable.SeatState{} },
		DeviceFree:  func(string) (float64, bool) { return 16, true },
		DeviceIndex: func(d string) (string, bool) { return d, true },
		HostFree:    func() (float64, bool) { return 80, true },
		Presence:    func() placetable.Presence { return placetable.Presence{Mode: "away", Known: true, Away: true} },
	}
	r := eligibleRemote()
	r.AgentSeat = "agent-pool"
	r.AgentCtxTokens = 262144
	r.Devices = remoteCards()
	cards, _ := gpuprobe.BuildCards(r.Devices, "")
	cfg := config.FlagshipFixture()
	cfg.Layers = append(cfg.Layers, config.LayerSpec{Name: "fast", Tier: "blackwell-16", Devices: []string{"0"}, Seats: []config.LayerSeat{
		{Role: placetable.RoleAgent, Model: "fast-agent", Device: "0", CtxTokens: 32768, FootprintGiB: 12},
	}})
	r.Layers = placetable.WithDeviceIDs(placetable.RowsFromConfig(cfg, live), cards)
	return r
}

func leaseOn(epoch uint64, class string, devices ...string) LeaseView {
	return LeaseView{Epoch: epoch, Class: class, Devices: devices, Busy: true, RemainingSec: 6 * 3600}
}

// asPublished sets the singular fields the way a node folds them for a reader one release
// behind (the worst across its live leases), so a test node looks like the real wire.
func asPublished(r NodeView, ls ...LeaseView) NodeView {
	r.Leases = ls
	for _, l := range ls {
		r.LeasedText = r.LeasedText || l.Class == "text"
		r.LeaseBusy = r.LeaseBusy || (l.Busy && !l.Overdue)
		r.LeaseOverdue = r.LeaseOverdue || l.Overdue
		r.LeaseExclusive = r.LeaseExclusive || l.Exclusive
		r.LeaseDraining = r.LeaseDraining || l.Draining
	}
	return r
}

// The headline: a long render on card 2 of a remote must not fence a contract that runs on
// the single layer's card-0 seat. The control arm is the same node read the old way.
func TestRemoteLeaseOnOtherCardDoesNotFenceNode(t *testing.T) {
	st := schemaSubtask()
	r := asPublished(flagshipRemote(t), leaseOn(7, "media", remCardC))
	if !remoteEligible(st, r) {
		fenced, why := leaseFenceReason(r, &st)
		t.Fatalf("a media lease on card 2 left the single layer's card-0 seat free; the node must stay a target (fenced=%v %q)", fenced, why)
	}

	// Control: the same node as a node one release behind publishes it (no leases[]): the
	// one lease block is the whole node, so today's fence applies.
	old := r
	old.Leases = nil
	if remoteEligible(st, old) {
		t.Fatal("control: without leases[] a busy non-text lease still fences the whole node, exactly as before")
	}
}

// A contract whose every seat sits on the leased card is fenced, and the reason names the
// lease and the cards.
func TestRemoteLeaseOnTheContractsCardsFences(t *testing.T) {
	st := schemaSubtask()
	st.Contract.Layer = "triple"
	r := asPublished(flagshipRemote(t), leaseOn(7, "media", remCardC))
	eligible, word, detail := eligibilityVerdict(st, r)
	if eligible || word != "lease" {
		t.Fatalf("a contract naming the three-card layer sits on the leased card 2: verdict (%v, %q, %q)", eligible, word, detail)
	}
	for _, want := range []string{"busy", "media", "7"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("the reason must name the lease (%q missing): %q", want, detail)
		}
	}
	if !strings.HasSuffix(detail, "lease 7 on 1 card") {
		t.Fatalf("the reason must say how many cards the lease holds: %q", detail)
	}

	// The same node, a contract naming the card-0 layer: not fenced.
	named := schemaSubtask()
	named.Contract.Layer = "fast"
	if !remoteEligible(named, r) {
		t.Fatal("a contract naming the card-0 layer is not held by a card-2 lease")
	}
}

// A chain is held only when EVERY seat is: a lease on card 0 holds the triple seat and the
// single seat alike.
func TestRemoteLeaseHoldingEverySeatOfTheChainFences(t *testing.T) {
	st := schemaSubtask()
	r := asPublished(flagshipRemote(t), leaseOn(3, "media", remCardA))
	if remoteEligible(st, r) {
		t.Fatal("a lease on card 0 holds both seats of the default chain (triple, then single): the node is fenced")
	}
}

// Exclusive and draining fence by the same rule: only the contracts they hold.
func TestRemoteExclusiveLeaseFencesOnlyTheContractsItHolds(t *testing.T) {
	excl := leaseOn(9, "text", remCardC)
	excl.Exclusive, excl.Busy = true, false
	r := asPublished(flagshipRemote(t), excl)
	if !remoteEligible(schemaSubtask(), r) {
		t.Fatal("an exclusive reservation on card 2 does not hold the card-0 seat")
	}
	triple := schemaSubtask()
	triple.Contract.Layer = "triple"
	eligible, word, detail := eligibilityVerdict(triple, r)
	if eligible || word != "lease" || !strings.Contains(detail, "exclusive") {
		t.Fatalf("an exclusive reservation fences the contract that needs card 2: (%v, %q, %q)", eligible, word, detail)
	}
	drain := leaseOn(9, "text", remCardA)
	drain.Draining, drain.Busy = true, false
	if remoteEligible(schemaSubtask(), asPublished(flagshipRemote(t), drain)) {
		t.Fatal("a draining reservation on card 0 holds every seat of the default chain")
	}
}

// A lease that names no cards is the whole node, whatever the node's layout.
func TestRemoteWholeNodeLeaseFencesEveryContract(t *testing.T) {
	r := asPublished(flagshipRemote(t), leaseOn(5, "media"))
	for _, layer := range []string{"", "fast", "triple"} {
		st := schemaSubtask()
		st.Contract.Layer = layer
		if remoteEligible(st, r) {
			t.Fatalf("a whole-node lease must fence a contract naming layer %q", layer)
		}
	}
}

// A node with no layers has no seat to narrow by, and a node that published no seat ids and
// no card table has seats nobody can place: both read every card.
func TestRemoteLeaseWithoutSeatCardsStaysWholeNode(t *testing.T) {
	plain := asPublished(eligibleRemote(), leaseOn(7, "media", remCardC))
	if remoteEligible(schemaSubtask(), plain) {
		t.Fatal("a plain node's seat has no declared cards: a busy lease on any card fences it")
	}
	blind := flagshipRemote(t)
	blind.Devices = nil
	for i := range blind.Layers {
		for j := range blind.Layers[i].Seats {
			blind.Layers[i].Seats[j].DeviceIDs = nil
		}
	}
	blind = asPublished(blind, leaseOn(7, "media", remCardC))
	if remoteEligible(schemaSubtask(), blind) {
		t.Fatal("a node that published neither seat ids nor cards cannot show a seat is free of the lease: fenced")
	}
}

// An overdue lease on the contract's cards ranks the node last and never excludes it; one on
// other cards is no reason to rank it down at all.
func TestOverdueLeaseOnTheContractsCardsRanksLastNotExcluded(t *testing.T) {
	st := schemaSubtask()
	st.Contract.Layer = "triple"

	over := leaseOn(4, "media", remCardC)
	over.Busy, over.Overdue = true, true // the wire says both: an overdue lease also publishes busy
	held := asPublished(flagshipRemote(t), over)
	held.NodeID = "node-held"
	free := flagshipRemote(t)
	free.NodeID = "node-free"

	if !remoteEligible(st, held) {
		t.Fatal("an overdue lease is ranked, never excluded: the abandoned lease must not make the node unroutable")
	}
	if got := Place("seed", st, localNode(), []NodeView{held, free}, true); got.NodeID != "node-free" {
		t.Fatalf("placed on %q, want the node without the overdue lease on the contract's cards", got.NodeID)
	}
	if got := Place("seed", st, localNode(), []NodeView{held}, true); got.NodeID != "node-held" {
		t.Fatalf("with nothing better the overdue node still takes the work, got %q", got.NodeID)
	}

	// The same overdue lease, on a contract that runs on card 0: no demotion, roster order stands.
	named := schemaSubtask()
	named.Contract.Layer = "fast"
	if got := Place("seed", named, localNode(), []NodeView{held, free}, true); got.NodeID != "node-held" {
		t.Fatalf("an overdue lease on card 2 says nothing about a card-0 contract: roster order must stand, got %q", got.NodeID)
	}
}

// Acceptance: two nodes, one with a long render on card 2. A contract that runs on card 0
// lands on the node holding the render, because its card is free; one that needs card 2
// lands on the other node.
func TestPlaceSendsWorkToTheNodeWhoseCardsAreFree(t *testing.T) {
	busy := asPublished(flagshipRemote(t), leaseOn(7, "media", remCardC))
	busy.NodeID = "node-render"
	idle := flagshipRemote(t)
	idle.NodeID = "node-idle"

	card0 := schemaSubtask()
	card0.Contract.Layer = "fast"
	if got := Place("seed", card0, localNode(), []NodeView{busy, idle}, true); got.NodeID != "node-render" {
		t.Fatalf("a card-0 contract must still use the render node's free card (roster order), got %q", got.NodeID)
	}
	card2 := schemaSubtask()
	card2.Contract.Layer = "triple"
	if got := Place("seed", card2, localNode(), []NodeView{busy, idle}, true); got.NodeID != "node-idle" {
		t.Fatalf("a contract that needs card 2 must go to the node whose card 2 is free, got %q", got.NodeID)
	}
}

// The decode: leases[] maps onto NodeView.Leases; a node that publishes none leaves it nil,
// which is how an older node (and a node with no lease) reads.
func TestNodeViewDecodesLeases(t *testing.T) {
	body := `{"node_id":"n","agent_enabled":true,"agent_seat":"s","agent_seat_resident":true,"agent_ctx_tokens":8192,` +
		`"lease":{"held":true,"class":"text","pid":1,"until":"2026-09-06T17:00:00Z","busy":true,"remaining_sec":3600},` +
		`"lease_exclusive":true,` +
		`"leases":[{"epoch":7,"class":"MEDIA","devices":["GPU-CCCC0000"],"scope":"declared","until":"2026-09-06T17:00:00Z","remaining_sec":3600,"busy":true,"verdict":"held"},` +
		`{"epoch":9,"class":"text","scope":"whole-node","exclusive":true,"stalled":true,"verdict":"held-stalled"}]}`
	srv := healthServer(t, body, nil)
	got, err := FetchNodeView(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Leases) != 2 {
		t.Fatalf("Leases = %+v, want both", got.Leases)
	}
	a, b := got.Leases[0], got.Leases[1]
	if a.Epoch != 7 || a.Class != "media" || len(a.Devices) != 1 || a.Devices[0] != remCardC || !a.Busy || a.RemainingSec != 3600 || a.Until.IsZero() || a.Scope != "declared" {
		t.Fatalf("lease 0 = %+v", a)
	}
	if b.Epoch != 9 || len(b.Devices) != 0 || !b.Exclusive || !b.Stalled || b.Verdict != "held-stalled" {
		t.Fatalf("lease 1 = %+v, want a whole-node exclusive stalled lease", b)
	}
	if !got.LeaseExclusive || !got.LeasedText {
		t.Fatalf("the singular fields still decode as before: exclusive=%v text=%v", got.LeaseExclusive, got.LeasedText)
	}

	// An older node: the singular block only.
	old := healthServer(t, `{"node_id":"n","lease":{"held":true,"class":"media","pid":1,"busy":true,"remaining_sec":3600}}`, nil)
	ov, err := FetchNodeView(context.Background(), old.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if ov.Leases != nil || !ov.LeaseBusy {
		t.Fatalf("an older node decodes with no Leases and its singular fields intact: %+v", ov)
	}
}

// A plain text reservation that reads busy never fences (a reservation steers placement, W-14): it
// demotes the node for the contracts it holds, and the node still takes the work when it is the only
// one. The control for the busy rung of the lease key.
func TestPlainBusyTextReservationOnTheContractsCardsDemotesNeverFences(t *testing.T) {
	st := schemaSubtask()
	text := leaseOn(6, "text", remCardA) // card 0 holds every seat of the default chain
	held := asPublished(flagshipRemote(t), text)
	held.NodeID = "node-held"
	free := flagshipRemote(t)
	free.NodeID = "node-free"
	if !remoteEligible(st, held) {
		fenced, why := leaseFenceReason(held, &st)
		t.Fatalf("a plain busy text reservation must not fence (fenced=%v %q)", fenced, why)
	}
	if got := Place("seed", st, localNode(), []NodeView{held, free}, true); got.NodeID != "node-free" {
		t.Fatalf("placed on %q, want the node without the reservation on the contract's cards", got.NodeID)
	}
	if got := Place("seed", st, localNode(), []NodeView{held}, true); got.NodeID != "node-held" {
		t.Fatalf("with nothing better the reserved node still takes the work, got %q", got.NodeID)
	}
	// And on a card the contract does not use it demotes nothing.
	other := asPublished(flagshipRemote(t), leaseOn(6, "text", remCardC))
	other.NodeID = "node-other"
	named := schemaSubtask()
	named.Contract.Layer = "fast"
	if got := Place("seed", named, localNode(), []NodeView{other, free}, true); got.NodeID != "node-other" {
		t.Fatalf("a reservation on card 2 says nothing about a card-0 contract: roster order must stand, got %q", got.NodeID)
	}
}

// The reason names the strongest hold, with the precedence the one-block rule has always had:
// exclusive, then draining, then busy; and it names the lease that holds the FIRST seat of the
// chain, the one a waiter would be waiting for.
func TestRemoteFenceReasonNamesTheStrongestHold(t *testing.T) {
	st := schemaSubtask()
	busy := leaseOn(3, "media", remCardA)
	drain := leaseOn(5, "text", remCardA)
	drain.Draining, drain.Busy = true, false
	excl := leaseOn(9, "text", remCardA)
	excl.Exclusive, excl.Busy = true, false

	_, why := leaseFenceReason(asPublished(flagshipRemote(t), busy), &st)
	if !strings.HasPrefix(why, "busy:") || !strings.Contains(why, "lease 3") {
		t.Fatalf("only a busy media lease: %q", why)
	}
	_, why = leaseFenceReason(asPublished(flagshipRemote(t), busy, drain), &st)
	if !strings.HasPrefix(why, "draining:") || !strings.Contains(why, "lease 5") {
		t.Fatalf("a draining reservation outranks a busy render: %q", why)
	}
	_, why = leaseFenceReason(asPublished(flagshipRemote(t), busy, drain, excl), &st)
	if !strings.HasPrefix(why, "exclusive:") || !strings.Contains(why, "lease 9") {
		t.Fatalf("an exclusive reservation outranks both: %q", why)
	}

	// The first seat of the chain is the three-card seat: an exclusive lease on card 1 holds only
	// that seat, while a render on card 0 holds every seat. The reason names what holds the
	// first seat, the exclusive one, not what holds the last.
	onB := leaseOn(8, "text", remCardB)
	onB.Exclusive, onB.Busy = true, false
	_, why = leaseFenceReason(asPublished(flagshipRemote(t), busy, onB), &st)
	if !strings.HasPrefix(why, "exclusive:") || !strings.Contains(why, "lease 8") {
		t.Fatalf("the first seat of the chain is held by the exclusive lease on card 1: %q", why)
	}
}

// A contract-less lane (vision, text) ranks a node by every lease it publishes, as the whole node.
func TestLeaseRankWithNoContractReadsEveryLease(t *testing.T) {
	long := asPublished(flagshipRemote(t), leaseOn(7, "media", remCardC))
	if got := leaseDemotionRank(long, nil); got != 1 {
		t.Fatalf("rank with no contract = %d, want 1 (a long lease anywhere demotes a lane that names no cards)", got)
	}
	over := leaseOn(7, "media", remCardC)
	over.Busy, over.Overdue = true, true
	if got := leaseDemotionRank(asPublished(flagshipRemote(t), over), nil); got != 2 {
		t.Fatalf("rank with no contract = %d, want 2 for an overdue lease", got)
	}
	if got := leaseDemotionRank(flagshipRemote(t), nil); got != 0 {
		t.Fatalf("rank with no lease = %d, want 0", got)
	}
}

// Card ids compare case-insensitively.
func TestSharesCardIsCaseInsensitive(t *testing.T) {
	if !sharesCard([]string{"GPU-AAAA0000"}, []string{remCardA, remCardB}) {
		t.Fatal("ids differing only in case name the same card")
	}
	if sharesCard([]string{remCardC}, []string{remCardA, remCardB}) {
		t.Fatal("different cards share nothing")
	}
}

func TestLeaseCardsPhraseCountsTheCardsALeaseHolds(t *testing.T) {
	for devices, want := range map[string]string{"": "the whole node", "a": "1 card", "a,b": "2 cards", "a,b,c": "3 cards"} {
		var l LeaseView
		if devices != "" {
			l.Devices = strings.Split(devices, ",")
		}
		if got := leaseCardsPhrase(l); got != want {
			t.Errorf("leaseCardsPhrase(%q) = %q, want %q", devices, got, want)
		}
	}
}

// What a lease with several ends reports as the place in line: the earliest end among the leases
// that are not overdue, and nothing for a lease whose window has passed (its stale remaining time
// says nothing about when it frees).
func TestRemoteLeasePlaceTakesTheEarliestEndOfALiveLease(t *testing.T) {
	st := schemaSubtask()
	stale := leaseOn(3, "media", remCardA)
	stale.Overdue, stale.RemainingSec = true, 100
	soon := leaseOn(5, "media", remCardA)
	soon.RemainingSec = 5000
	later := leaseOn(6, "media", remCardA)
	later.RemainingSec = 9000
	v := asPublished(flagshipRemote(t), stale, later, soon)
	p := remoteLeasePlace(v, &st, "busy: media lease 5 on 1 card")
	if p.On != "lease" || p.EtaSec != 5000 {
		t.Fatalf("place = %+v, want the earliest live end (5000 s), not the overdue lease's stale 100 nor the latest 9000", p)
	}
	if p := remoteLeasePlace(asPublished(flagshipRemote(t), stale), &st, "x"); p.EtaSec != 0 {
		t.Fatalf("an overdue lease says nothing about when it frees: %+v", p)
	}
}
