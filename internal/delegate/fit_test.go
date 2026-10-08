package delegate

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// fitSubtask builds a placeable subtask around one goal string. EstTokens is
// tiny on purpose in most rows: these tests are about SHAPE and ceiling ORDER,
// so every seat under test is adequate unless the case says otherwise.
func fitSubtask(goal string, est int) Subtask {
	return Subtask{
		Contract: core.AgentContract{
			SchemaVersion: core.AgentWireSchemaVersion,
			Goal:          goal,
			OutputSchema:  json.RawMessage(`{"properties":{"answer":{"type":"string"}}}`),
			MaxSteps:      4,
			TimeoutSec:    30,
		},
		EstTokens: est,
	}
}

// TestInferKindRoutesRealisticGoals is the routing table. The first four rows
// are the cases measured WRONG under a naive reasoning pattern that carries a
// bare "how " alternative and defaults an unmatched goal to reasoning:
//
//	"how many files changed"        -> reasoning (the bare "how " alternative fired)
//	"what is the default queue cap" -> reasoning (no match, expensive default)
//
// Both must read MECHANICAL: the first is a count, the second is ambiguous —
// and on a harness built for cheap grunt work, ambiguity belongs on the cheap
// seat, not the expensive one.
func TestInferKindRoutesRealisticGoals(t *testing.T) {
	cases := []struct {
		goal string
		want Kind
		why  string
	}{
		{"how many files changed", KindMechanical, "a count, not an explanation - `how many` must beat a bare `how`"},
		{"what is the default queue cap", KindMechanical, "unmatched -> the CHEAP seat, never the expensive one"},
		{"count the exported functions", KindMechanical, "counting verb"},
		{"explain how the retry path interacts with the queue cap", KindReasoning, "explanation verb"},

		{"extract the version string from each file and report it", KindMechanical, "extraction"},
		{"list every exported function name in these files", KindMechanical, "listing"},
		{"summarize each log file into three bullets", KindMechanical, "digesting"},
		{"tabulate the queue depth reported by each node", KindMechanical, "tabulation"},
		{"how much context does each seat advertise", KindMechanical, "a quantity question is a lookup"},
		{"count how many times the retry fires", KindMechanical, "the quantity rule runs before the explanation rule"},
		{"", KindMechanical, "an empty goal is maximally ambiguous -> cheap seat"},

		{"why does the guard fire before the lease is released", KindReasoning, "causal question"},
		{"trace how the queue cap propagates across these modules", KindReasoning, "cross-module tracing"},
		{"compare the two retry implementations and say which is safer", KindReasoning, "comparison"},
		{"describe the relationship between the lease and the placement gate", KindReasoning, "relationship"},
		{"explain why the extraction step fails on an empty file", KindReasoning, "explanation outranks the mechanical verb it contains"},
		{"how the retry path interacts with the queue cap", KindReasoning, "a genuine how-question still reads as reasoning"},
		{"work out where the cap is enforced across all files", KindReasoning, "`across all files` is the natural cross-file phrasing"},
	}
	for _, c := range cases {
		if got := inferKind(fitSubtask(c.goal, 100)); got != c.want {
			t.Errorf("inferKind(%q) = %v, want %v (%s)", c.goal, got, c.want, c.why)
		}
	}
}

// TestScoreFitOrdersSeatsByShape: reasoning wants ceiling headroom, mechanical
// wants the SMALLEST ADEQUATE seat so the roomier seat stays free for work that
// needs it.
func TestScoreFitOrdersSeatsByShape(t *testing.T) {
	big := NodeView{NodeID: "big-seat", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 131072}
	small := NodeView{NodeID: "small-seat", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 32768}

	reason := fitSubtask("explain how these modules interact and why the guard fires", 100)
	if !scoreFit(reason, big).beats(scoreFit(reason, small)) {
		t.Errorf("reasoning must prefer the roomier seat: big=%+v small=%+v", scoreFit(reason, big), scoreFit(reason, small))
	}
	mech := fitSubtask("list every exported function name in these files", 100)
	if !scoreFit(mech, small).beats(scoreFit(mech, big)) {
		t.Errorf("mechanical must prefer the smaller adequate seat: small=%+v big=%+v", scoreFit(mech, small), scoreFit(mech, big))
	}
}

