package delegate

// GPU routing P1: the delegator reads a node's per-card truth (health
// gpu_devices[]) instead of one utilisation scalar, ranks nodes with a free
// card first, and ranks a node whose lease is overdue last without excluding it.
// Fixtures are synthetic: three-card boxes with made-up card ids.

import (
	"context"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// card is one synthetic nvidia-smi device. 16 GiB total, util known.
func card(idx int, util int, freeGiB float64, display bool) gpuprobe.Device {
	uuids := []string{"GPU-1111aaaa-2222-3333-4444-555566667777", "GPU-2222bbbb-3333-4444-5555-666677778888", "GPU-3333cccc-4444-5555-6666-777788889999", "GPU-4444dddd-5555-6666-7777-888899990000"}
	return gpuprobe.Device{Index: idx, UUID: uuids[idx], Name: "synthetic 16 GB", TotalGiB: 16, FreeGiB: freeGiB, UtilPct: util, UtilKnown: true, DisplayActive: display}
}

// threeCards builds a 3-card box from per-card utilisations; every card has
// 15 GiB free unless the test edits it.
func threeCards(u0, u1, u2 int) []gpuprobe.Device {
	return []gpuprobe.Device{card(0, u0, 15, false), card(1, u1, 15, false), card(2, u2, 15, false)}
}

// ---------------------------------------------------------------------------
// decode
// ---------------------------------------------------------------------------

func TestNodeViewDecodesGpuDevices(t *testing.T) {
	body := `{"node_id":"n","agent_enabled":true,"gpu_devices":[` +
		`{"index":0,"uuid":"GPU-1111aaaa-2222-3333-4444-555566667777","name":"synthetic 16 GB","vram_total_gb":16,"vram_free_gb":14.5,"util_pct":3,"util_known":true,"display_active":true},` +
		`{"index":1,"uuid":"GPU-2222bbbb-3333-4444-5555-666677778888","name":"synthetic 16 GB","vram_total_gb":16,"vram_free_gb":2,"util_pct":97,"util_known":true}]}`
	v, err := FetchNodeView(context.Background(), healthServer(t, body, nil).URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Devices) != 2 {
		t.Fatalf("Devices = %+v, want both cards", v.Devices)
	}
	d0, d1 := v.Devices[0], v.Devices[1]
	if d0.UUID != "GPU-1111aaaa-2222-3333-4444-555566667777" || d0.FreeGiB != 14.5 || d0.TotalGiB != 16 || d0.UtilPct != 3 || !d0.UtilKnown || !d0.DisplayActive {
		t.Fatalf("card 0 decoded as %+v", d0)
	}
	if d1.Index != 1 || d1.FreeGiB != 2 || d1.UtilPct != 97 || d1.DisplayActive {
		t.Fatalf("card 1 decoded as %+v", d1)
	}
}

// A node one release behind (or a single-device source) publishes no
// gpu_devices key. That is UNKNOWN, never "no free card".
func TestNodeViewWithoutGpuDevicesIsUnknownNotFull(t *testing.T) {
	v, err := FetchNodeView(context.Background(), healthServer(t, `{"node_id":"old","agent_enabled":true}`, nil).URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Devices != nil {
		t.Fatalf("Devices = %+v, want nil for an older node", v.Devices)
	}
	if free, total, known := v.FreeCards(); known || free != 0 || total != 0 {
		t.Fatalf("FreeCards() = %d,%d,%v: a node that publishes no devices must read unknown", free, total, known)
	}
}

// Overdue travels as its own fact. It must NOT decode as LeaseBusy: a busy
// non-text lease is a hard exclusion in the gate, and an abandoned lease may
// rank last but may not make the node unroutable for longer than it was before.
func TestFetchNodeViewDecodesOverdueSeparatelyFromBusy(t *testing.T) {
	view := func(body string) NodeView {
		t.Helper()
		v, err := FetchNodeView(context.Background(), healthServer(t, body, nil).URL, "")
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	over := view(`{"node_id":"n","agent_enabled":true,"lease":{"held":true,"class":"media","busy":true,"overdue":true}}`)
	if !over.LeaseOverdue {
		t.Fatal("an overdue held lease must decode to LeaseOverdue")
	}
	if over.LeaseBusy {
		t.Fatal("an overdue-only lease decoded as LeaseBusy: the gate would hard-exclude the node, the opposite of ranking it last")
	}
	// A genuinely long lease is busy and not overdue, as before.
	long := view(`{"node_id":"n","agent_enabled":true,"lease":{"held":true,"class":"media","busy":true,"remaining_sec":21600}}`)
	if !long.LeaseBusy || long.LeaseOverdue {
		t.Fatalf("long lease = busy %v overdue %v, want busy and not overdue", long.LeaseBusy, long.LeaseOverdue)
	}
	// A node one release behind publishes neither.
	if old := view(`{"node_id":"n","agent_enabled":true,"lease":{"held":true,"class":"media"}}`); old.LeaseOverdue || old.LeaseBusy {
		t.Fatal("an older node's lease block decoded as busy or overdue")
	}
	// overdue without a held lease is nothing.
	if no := view(`{"node_id":"n","agent_enabled":true,"lease":{"held":false,"overdue":true}}`); no.LeaseOverdue {
		t.Fatal("an unheld lease cannot be overdue")
	}
}

// ---------------------------------------------------------------------------
// overdue: ranked last, never excluded
// ---------------------------------------------------------------------------

func TestDelegatorRanksOverdueNodeLastNotExcluded(t *testing.T) {
	st := schemaSubtask()
	overdue := eligibleRemote()
	overdue.NodeID, overdue.LeaseOverdue = "node-overdue", true
	overdue.QueueDepth = 0 // the best node on every other key

	worse := eligibleRemote()
	worse.NodeID = "node-worse"
	worse.QueueDepth = 5

	// Not excluded: the overdue node is still a placement target.
	if !remoteEligible(st, overdue) {
		t.Fatal("an overdue lease must not exclude the node: an abandoned lease may not make it unroutable")
	}
	// Ranked last: it loses to a node that is worse on every other key.
	if betterRemote("seed", &st, 0, overdue, worse) {
		t.Fatal("an overdue node beat a clean node on its other keys; overdue means ranked last")
	}
	if !betterRemote("seed", &st, 0, worse, overdue) {
		t.Fatal("a clean node must beat an overdue one")
	}
	// A saturated clean node still beats it: last means last.
	saturatedNode := eligibleRemote()
	saturatedNode.NodeID, saturatedNode.SaturationKnown, saturatedNode.SaturationHigh = "node-sat", true, true
	if betterRemote("seed", &st, 0, overdue, saturatedNode) {
		t.Fatal("an overdue node must rank below even a saturated clean node")
	}
	// Place: chosen only when it is the sole eligible remote, never refused.
	got := Place("seed", st, localNode(), []NodeView{overdue, worse}, true)
	if got.NodeID != "node-worse" {
		t.Fatalf("Place chose %q, want the clean node", got.NodeID)
	}
	got = Place("seed", st, localNode(), []NodeView{overdue}, true)
	if got.NodeID != "node-overdue" {
		t.Fatalf("Place chose %q: with no other remote the overdue node must still take the work (queued on the node, never refused)", got.NodeID)
	}
	// Exclusive and draining still fence, overdue or not.
	exclusive := overdue
	exclusive.LeaseExclusive = true
	if remoteEligible(st, exclusive) {
		t.Fatal("an exclusive lease fences the cards whatever its age")
	}
	// Order among overdue and busy: overdue is the lower rank.
	busy := eligibleRemote()
	busy.NodeID, busy.LeasedText, busy.LeaseBusy = "node-busy", true, true
	if betterRemote("seed", &st, 0, overdue, busy) {
		t.Fatal("an overdue node must rank below a node under a genuine long lease: the declared end of an overdue lease is no evidence of when it frees")
	}
}

// The vision and text lanes exclude on LeasedText || LeaseBusy; an overdue
// MEDIA lease is neither, so it stays a target there too, ranked last.
func TestOverdueMediaLeaseStaysATargetForTheVisionAndTextLanes(t *testing.T) {
	both := []string{"classify", "extract"}
	overdue := NodeView{NodeID: "over", Tasks: []string{"vision", "text"}, TextTasks: both, LeaseOverdue: true}
	clean := NodeView{NodeID: "clean", Tasks: []string{"vision", "text"}, TextTasks: both, QueueDepth: 4}
	if i, ok := PlaceVision([]NodeView{overdue}, "vqa"); !ok || i != 0 {
		t.Fatalf("PlaceVision over an overdue-only fleet = %d,%v: must still place", i, ok)
	}
	if i, ok := PlaceVision([]NodeView{overdue, clean}, "vqa"); !ok || i != 1 {
		t.Fatalf("PlaceVision = %d,%v, want the clean node ranked ahead of the overdue one", i, ok)
	}
	if i, ok := PlaceText([]NodeView{overdue}, "classify"); !ok || i != 0 {
		t.Fatalf("PlaceText over an overdue-only fleet = %d,%v: must still place", i, ok)
	}
	if i, ok := PlaceText([]NodeView{overdue, clean}, "classify"); !ok || i != 1 {
		t.Fatalf("PlaceText = %d,%v, want the clean node ranked ahead of the overdue one", i, ok)
	}
}

// An operator who sees work land on the worse node needs the lease behind the
// order named, the same way a long or text lease is named.
func TestLeasedLanesNamesAnOverdueLease(t *testing.T) {
	overdue := eligibleRemote()
	overdue.NodeID, overdue.LeaseOverdue = "node-overdue", true
	clean := eligibleRemote()
	clean.NodeID = "node-clean"
	got := leasedLanes([]NodeView{overdue, clean})
	if len(got) != 1 || !strings.Contains(got[0], "node-overdue") || !strings.Contains(got[0], "overdue") {
		t.Fatalf("leasedLanes = %v, want only the overdue node, named as overdue", got)
	}
}

// ---------------------------------------------------------------------------
// free cards
// ---------------------------------------------------------------------------

func TestFreeCardDefinition(t *testing.T) {
	cases := []struct {
		name      string
		devs      []gpuprobe.Device
		free      int
		wantKnown bool
	}{
		{"all idle", threeCards(0, 0, 0), 3, true},
		{"busy card is not free", threeCards(97, 0, 0), 2, true},
		{"util just under the working line is free", threeCards(14, 14, 14), 3, true},
		{"util at the working line is not", threeCards(15, 15, 15), 0, true},
		{"display card is never free", []gpuprobe.Device{card(0, 0, 15, true), card(1, 0, 15, false), card(2, 0, 15, false)}, 2, true},
		{"idle util but no VRAM left is not free", []gpuprobe.Device{card(0, 0, 1, false), card(1, 0, 15, false)}, 1, true},
		{"a card whose util is unknown is never idle", []gpuprobe.Device{card(0, 0, 15, false), {Index: 1, UUID: "GPU-2222bbbb-3333-4444-5555-666677778888", TotalGiB: 16, FreeGiB: 16}}, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := NodeView{Devices: tc.devs}
			free, total, known := v.FreeCards()
			if known != tc.wantKnown || free != tc.free || total != len(tc.devs) {
				t.Fatalf("FreeCards() = %d of %d, known %v; want %d of %d, known %v", free, total, known, tc.free, len(tc.devs), tc.wantKnown)
			}
		})
	}
	// Unknown utilisation is never idle, and a node whose every card reads
	// unknown is UNKNOWN, not full.
	unk := NodeView{Devices: []gpuprobe.Device{{Index: 0, UUID: "GPU-1111aaaa-2222-3333-4444-555566667777", TotalGiB: 16, FreeGiB: 16}}}
	if free, _, known := unk.FreeCards(); known || free != 0 {
		t.Fatalf("a node whose util is unknown on every card: free %d known %v, want unknown", free, known)
	}
}

// A seat that spans cards needs that many free ones: the pair layer served by
// two cards is not served by one free card.
func TestFreeCardsForALayerNeedTheCardsTheSeatSpans(t *testing.T) {
	rows := []placetable.LayerRow{
		{Name: "single", Devices: []string{"0"}, Seats: []placetable.SeatRow{{Role: "agent", Device: "0", FootprintGiB: 12}}},
		{Name: "pair", Devices: []string{"0,1"}, Seats: []placetable.SeatRow{{Role: "agent", Device: "0,1", FootprintGiB: 24}}},
	}
	oneFree := NodeView{Devices: threeCards(97, 97, 0), Layers: rows}
	room := cardRoomFor(oneFree, "pair")
	if !room.known || room.free != 1 || room.need != 2 {
		t.Fatalf("pair on one free card = %+v, want free 1 needing 2", room)
	}
	if cardTier(oneFree, "pair") != tierNone {
		t.Fatal("one free card must not read as room for a two-card layer")
	}
	if cardTier(oneFree, "single") != tierSome {
		t.Fatal("one free card is room for the single layer")
	}
	// VRAM: the single layer's seat is 12 GiB; a card with 10 free does not fit.
	tight := NodeView{Devices: []gpuprobe.Device{card(0, 0, 10, false)}, Layers: rows}
	if cardTier(tight, "single") != tierNone {
		t.Fatal("an idle card without the footprint's VRAM must not count as free for that layer")
	}
	// A layer the node does not declare says nothing about its cards.
	if cardTier(oneFree, "triple") != tierSome {
		t.Fatalf("an undeclared layer falls back to the layer-agnostic count, got tier %d", cardTier(oneFree, "triple"))
	}
}

// A warm agent seat fills the card it sits on. 0 % utilisation with 2 GiB free
// on a 16 GiB card is the seat idling, not a card that is full of something
// else, and a node serving from a resident seat must not lose to a cold node
// for being warm. The node's own seat_loaded is the evidence; unknown residency
// waives nothing, and a card that is busy is busy whatever sits on it.
func TestAWarmSeatsOwnCardCountsAsFreeForIt(t *testing.T) {
	loaded, notLoaded := true, false
	warmCard := []gpuprobe.Device{card(0, 0, 2, false)}

	warm := NodeView{Devices: warmCard, SeatLoaded: &loaded}
	if cardTier(warm, "") != tierSome {
		t.Fatalf("an idle card holding the node's loaded seat read as full: tier %d", cardTier(warm, ""))
	}
	if free, total, known := warm.FreeCards(); !known || free != 1 || total != 1 {
		t.Fatalf("FreeCards() = %d of %d, known %v; want 1 of 1", free, total, known)
	}
	if cardTier(NodeView{Devices: warmCard, SeatLoaded: &notLoaded}, "") != tierNone {
		t.Fatal("2 GiB free with the seat NOT loaded is a card full of something else, and must read as full")
	}
	if cardTier(NodeView{Devices: warmCard}, "") != tierNone {
		t.Fatal("unknown residency must waive nothing: a node that does not say its seat is loaded gets no credit for it")
	}
	busyWarm := NodeView{Devices: []gpuprobe.Device{card(0, 97, 2, false)}, SeatLoaded: &loaded}
	if cardTier(busyWarm, "") != tierNone {
		t.Fatal("a resident seat vouches for its card's VRAM, never for its utilisation: a busy card is busy")
	}
	// A seat that spans two cards vouches for two, and no more.
	rows := func(loaded bool) []placetable.LayerRow {
		return []placetable.LayerRow{{Name: "pair", Devices: []string{"0,1"}, Seats: []placetable.SeatRow{{Role: "agent", Device: "0,1", FootprintGiB: 24, Loaded: loaded}}}}
	}
	pairCards := []gpuprobe.Device{card(0, 0, 2, false), card(1, 0, 2, false), card(2, 0, 2, false)}
	if cardTier(NodeView{Devices: pairCards, Layers: rows(true)}, "pair") != tierSome {
		t.Fatal("a loaded pair seat on two idle cards read as full of itself")
	}
	if room := cardRoomFor(NodeView{Devices: pairCards, Layers: rows(true)}, "pair"); room.free != 2 {
		t.Fatalf("a loaded pair seat vouches for the 2 cards it spans, not all 3: free = %d", room.free)
	}
	if cardTier(NodeView{Devices: pairCards, Layers: rows(false)}, "pair") != tierNone {
		t.Fatal("a pair seat that is not loaded has no cards to vouch for")
	}
	// The node-level seat_loaded describes the default agent seat, which may be
	// another layer's: it is no evidence that the PAIR seat is loaded.
	if cardTier(NodeView{Devices: pairCards, Layers: rows(false), SeatLoaded: &loaded}, "pair") != tierNone {
		t.Fatal("the node's default-seat seat_loaded must not vouch for a different layer's seat")
	}
}

// betterRemote's free-card key: known-with-room beats unknown beats known-full,
// computed per node (a total preorder), and sits below the lease, saturation
// and capacity keys.
func TestBetterRemoteFreeCardTiers(t *testing.T) {
	st := schemaSubtask()
	mk := func(id string, devs []gpuprobe.Device) NodeView {
		v := eligibleRemote()
		v.NodeID, v.Devices = id, devs
		return v
	}
	room := mk("room", threeCards(97, 0, 0))
	unknown := mk("unknown", nil)
	full := mk("full", threeCards(97, 97, 97))

	order := []NodeView{room, unknown, full}
	for i := range order {
		for j := range order {
			got := betterRemote("seed", &st, 0, order[i], order[j])
			want := i < j
			if got != want {
				t.Errorf("betterRemote(%s over %s) = %v, want %v", order[i].NodeID, order[j].NodeID, got, want)
			}
		}
	}
	// Lower keys do not outvote it: a node with room but a deeper queue wins,
	// because per-card truth outranks a queue COUNT.
	deep := mk("deep", threeCards(0, 97, 97))
	deep.QueueDepth = 2
	shallow := mk("shallow", threeCards(97, 97, 97))
	shallow.QueueDepth = 1
	if !betterRemote("seed", &st, 0, deep, shallow) {
		t.Fatal("a node with a free card must beat one with none even at a deeper queue depth")
	}
	// Higher keys still outvote it: saturation first.
	sat := mk("sat", threeCards(0, 0, 0))
	sat.SaturationKnown, sat.SaturationHigh = true, true
	if betterRemote("seed", &st, 0, sat, full) {
		t.Fatal("a saturated node must not beat an unsaturated one because it has a free card")
	}
	// And it replaces the util scalar, it does not stack on it: equal tiers
	// still fall through to utilisation.
	a := mk("a", threeCards(30, 30, 0))
	a.WorkUtilPct, a.WorkUtilKnown = 30, true
	b := mk("b", threeCards(80, 0, 0))
	b.WorkUtilPct, b.WorkUtilKnown = 80, true
	if !betterRemote("seed", &st, 0, a, b) {
		t.Fatal("equal free-card tiers must fall through to the utilisation tie-break")
	}
}

func TestPlaceAutoRemotePrefersNodeWithFreeCard(t *testing.T) {
	st := oneStepSchemaContract(300, false)
	busy := eligibleRemote()
	busy.NodeID, busy.Devices = "node-a", threeCards(97, 96, 99)
	free := eligibleRemote()
	free.NodeID, free.Devices = "node-b", threeCards(97, 96, 2)
	r := &runner{route: "remote"}
	// The all-busy node is listed first, so roster order would pick it.
	slot := r.placeAutoRemote("seed", st, localNode(), []NodeView{busy, free}, []string{"http://node-a", "http://node-b"}, true, map[string]int{}, nil)
	if slot.view.NodeID != "node-b" {
		t.Fatalf("placed on %q (%s), want the node with a free card", slot.view.NodeID, slot.reason)
	}
	if !strings.Contains(slot.reason, "free card") {
		t.Fatalf("reason %q must name the free-card basis", slot.reason)
	}
}

// Headroom is cards, not one util scalar. Node A reads 90 % on its busiest card
// (one card is a game, a render, a foreign job) and has two idle ones; node B
// reads 30 % everywhere and has none. The scalar prefers B. Dealing three
// subtasks, the two free cards of A take the first two, and the third, with no
// free card left anywhere, falls back to the old ordering.
func TestHeadroomCountsFreeCardsNotNodeUtil(t *testing.T) {
	a := eligibleRemote()
	a.NodeID, a.Devices = "node-a", threeCards(90, 0, 0)
	a.WorkUtilPct, a.WorkUtilKnown = 90, true
	a.MaxConcurrentJobs = 4
	b := eligibleRemote()
	b.NodeID, b.Devices = "node-b", threeCards(30, 30, 30)
	b.WorkUtilPct, b.WorkUtilKnown = 30, true
	b.MaxConcurrentJobs = 4

	st := oneStepSchemaContract(300, false).Contract
	r := &runner{route: "remote"}
	slots := r.dealAutoRemote([]core.AgentContract{st, st, st}, localNode(), []NodeView{a, b}, []string{"http://node-a", "http://node-b"}, true, nil)
	var got []string
	for _, s := range slots {
		got = append(got, s.view.NodeID)
	}
	if strings.Join(got, ",") != "node-a,node-a,node-b" {
		t.Fatalf("deal = %v, want node-a twice (its two free cards) and then node-b once the free cards are spent", got)
	}
	// And the hard capacity limit is untouched: a node at its execution ceiling
	// is not dealt work however many cards are free.
	a.JobsRunning = 4
	slots = r.dealAutoRemote([]core.AgentContract{st}, localNode(), []NodeView{a, b}, []string{"http://node-a", "http://node-b"}, true, nil)
	if slots[0].view.NodeID != "node-b" {
		t.Fatalf("a node at max_concurrent_jobs was dealt work (%s): free cards must rank, never override the hard ceiling", slots[0].view.NodeID)
	}
}

// route=spread keeps its one-subtask-per-seat-per-cycle invariant; the free-card
// preference only decides WHICH seat takes the slot inside the cycle.
func TestFitPickPrefersTheNodeWithAFreeCard(t *testing.T) {
	st := schemaSubtask()
	full := eligibleRemote()
	full.NodeID, full.Devices = "full", threeCards(97, 97, 97)
	room := eligibleRemote()
	room.NodeID, room.Devices = "room", threeCards(97, 0, 97)
	nodes := []NodeView{full, room}
	bases := []string{"http://full", "http://room"}

	if k := fitPick(st, nodes, bases, 0, map[string]bool{}, nil); k != 1 {
		t.Fatalf("fitPick = %d, want the node with a free card (1)", k)
	}
	// Once the free-card node has had its turn this cycle, the other one still
	// gets its subtask: the cycle invariant holds.
	if k := fitPick(st, nodes, bases, 0, map[string]bool{"http://room": true}, nil); k != 0 {
		t.Fatalf("fitPick with the free-card node already dealt = %d, want the remaining node (0)", k)
	}
}

// Through the real deal: with two otherwise identical remotes, the rotation
// would hand the first remote slot to the first-listed one. The node whose cards
// are all busy is listed first, and the free-card node takes the first remote
// slot; the cycle still gives the other node its subtask, so every seat is dealt.
func TestDealSpreadGivesTheFirstRemoteSlotToTheNodeWithAFreeCard(t *testing.T) {
	full, room := fitBigRemote, fitBigRemote
	full.NodeID, full.Devices = "full-remote", threeCards(97, 96, 99)
	room.NodeID, room.Devices = "room-remote", threeCards(97, 96, 1)
	r := fitRunner(full, room)
	where, _ := deal(r, repeatGoal(fitMechGoal, 3)...)
	if !equalStrings(where, []string{"local-box", "room-remote", "full-remote"}) {
		t.Fatalf("deal = %v, want the free-card node first among the remotes and every seat dealt once", where)
	}
	// With no per-card truth anywhere the deal is exactly the rotation order.
	a, b := fitBigRemote, fitBigRemote
	a.NodeID, b.NodeID = "a-remote", "b-remote"
	where, _ = deal(fitRunner(a, b), repeatGoal(fitMechGoal, 3)...)
	if !equalStrings(where, []string{"local-box", "a-remote", "b-remote"}) {
		t.Fatalf("deal without devices = %v, want the unchanged rotation order", where)
	}
}
