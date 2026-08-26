package delegate

import (
	"encoding/json"
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
	fitSmallRemote = NodeView{NodeID: "small-remote", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 32768}
)

// TestPlaceSpreadFitScoresTheRemoteSlots is the behaviour change: the remote
// slots stop being a blind rotation. Round-robin hands subtask 1 the FIRST
// remote and subtask 2 the SECOND whatever their shape; fit scoring sends the
// mechanical one to the small seat and the reasoning one to the big seat.
func TestPlaceSpreadFitScoresTheRemoteSlots(t *testing.T) {
	// Roster order deliberately puts the BIG remote first, so round-robin and
	// fit scoring disagree on BOTH subtasks.
	r := fitRunner(fitBigRemote, fitSmallRemote)
	local := fitLocal()

	mech := fitSubtask("list every exported function name in these files", 100)
	p, dead := r.placeSpread(1, mech, local)
	if dead {
		t.Fatal("a healthy fleet must not read as dead")
	}
	if p.view.NodeID != "small-remote" {
		t.Errorf("mechanical subtask 1 landed on %s (%s), want small-remote", p.view.NodeID, p.reason)
	}

	reason := fitSubtask("explain how the retry path interacts with the queue cap", 100)
	p, _ = r.placeSpread(2, reason, local)
	if p.view.NodeID != "big-remote" {
		t.Errorf("reasoning subtask 2 landed on %s (%s), want big-remote", p.view.NodeID, p.reason)
	}
}

// TestPlaceSpreadKeepsSubtaskZeroLocal pins the guarantee the fit score is NOT
// allowed to break: subtask 0 lands on the local seat whatever its shape. A
// single-subtask spread is the riskiest case for a shape heuristic — getting it
// wrong sends the entire run off-box on one regex match.
func TestPlaceSpreadKeepsSubtaskZeroLocal(t *testing.T) {
	r := fitRunner(fitBigRemote, fitSmallRemote)
	local := fitLocal()
	for _, goal := range []string{
		"list every exported function name in these files",
		"explain how the retry path interacts with the queue cap",
		"what is the default queue cap",
	} {
		p, _ := r.placeSpread(0, fitSubtask(goal, 100), local)
		if !p.view.Local {
			t.Errorf("subtask 0 (%q) landed on %s — slot 0 must stay local", goal, p.view.NodeID)
		}
	}
}

// TestPlaceSpreadLeavesTheLocalRotationSlotAlone: the fit contest ranks seats by
// ADVERTISED ceiling, and the local seat advertises none in a delegator run, so
// it is never entered into the contest — it keeps every rotation slot it already
// had (i mod len == 0), not only subtask 0.
func TestPlaceSpreadLeavesTheLocalRotationSlotAlone(t *testing.T) {
	r := fitRunner(fitBigRemote, fitSmallRemote)
	local := fitLocal()
	mech := fitSubtask("list every exported function name in these files", 100)
	if p, _ := r.placeSpread(3, mech, local); !p.view.Local {
		t.Errorf("subtask 3 of a 3-seat spread landed on %s, want the local rotation slot", p.view.NodeID)
	}
}

// TestPlaceSpreadTiesKeepRotating: with equal-ceiling remotes the fit score
// cannot separate them, so the old rotation must still deal them one each —
// otherwise a fan-out of same-shaped contracts stacks on a single seat.
func TestPlaceSpreadTiesKeepRotating(t *testing.T) {
	a := NodeView{NodeID: "node-a", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 32768}
	b := NodeView{NodeID: "node-b", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 32768}
	r := fitRunner(a, b)
	local := fitLocal()
	st := fitSubtask("list every exported function name in these files", 100)

	want := []string{"local-box", "node-a", "node-b", "local-box"}
	for i, w := range want {
		p, _ := r.placeSpread(i, st, local)
		if p.view.NodeID != w {
			t.Errorf("subtask %d landed on %s, want %s", i, p.view.NodeID, w)
		}
	}
}

// TestPlaceSpreadSkipsASeatThatCannotHoldTheContract: per-subtask eligibility
// drops an over-size seat before the fit score ever sees it, so a mechanical
// contract cannot be dealt to the small seat merely because it is the cheapest.
func TestPlaceSpreadSkipsASeatThatCannotHoldTheContract(t *testing.T) {
	tiny := NodeView{NodeID: "tiny-remote", AgentEnabled: true, AgentResident: true, AgentCtxTokens: 8192}
	r := fitRunner(fitBigRemote, tiny)
	local := fitLocal()
	mech := fitSubtask("list every exported function name in these files", 20000)
	p, _ := r.placeSpread(1, mech, local)
	if p.view.NodeID != "big-remote" {
		t.Errorf("landed on %s (%s), want big-remote — the tiny seat cannot hold the contract", p.view.NodeID, p.reason)
	}
}