// TestScoreFitRanksAnInadequateSeatLast pins the ADEQUACY half of "smallest
// adequate seat": a seat too small to hold the contract plus the loop's own
// reserve must never win the mechanical contest by being cheap, and an
// UNADVERTISED ceiling is not a small one — unknown is not a capacity.
func TestScoreFitRanksAnInadequateSeatLast(t *testing.T) {
	adequateSeat := NodeView{NodeID: "adequate", AgentCtxTokens: 32768}
	tooSmall := NodeView{NodeID: "too-small", AgentCtxTokens: 4096}
	unadvertised := NodeView{NodeID: "unadvertised"}

	for _, goal := range []string{
		"list every exported function name in these files",
		"explain how these modules interact",
	} {
		st := fitSubtask(goal, 8000) // 8000 + specReserve is over 4096
		if adequate(st, tooSmall) {
			t.Fatalf("%q: 8000+%d must not fit a 4096 seat", goal, specReserve)
		}
		if !scoreFit(st, adequateSeat).beats(scoreFit(st, tooSmall)) {
			t.Errorf("%q: an inadequate seat outranked an adequate one", goal)
		}
		if !scoreFit(st, adequateSeat).beats(scoreFit(st, unadvertised)) {
			t.Errorf("%q: an UNADVERTISED ceiling outranked an advertised, adequate one", goal)
		}
	}
}

// TestAnInadequateSeatRanksLastBesideARatedSlowAdequateSeat is the same promise with a RATED adequate seat, which
// the test above never exercised (its fixtures publish no rate). The score of a rated mechanical seat is
// -int(eta x 10) x 2^24 - window, and past an eta of about 12.8 s that is below math.MinInt32: when "cannot hold
// the contract" was that number, a slow adequate seat ranked BELOW a seat that could not hold the contract at
// all, and the spread pick dealt the contract to the seat that cannot run it. The pick below is driven directly
// (fitPickWith does no eligibility filtering), because that is where the collision was latent: placeSpreadWith
// filters through remoteEligible first, and a ranking must not depend on that.
func TestAnInadequateSeatRanksLastBesideARatedSlowAdequateSeat(t *testing.T) {
	for _, goal := range []string{fitMechGoal, fitReasonGoal} {
		st := fitSubtask(goal, 8000) // 8000 + specReserve is over 4096
		slow := etaFixtureRemote("slow-adequate", 32768, 5, 0)
		small := etaFixtureRemote("small-inadequate", 4096, 40, 0) // rated, and 8x faster: eta cannot be what ranks it
		eta, rated := etaFor(st, slow)
		if !rated || eta <= 13 {
			t.Fatalf("%q: fixture bug: the adequate seat must be rated with an eta above 13 s, got %.1f s (rated %v)", goal, eta, rated)
		}
		if !adequate(st, slow) || adequate(st, small) {
			t.Fatalf("%q: fixture bug: adequate(slow)=%v adequate(small)=%v", goal, adequate(st, slow), adequate(st, small))
		}
		ks, kt := scoreFit(st, slow), scoreFit(st, small)
		if inferKind(st) == KindMechanical {
			// The collision itself: the adequate seat's raw score sits below the value the inadequate seat used to carry.
			if int64(ks.score) >= math.MinInt32 {
				t.Fatalf("%q: fixture bug: the adequate seat's score %d is not below math.MinInt32, so this case would not have collided", goal, ks.score)
			}
		}
		if !ks.adequate || kt.adequate {
			t.Fatalf("%q: adequacy: slow=%+v small=%+v", goal, ks, kt)
		}
		if !ks.beats(kt) || kt.beats(ks) {
			t.Errorf("%q: the adequate seat %+v must beat the inadequate one %+v, and not the reverse", goal, ks, kt)
		}
		// The spread pick, in both roster orders and from either rotation slot, so the incumbent is each seat in turn.
		for _, order := range [][]NodeView{{small, slow}, {slow, small}} {
			bases := []string{"http://" + order[0].NodeID, "http://" + order[1].NodeID}
			for slot := 0; slot < 2; slot++ {
				k := fitPick(st, order, bases, slot, map[string]bool{}, nil)
				if k < 0 || order[k].NodeID != "slow-adequate" {
					t.Errorf("%q: fitPick(slot %d, %s first) chose index %d, want the adequate seat although it is slower", goal, slot, order[0].NodeID, k)
				}
			}
			// Ranking last is not excluding: once the adequate seat has taken its turn this cycle, the other still gets its slot.
			dealt := map[string]bool{"http://slow-adequate": true}
			if k := fitPick(st, order, bases, 0, dealt, nil); k < 0 || order[k].NodeID != "small-inadequate" {
				t.Errorf("%q: fitPick with the adequate seat dealt chose index %d, want the remaining seat", goal, k)
			}
		}
	}
}

