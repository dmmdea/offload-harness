package delegate

import (
	"context"
	"fmt"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
)

// The queue deadline asks before it withdraws (ADR 0063, amendment 2026-10-10, register C16).
//
// A job accepted into a node's backlog is given clamp(1.5 x the node's ETA + 30 s, 60 s, patience) to
// start (queueBudgetFor). The ETA is the median of the node's recent walls, so a one-worker node that is
// mid-way through a job that runs three times its median publishes a wait that is a fraction of the one
// the job faces, and the budget could run out with the job next in line. The delegator then withdrew it
// (ADR 0064) and the withdrawal was filed as a capacity refusal; with no other node to take the job it
// went back to the capacity wait, which does not exclude the node it came from and asks it again after
// its cooldown, putting the job at the BACK of the same line: the wait already spent was thrown away to
// land where it started. That is a refusal of work that was making progress, and the operator's rule is
// to queue and never refuse.
//
// The budget is therefore where the delegator asks whether there is a better place, and it leaves only if
// there is one or the node gives no reason to wait:
//
//   - the node's workers are all running (a published ceiling, that many jobs running): the job is waiting
//     for a worker, which is a queue and not a wedge. A node that reports a free worker and still has not
//     started the job is not working, and the job is withdrawn at the budget as it always was; so is a
//     node whose health cannot be read ("unknown is never a yes");
//   - no other node, and not the local seat, has room for it now (betterPlaceThan): a withdrawal could only
//     move the job to a place that is no better.
//
// Then the job stays, and is asked about again a floor (60 s) later, until the patience the contract was
// given (the poll budget) is spent. The patience, not the ETA, is what ends the wait for a start.

// queueStayStep is how much longer a job that stays in line is given before it is asked about again:
// the queue budget's own floor (chosen, as the floor is).
func queueStayStep() time.Duration { return time.Duration(queueBudgetFloorSec) * pollSecond }

// stayInLine reports whether a job still queued on base at its queue budget should stay there, and the
// words for it.
func (r *runner) stayInLine(ctx context.Context, base string, contract core.AgentContract) (string, bool) {
	rctx, cancel := context.WithTimeout(ctx, pollRequestTimeout)
	fresh, err := FetchNodeView(rctx, base, r.cfg.FleetAuthToken)
	cancel()
	if err != nil {
		return "", false
	}
	if fresh.MaxConcurrentJobs <= 0 || fresh.JobsRunning < fresh.MaxConcurrentJobs {
		return "", false
	}
	if _, better := r.betterPlaceThan(ctx, base, contract); better {
		return "", false
	}
	return fmt.Sprintf("the node's workers were all busy (%d of %d running) and no other place had room", fresh.JobsRunning, fresh.MaxConcurrentJobs), true
}

// betterPlaceThan reports whether the contract could go somewhere other than base right now, and where: a
// remote node that has room for a new dispatch and can start it inside the contract's patience
// (withRoom, the test a re-placement applies), or the local seat when the route allows it, no lease
// reserves or fences it and its run-cap line has a free slot ahead of a newcomer (replacementNode's last
// resort). It changes nothing: it is the question the queue deadline asks before it withdraws a job.
func (r *runner) betterPlaceThan(ctx context.Context, base string, contract core.AgentContract) (string, bool) {
	if r.route != "local" {
		views, bases, _, _ := r.fetchViewsDetailed(ctx)
		views, bases = untried(views, bases, map[string]bool{base: true})
		st := Subtask{Contract: contract, EstTokens: EstimateTokens(contract)}
		if cands, _, _ := r.withRoom(st, views, bases); len(cands) > 0 {
			return laneID(cands[0]) + " has room", true
		}
	}
	if r.route == "remote" || r.local == nil {
		return "", false
	}
	local := r.localView()
	if !r.localServesLayer(local, contract) || Reserved(r.localLease(contract)) {
		return "", false
	}
	if _, _, fenced := r.fencedLocal(contract); fenced {
		return "", false
	}
	if free, _ := r.localSlotAhead(); !free {
		return "", false
	}
	return "the local seat has a free slot", true
}

// keptInLineNote says, on a queue deadline, that the job was left in line past its derived budget and why,
// so the end of a long wait is not read as the budget the node's ETA gave.
func keptInLineNote(times int, why string) string {
	if times == 0 {
		return ""
	}
	return fmt.Sprintf("; kept in line %d time(s) past the derived budget, up to the contract's patience: %s", times, why)
}
