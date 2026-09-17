package placement

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

// TestRequestForContractSizesAnAutoContractAgainstTheCap (register D-03): the
// long-seat prefill check defers a contract whose prefill outruns its budget;
// a timeout_auto contract's real wall is decided by the node inside the cap, so
// placement must size it against the cap, never the 300 s the wire carries.
func TestRequestForContractSizesAnAutoContractAgainstTheCap(t *testing.T) {
	auto := RequestForContract(core.AgentContract{Goal: "g", TimeoutSec: core.AgentTimeoutSecDefault, TimeoutAuto: true}, 1000, 0)
	if auto.BudgetSec != core.AgentTimeoutSecCap {
		t.Fatalf("auto BudgetSec = %d, want the cap %d", auto.BudgetSec, core.AgentTimeoutSecCap)
	}
	explicit := RequestForContract(core.AgentContract{Goal: "g", TimeoutSec: 120}, 1000, 0)
	if explicit.BudgetSec != 120 {
		t.Fatalf("explicit BudgetSec = %d, want 120", explicit.BudgetSec)
	}
}
