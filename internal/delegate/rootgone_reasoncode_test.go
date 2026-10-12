package delegate

// Register C-79 in the ledger and the delegation log. A lost-root defer is filed under
// the closed reason code `infrastructure`: the class the node gives it is what the
// reason code switch reads, and the one place prose decides a code is the seat-down
// HINT (seatUnreachable), whose words are dial failures. The operating system's own
// words for a root that cannot be opened are in the reason ("no such file or
// directory", "The system cannot find the path specified."), so this pins that none
// of them reads as a dead seat.

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

func TestARootGoneDeferIsFiledAsInfrastructureInTheLedger(t *testing.T) {
	for _, cause := range []string{
		"no such file or directory",
		"not a directory",
		"permission denied",
		"The system cannot find the path specified.",
		"The system cannot find the file specified.",
		"Access is denied.",
	} {
		t.Run(cause, func(t *testing.T) {
			w := rootGoneWire("node-a")
			// The wording pipeline.rootGoneReason files: the constant, the root, the cause, the tail.
			w.Reason = core.RootGoneReason + "/jobs/agent-local-1/context: " + cause +
				" - the run's directory could not be opened, so the run was stopped, or its result withheld, instead of going on without it; nothing about the contract was learned"
			if got := reasonCodeFor(PlacedResult{Result: w}); got != ledger.ReasonInfrastructure {
				t.Fatalf("reason_code = %q, want %q: a lost root is the box's, and no operating system cause may read as a dead seat", got, ledger.ReasonInfrastructure)
			}
		})
	}
}