// TestAdequacyOutranksEveryDemotionKeyOfTheSpreadPick: the keys fitPickWith leads with (an overdue lease, a
// saturated node, free cards) order the seats that CAN hold the contract. None of them may lift a seat that
// cannot hold it above one that can: it would fail at dispatch however free its cards are.
func TestAdequacyOutranksEveryDemotionKeyOfTheSpreadPick(t *testing.T) {
	st := fitSubtask(fitMechGoal, 8000)
	for _, tc := range []struct {
		name string
		edit func(slow, small *NodeView)
	}{
		{"the inadequate seat has a free card and the adequate seat has none", func(slow, small *NodeView) {
			slow.Devices, small.Devices = threeCards(97, 97, 97), threeCards(97, 0, 97)
		}},
		{"the adequate seat is saturated", func(slow, small *NodeView) {
			slow.MaxQueueDepth, slow.QueueDepth = 1, 1
		}},
		{"the adequate seat holds an overdue lease", func(slow, small *NodeView) {
			slow.LeaseOverdue = true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slow := etaFixtureRemote("slow-adequate", 32768, 5, 0)
			small := etaFixtureRemote("small-inadequate", 4096, 40, 0)
			tc.edit(&slow, &small)
			for _, order := range [][]NodeView{{small, slow}, {slow, small}} {
				bases := []string{"http://" + order[0].NodeID, "http://" + order[1].NodeID}
				k := fitPick(st, order, bases, 0, map[string]bool{}, nil)
				if k < 0 || order[k].NodeID != "slow-adequate" {
					t.Errorf("fitPick (%s first) chose index %d, want the adequate seat: adequacy is the first key", order[0].NodeID, k)
				}
			}
		})
	}
}

// A node that publishes layer rows is judged by the table's decision, and a decision that defers or waits is
// inadequate for the ranking exactly as it is for the gate.
func TestALayerDecisionThatDefersOrWaitsIsAnInadequateKey(t *testing.T) {
	big := Subtask{Contract: contractOfTokens(200_000)}
	big.EstTokens = EstimateTokens(big.Contract)
	deferred := eligibleRemote()
	deferred.Layers = pairOnlyRows(t) // largest window 163,840: a defer
	if dec, ok := remoteDecision(big, deferred); !ok || !dec.Defer {
		t.Fatalf("fixture bug: want a defer, got %+v (ok %v)", dec, ok)
	}
	waiting := eligibleRemote()
	waiting.Layers = fixtureRows(t, busyPair(2)) // overflow to the long seat evicts a mid-flight agent seat: a wait
	if dec, ok := remoteDecision(big, waiting); !ok || !dec.Wait || dec.Defer {
		t.Fatalf("fixture bug: want a wait, got %+v (ok %v)", dec, ok)
	}
	idle := eligibleRemote()
	idle.Layers = fixtureRows(t, idlePair())
	if k := scoreFit(big, idle); !k.adequate {
		t.Fatalf("fixture bug: an idle pair holds the contract on its long seat, got %+v", k)
	}
	for name, v := range map[string]NodeView{"defer": deferred, "wait": waiting} {
		if k := scoreFit(big, v); k.adequate {
			t.Errorf("a %s decision is adequate: %+v", name, k)
		}
		if !scoreFit(big, idle).beats(scoreFit(big, v)) {
			t.Errorf("a %s decision ranks level with or above a seat that holds the contract", name)
		}
	}
}

