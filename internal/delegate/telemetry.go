package delegate

import (
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// fleetJobIDOf is the id the fleet node knows a job by: the id the delegator
// dispatched it under, for an attempt whose dispatch the node ACKED. A job that
// never left this box has none, and neither has one whose dispatch was refused
// (or never reached the node): no node holds that id, and a row that named one
// would read, to a join from the delegator's side, as a job the node lost. The
// ack is what intentRecorded records, so the id is on a finished row exactly when
// a started marker was written for it. It equals the row's own job_id today; it is
// a column of its own so the join to a node's ledger does not depend on that
// staying true.
func fleetJobIDOf(pr PlacedResult) string {
	if pr.ranBase == "" || !pr.intentRecorded {
		return ""
	}
	return pr.JobID
}

// recordStarted writes the dispatch marker: a ledger row (Phase "started") that
// says a job was handed to a seat, written the moment it is — after a node's ack,
// or as a local run begins — and not when the job ends. Until now a row existed
// only once its job was over, so a hang, and a ghost that outlived its delegator,
// were invisible exactly while they mattered. The finished row, written when the
// job ends, carries the same job id; the marker is never a job (ledger.JobRows
// and every counter skip it) and only says a job BEGAN.
//
// Best effort, like every telemetry write: a marker that cannot be written costs
// visibility and never fails the work. It is not counted in the ledger-loss tally
// either, which reports rows the run OWES and a marker is an addition.
func (r *runner) recordStarted(contract core.AgentContract, jobID, fleetJobID, node, seat, placement string) {
	if r.led == nil {
		return
	}
	_ = r.led.Record(ledger.Entry{
		Task:       "agent_delegate",
		Phase:      ledger.PhaseStarted,
		ReasonCode: ledger.ReasonStarted,
		Door:       doorOf(contract),
		JobID:      jobID,
		FleetJobID: fleetJobID,
		Route:      r.route,
		Placement:  placement,
		ModelTier:  node + ":" + seat,
	})
}
