package delegate

import (
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// AutoPollBoundForTest exposes the auto poll bound to the EXTERNAL test
// package (delegate_test). It exists for exactly one test: the anti-drift
// check that the delegator's bound equals the wall the NODE would size for the
// same contract on the same seat (autopoll_drift_test.go). That test has to
// import internal/pipeline, which imports this package, so it can only live in
// an external test package — and an external test package cannot reach an
// unexported function. Test-only: this file compiles into no binary.
func AutoPollBoundForTest(view NodeView, c core.AgentContract) (time.Duration, string) {
	return autoPollBound(view, c)
}

// PollSecondForTest is the unit an integer wall is converted to wall clock
// with (one real second unless a test compressed it) — the external test
// converts the node's seconds into the same units the bound is expressed in.
func PollSecondForTest() time.Duration { return pollSecond }