// fitRunner builds a runner whose spread snapshot is exactly these remotes.
func fitRunner(views ...NodeView) *runner {
	bases := make([]string, len(views))
	for i, v := range views {
		bases[i] = "http://" + v.NodeID + ":18811"
	}
	return &runner{spreadViews: views, spreadBases: bases}
}

func fitLocal() NodeView {
	return NodeView{NodeID: "local-box", AgentSeat: "local-seat", Local: true}
}

var (
	fitBigRemote   = NodeView{NodeID: "big-remote", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 131072}
	fitMidRemote   = NodeView{NodeID: "mid-remote", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 65536}
	fitSmallRemote = NodeView{NodeID: "small-remote", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 32768}
)

const (
	fitMechGoal   = "list every exported function name in these files"
	fitReasonGoal = "explain how the retry path interacts with the queue cap"
)

// deal runs the REAL dealSpread over these goals and returns the node id each
// subtask landed on. It calls the production function rather than
// re-implementing its loop: a test that mirrors the code under test stops
// covering it the moment the code changes.
func deal(r *runner, goals ...string) ([]string, []spreadSlot) {
	contracts := make([]core.AgentContract, len(goals))
	for i, g := range goals {
		contracts[i] = fitSubtask(g, 100).Contract
	}
	slots := r.dealSpread(contracts, fitLocal())
	where := make([]string, len(slots))
	for i, sl := range slots {
		where[i] = sl.view.NodeID
	}
	return where, slots
}

func repeatGoal(goal string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = goal
	}
	return out
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestDealSpreadSameShapedFanOutReachesEverySeat is the property the fit score
// is NOT allowed to buy: within each deal cycle every eligible seat takes at
// most one subtask, so a same-shaped fan-out still reaches all of them.
//
// A free per-slot re-pick fails this outright — the smallest seat wins every
// mechanical slot and the roomiest wins every reasoning slot, which is exactly
// the stacking route=spread exists to remove. The roster is UNEQUAL on purpose:
// with equal ceilings every implementation passes, which is how a collapse can
// ship green.
func TestDealSpreadSameShapedFanOutReachesEverySeat(t *testing.T) {
	nodeB := NodeView{NodeID: "node-b", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 131072}
	nodeA := NodeView{NodeID: "node-a", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 32768}
	nodeC := NodeView{NodeID: "node-c", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 32768}

	for _, tc := range []struct{ name, goal string }{
		{"mechanical", fitMechGoal},
		{"reasoning", fitReasonGoal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fitRunner(nodeB, nodeA, nodeC)
			where, _ := deal(r, repeatGoal(tc.goal, 8)...)

			counts := map[string]int{}
			for _, id := range where {
				counts[id]++
			}
			for _, id := range []string{"local-box", "node-b", "node-a", "node-c"} {
				if counts[id] != 2 {
					t.Errorf("%s got %d of 8 subtasks, want 2 — deal was %v", id, counts[id], where)
				}
			}
			// The invariant itself, asserted directly: no seat twice in a cycle.
			for c := 0; c < 2; c++ {
				cycle := where[c*4 : c*4+4]
				seen := map[string]bool{}
				for _, id := range cycle {
					if seen[id] {
						t.Errorf("cycle %d dealt %s twice: %v", c, id, cycle)
					}
					seen[id] = true
				}
			}
		})
	}
}

// TestDealSpreadFitPicksWithinTheCycle: fit still decides WHICH seat a subtask
// gets, it just cannot re-pick the same winner every slot. On a three-tier
// roster the first remote slot of a cycle goes to the smallest seat for
// mechanical work and the roomiest for reasoning work, and the rest of the
// cycle follows in fit order.
func TestDealSpreadFitPicksWithinTheCycle(t *testing.T) {
	// Roster order deliberately puts the BIG remote first, so rotation and fit
	// disagree about the first remote slot under both shapes.
	r := fitRunner(fitBigRemote, fitMidRemote, fitSmallRemote)
	mech, _ := deal(r, repeatGoal(fitMechGoal, 4)...)
	if want := []string{"local-box", "small-remote", "mid-remote", "big-remote"}; !equalStrings(mech, want) {
		t.Errorf("mechanical deal = %v, want %v (smallest adequate seat first)", mech, want)
	}

	r = fitRunner(fitBigRemote, fitMidRemote, fitSmallRemote)
	reason, _ := deal(r, repeatGoal(fitReasonGoal, 4)...)
	if want := []string{"local-box", "big-remote", "mid-remote", "small-remote"}; !equalStrings(reason, want) {
		t.Errorf("reasoning deal = %v, want %v (roomiest seat first)", reason, want)
	}
}

