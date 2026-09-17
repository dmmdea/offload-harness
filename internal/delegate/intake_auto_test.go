package delegate

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestPrepareContractStampsTimeoutAutoOnlyWhenTheCallerNamedNoWall (register
// D-03): unset → the wire default plus the marker; set → the number, no marker;
// a spec that says timeout_auto next to its own timeout_sec is a contradiction
// the explicit number wins.
func TestPrepareContractStampsTimeoutAutoOnlyWhenTheCallerNamedNoWall(t *testing.T) {
	unset, err := PrepareContractWithCap(SubtaskSpec{AgentContract: core.AgentContract{Goal: "g"}}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !unset.TimeoutAuto || unset.TimeoutSec != core.AgentTimeoutSecDefault {
		t.Fatalf("unset: auto=%v timeout=%d, want true/%d", unset.TimeoutAuto, unset.TimeoutSec, core.AgentTimeoutSecDefault)
	}
	set, err := PrepareContractWithCap(SubtaskSpec{AgentContract: core.AgentContract{Goal: "g", TimeoutSec: 120}}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if set.TimeoutAuto || set.TimeoutSec != 120 {
		t.Fatalf("set: auto=%v timeout=%d, want false/120", set.TimeoutAuto, set.TimeoutSec)
	}
	lied, err := PrepareContractWithCap(SubtaskSpec{AgentContract: core.AgentContract{Goal: "g", TimeoutSec: 120, TimeoutAuto: true}}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if lied.TimeoutAuto {
		t.Fatalf("a spec naming timeout_sec AND timeout_auto must keep the explicit number: %+v", lied)
	}
}

// TestExecutionBudgetHoldsTheCapOpenForAnAutoContract: the delegator's clock
// for a timeout_auto contract is the wire cap — the node's wall lands anywhere
// inside it — and an explicit or defaulted contract is unchanged.
func TestExecutionBudgetHoldsTheCapOpenForAnAutoContract(t *testing.T) {
	cases := []struct {
		name string
		c    core.AgentContract
		want int
	}{
		{"auto → the cap", core.AgentContract{TimeoutSec: core.AgentTimeoutSecDefault, TimeoutAuto: true}, core.AgentTimeoutSecCap},
		{"explicit → itself", core.AgentContract{TimeoutSec: 120}, 120},
		{"unset (older caller) → the default", core.AgentContract{}, core.AgentTimeoutSecDefault},
	}
	for _, c := range cases {
		if got := executionBudgetSec(c.c); got != c.want {
			t.Errorf("%s: executionBudgetSec = %d, want %d", c.name, got, c.want)
		}
	}
}
