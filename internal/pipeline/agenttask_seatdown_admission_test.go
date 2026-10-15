package pipeline

import (
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// The seat-down tests compress the liveness floor to 200 ms so a wedged seat is
// seen in a fraction of a second. The floor is also the least any phase may be
// silent, and with the admission gate off (a negative agent_admission_wait_sec,
// which makes the admission budget 0) it is the whole allowance of the admission
// phase: the run's own setup between the monitor's start and the loop's first
// request, which a starved process stretches to seconds (on a host pinned to two
// cores the first request went out more than 200 ms after the monitor started in
// 35 of 112 runs, the slowest 6 s later). A run whose setup took longer than the
// floor was stalled "in admission" before it had asked the seat for anything, and
// filed as a seat down with no step in flight:
//
//	stalled: no progress for 2s in admission (allowed 0s; 0 tok so far)
//	seat down: the seat's engine did no work for 0s while this request waited in admission (allowed 0s; ...)
//
// The setup is not the seat's silence and the compressed floor was never meant to
// bound it. The pipeline these tests run through gives admission its own budget.
func TestSeatDownPipelineAdmissionOutlastsTheCompressedFloor(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	p := seatDownPipeline(t, "http://127.0.0.1:1", 0)

	pol := LivenessPolicyFor(p.cfg, seatrate.Seat{}, admissionBudget(p.cfg.AgentAdmissionWaitSec))

	if pol.Floor != 200*time.Millisecond {
		t.Fatalf("floor = %s, want the 200 ms the seat-down tests compress it to", pol.Floor)
	}
	if got := pol.Allowance(agent.PhaseAdmission, 0); got < 10*time.Second {
		t.Fatalf("the admission phase may be silent for %s with the floor at %s, want at least 10 s: a run's setup before its first request must not race the compressed floor", got, pol.Floor)
	}
}
