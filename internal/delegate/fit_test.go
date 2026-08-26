package delegate

import (
	"encoding/json"
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
	if scoreFit(reason, big) <= scoreFit(reason, small) {
		t.Errorf("reasoning must prefer the roomier seat: big=%d small=%d", scoreFit(reason, big), scoreFit(reason, small))
	}
	mech := fitSubtask("list every exported function name in these files", 100)
	if scoreFit(mech, small) <= scoreFit(mech, big) {
		t.Errorf("mechanical must prefer the smaller adequate seat: small=%d big=%d", scoreFit(mech, small), scoreFit(mech, big))
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
		if scoreFit(st, tooSmall) >= scoreFit(st, adequateSeat) {
			t.Errorf("%q: an inadequate seat outranked an adequate one", goal)
		}
		if scoreFit(st, unadvertised) >= scoreFit(st, adequateSeat) {
			t.Errorf("%q: an UNADVERTISED ceiling outranked an advertised, adequate one", goal)
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
	qube := NodeView{NodeID: "qube", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 131072}
	aorus := NodeView{NodeID: "aorus", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 32768}
	lenovo := NodeView{NodeID: "lenovo", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 32768}

	for _, tc := range []struct{ name, goal string }{
		{"mechanical", fitMechGoal},
		{"reasoning", fitReasonGoal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := fitRunner(qube, aorus, lenovo)
			where, _ := deal(r, repeatGoal(tc.goal, 8)...)

			counts := map[string]int{}
			for _, id := range where {
				counts[id]++
			}
			for _, id := range []string{"local-box", "qube", "aorus", "lenovo"} {
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
