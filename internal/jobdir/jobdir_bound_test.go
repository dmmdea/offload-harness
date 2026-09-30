package jobdir_test

import (
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/jobdir"
)

// MaxRunLifetime's doc comment shows the arithmetic that keeps the startup
// sweep from reclaiming a run that is still going: admission, the seat-cap
// wait (as long as the run's wall) and the liveness ceiling, added up. This
// pins that arithmetic to the constants it cites, so raising one of them past
// what 24 h can cover fails here instead of turning the sweep back into a
// deleter of live runs. The bound is twice the sum, the margin the doc states
// (5.5x today) rounded down.
func TestMaxRunLifetimeOutlastsTheLongestLocalRun(t *testing.T) {
	longest := time.Duration(core.AgentAdmissionSecDefault+core.AgentTimeoutSecCap+core.AgentCeilingSecCap) * time.Second
	if jobdir.MaxRunLifetime < 2*longest {
		t.Fatalf("MaxRunLifetime = %v, but the longest local run is admission %d s + wall %d s + ceiling %d s = %v: "+
			"a sweep would reclaim a live run by age. Raise MaxRunLifetime (and its doc comment) or lower the cap you changed",
			jobdir.MaxRunLifetime, core.AgentAdmissionSecDefault, core.AgentTimeoutSecCap, core.AgentCeilingSecCap, longest)
	}
}