// TestDealSpreadMixedShapesStillFillTheCycle: the deal is joint, so a cycle
// holding contracts of DIFFERENT shapes still touches every seat once — the
// case no per-subtask pick can satisfy (see dealSpread's proof).
func TestDealSpreadMixedShapesStillFillTheCycle(t *testing.T) {
	r := fitRunner(fitBigRemote, fitMidRemote, fitSmallRemote)
	where, _ := deal(r, fitMechGoal, fitMechGoal, fitReasonGoal, fitMechGoal)
	seen := map[string]bool{}
	for _, id := range where {
		if seen[id] {
			t.Fatalf("a mixed-shape cycle dealt %s twice: %v", id, where)
		}
		seen[id] = true
	}
	if where[1] != "small-remote" {
		t.Errorf("the mechanical slot went to %s, want small-remote — %v", where[1], where)
	}
}

// TestDealSpreadKeepsSubtaskZeroLocal pins the guarantee the fit score is NOT
// allowed to break: subtask 0 lands on the local seat whatever its shape. A
// single-subtask spread is the riskiest case for a shape heuristic — getting it
// wrong sends the entire run off-box on one regex match.
func TestDealSpreadKeepsSubtaskZeroLocal(t *testing.T) {
	for _, goal := range []string{fitMechGoal, fitReasonGoal, "what is the default queue cap"} {
		r := fitRunner(fitBigRemote, fitSmallRemote)
		where, slots := deal(r, goal)
		if !slots[0].view.Local {
			t.Errorf("subtask 0 (%q) landed on %s — slot 0 must stay local", goal, where[0])
		}
	}
}

// TestDealSpreadLeavesTheLocalRotationSlotAlone: the fit contest ranks seats by
// ADVERTISED ceiling, and the local seat advertises none in a delegator run, so
// it is never entered into the contest — it keeps every rotation slot it already
// had (i mod len == 0), not only subtask 0.
func TestDealSpreadLeavesTheLocalRotationSlotAlone(t *testing.T) {
	r := fitRunner(fitBigRemote, fitSmallRemote)
	_, slots := deal(r, repeatGoal(fitMechGoal, 4)...)
	if !slots[3].view.Local {
		t.Errorf("subtask 3 of a 3-seat spread landed on %s, want the local rotation slot", slots[3].view.NodeID)
	}
}

// TestDealSpreadTiesKeepRotating: with equal-ceiling remotes the fit score
// cannot separate them, so the deal must reproduce the old rotation exactly —
// the compatibility pin for every fleet whose seats match.
func TestDealSpreadTiesKeepRotating(t *testing.T) {
	a := NodeView{NodeID: "node-a", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 32768}
	b := NodeView{NodeID: "node-b", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 32768}
	r := fitRunner(a, b)
	where, _ := deal(r, repeatGoal(fitMechGoal, 4)...)
	if want := []string{"local-box", "node-a", "node-b", "local-box"}; !equalStrings(where, want) {
		t.Errorf("deal = %v, want %v", where, want)
	}
}

// TestDealSpreadSkipsASeatThatCannotHoldTheContract: per-subtask eligibility
// drops an over-size seat before the fit score ever sees it, so a mechanical
// contract cannot be dealt to the small seat merely because it is the cheapest.
func TestDealSpreadSkipsASeatThatCannotHoldTheContract(t *testing.T) {
	tiny := NodeView{NodeID: "tiny-remote", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 8192}
	r := fitRunner(fitBigRemote, tiny)
	// A REAL over-size contract: dealSpread recomputes EstTokens from the
	// contract itself (as production does), so the bulk has to be genuine —
	// 60 KiB of context doc estimates to ~20k tokens, past the 8192 seat and
	// well inside the 131072 one.
	big := fitSubtask(fitMechGoal, 0).Contract
	big.Context = []core.ContextDoc{{Name: "big.txt", Text: strings.Repeat("x", 60000)}}
	if est := EstimateTokens(big); est+specReserve <= tiny.AgentCtxTokens || est+specReserve > fitBigRemote.AgentCtxTokens {
		t.Fatalf("fixture off: est=%d must exceed the tiny seat and fit the big one", est)
	}
	slots := r.dealSpread([]core.AgentContract{big, big}, fitLocal())
	if slots[1].view.NodeID != "big-remote" {
		t.Errorf("landed on %s (%s), want big-remote — the tiny seat cannot hold the contract",
			slots[1].view.NodeID, slots[1].reason)
	}
}

// TestDealSpreadReasonNamesTheRotationSlotAndRule: "slot N of M" must stay the
// ROTATION slot (an operator reading results[].placement counts subtasks, not
// roster indices), and the reason must name the rule that read the shape —
// "mechanical/default" and "mechanical/mechanical-verb" send you to different
// fixes.
func TestDealSpreadReasonNamesTheRotationSlotAndRule(t *testing.T) {
	r := fitRunner(fitBigRemote, fitMidRemote, fitSmallRemote)
	_, slots := deal(r, fitMechGoal, fitMechGoal, "what is the default queue cap")

	if got := slots[1].reason; !strings.Contains(got, "slot 2 of 4") || !strings.Contains(got, "fit=mechanical/mechanical-verb") {
		t.Errorf("subtask 1 reason = %q, want the rotation slot 2 of 4 and the deciding rule", got)
	}
	if got := slots[2].reason; !strings.Contains(got, "slot 3 of 4") || !strings.Contains(got, "fit=mechanical/default") {
		t.Errorf("subtask 2 reason = %q, want slot 3 of 4 and the no-match rule named `default`", got)
	}
	if got := slots[0].reason; got != "route=spread → local (slot 1 of 4)" {
		t.Errorf("local reason = %q, want the unchanged local form", got)
	}
}

// TestDealSpreadSkipsTheLocalSlotWhenTheLocalSeatIsBusy (0.113.20): a local seat
// that already holds a request loses EVERY rotation slot — subtask 0 included —
// to the eligible remotes with room, dealt one-per-seat-per-cycle as before,
// and each reason names the count that was read.
func TestDealSpreadSkipsTheLocalSlotWhenTheLocalSeatIsBusy(t *testing.T) {
	r := fitRunner(fitBigRemote, fitSmallRemote)
	r.spreadLocalBusy = busyReading{busy: true, inflight: 3, note: "metrics"}
	where, slots := deal(r, repeatGoal(fitMechGoal, 4)...)
	for i, sl := range slots {
		if sl.view.Local {
			t.Errorf("subtask %d landed local (%s) while the local seat was busy", i, where[i])
		}
		if !strings.Contains(sl.reason, "local seat busy: 3 in flight") {
			t.Errorf("subtask %d reason %q must name the in-flight count", i, sl.reason)
		}
	}
	counts := map[string]int{}
	for _, w := range where {
		counts[w]++
	}
	if counts["big-remote"] != 2 || counts["small-remote"] != 2 {
		t.Errorf("deal = %v; want two subtasks on each remote (one per seat per cycle)", where)
	}
}

// TestDealSpreadBusyLocalStaysLocalWhenNoRemoteHasRoom: the busy rule is an
// optimisation, never a way to lose work — with every eligible remote saturated
// the deal falls back to the ordinary rotation and says why.
func TestDealSpreadBusyLocalStaysLocalWhenNoRemoteHasRoom(t *testing.T) {
	full := fitBigRemote
	full.SaturationKnown, full.SaturationHigh = true, true
	r := fitRunner(full)
	r.spreadLocalBusy = busyReading{busy: true, inflight: 2}
	where, slots := deal(r, repeatGoal(fitMechGoal, 2)...)
	if !slots[0].view.Local {
		t.Fatalf("subtask 0 landed on %s; with no remote that has room it must stay local", where[0])
	}
	if !strings.Contains(slots[0].reason, "local seat busy: 2 in flight; no remote with room") {
		t.Errorf("reason %q must say the seat was busy and why the slot stayed local", slots[0].reason)
	}
	if where[1] != "big-remote" {
		t.Errorf("subtask 1 landed on %s; the ordinary rotation still deals the remote its slot", where[1])
	}
}

// TestDealSpreadAlwaysKeepsTheLocalSlotWhenConfigured: agent_spread_local_slot
// "always" is the pre-0.113.20 deal — a busy reading changes nothing.
func TestDealSpreadAlwaysKeepsTheLocalSlotWhenConfigured(t *testing.T) {
	r := fitRunner(fitBigRemote, fitSmallRemote)
	r.cfg.AgentSpreadLocalSlot = "always"
	r.spreadLocalBusy = busyReading{busy: true, inflight: 5}
	where, _ := deal(r, repeatGoal(fitMechGoal, 4)...)
	if want := []string{"local-box", "small-remote", "big-remote", "local-box"}; !equalStrings(where, want) {
		t.Errorf("deal = %v, want %v (the unconditional local slot; mechanical goals take the smallest adequate seat first)", where, want)
	}
}

// TestDealSpreadIdleLocalSeatKeepsEverySlot is the control arm of the busy
// rule: an explicit idle reading deals exactly as every earlier version did.
func TestDealSpreadIdleLocalSeatKeepsEverySlot(t *testing.T) {
	r := fitRunner(fitBigRemote, fitSmallRemote)
	r.spreadLocalBusy = busyReading{busy: false, inflight: 0, note: "metrics"}
	where, slots := deal(r, repeatGoal(fitMechGoal, 4)...)
	if want := []string{"local-box", "small-remote", "big-remote", "local-box"}; !equalStrings(where, want) {
		t.Errorf("deal = %v, want %v", where, want)
	}
	for i, sl := range slots {
		if strings.Contains(sl.reason, "busy") {
			t.Errorf("subtask %d reason %q mentions busy on an idle seat", i, sl.reason)
		}
	}
}

// TestDealSpreadSheddableBusyLocalNeedsAnIdleRemoteSlot: a sheddable run under
// the busy rule takes only a remote with an IDLE slot (hasRoom's sheddable arm);
// a remote that merely is not saturated is not enough for it.
func TestDealSpreadSheddableBusyLocalNeedsAnIdleRemoteSlot(t *testing.T) {
	noIdle := fitBigRemote
	noIdle.SaturationKnown, noIdle.SaturationHigh, noIdle.IdleSlot = true, false, false
	idle := fitSmallRemote
	idle.SaturationKnown, idle.SaturationHigh, idle.IdleSlot = true, false, true
	r := fitRunner(noIdle, idle)
	r.priority = core.BandSheddable
	r.spreadLocalBusy = busyReading{busy: true, inflight: 1}
	where, _ := deal(r, repeatGoal(fitMechGoal, 2)...)
	for i, w := range where {
		if w != "small-remote" {
			t.Errorf("subtask %d landed on %s; a sheddable run may only take the remote with an idle slot", i, w)
		}
	}
}

// TestDealSpreadBusyLocalWithNoEligibleRemoteSaysSo: with no remote able to
// take the contract at all, the fallback reason says "no eligible remote", not
// "no remote with room" — an operator must not be sent chasing capacity.
func TestDealSpreadBusyLocalWithNoEligibleRemoteSaysSo(t *testing.T) {
	off := fitBigRemote
	off.AgentEnabled = false
	r := fitRunner(off)
	r.spreadLocalBusy = busyReading{busy: true, inflight: 1}
	_, slots := deal(r, fitMechGoal)
	if !slots[0].view.Local || !strings.Contains(slots[0].reason, "no eligible remote)") {
		t.Fatalf("reason %q (local=%v); want local with 'no eligible remote'", slots[0].reason, slots[0].view.Local)
	}
}
