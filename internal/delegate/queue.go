// queue.go — the delegator side of the Option B pull queue (ADR 0030, dark by
// default): route:"queue" SUBMITS every subtask to the config-elected holder
// and polls the holder for results; which NODE runs each job is the claim
// loops' decision, not the delegator's. Durability is the holder's (bbolt),
// so this route deliberately does not write the push path's intent ledger —
// a delegator that dies mid-poll re-polls the holder, which still has both
// the job and its result.
package delegate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

const queuePollEvery = 3 * time.Second

// runQueued places every subtask through the holder. Contracts must carry an
// OutputSchema (the same remote-placement rule as push: schema-less work
// cannot leave the box) — enforced here so a mixed batch fails loudly before
// anything is submitted.
func runQueued(ctx context.Context, cfg config.Config, subtasks []core.AgentContract, rescue RescueFunc) ([]PlacedResult, Summary, error) {
	holder := strings.TrimRight(strings.TrimSpace(cfg.FleetQueueHolder), "/")
	if holder == "" {
		return nil, Summary{}, fmt.Errorf("delegate: route \"queue\" needs fleet_queue_holder configured (ADR 0030)")
	}
	for i, c := range subtasks {
		if len(c.OutputSchema) == 0 {
			return nil, Summary{}, fmt.Errorf("delegate: subtask %d has no output_schema — queue placement, like remote, requires one", i)
		}
	}
	// SF-40: the holder is the one node this box submits to, so it is checked here, where the route bypasses
	// the placement gate. Which node later CLAIMS a queued job is the claimants' own configuration and is not
	// something this box can see or fence.
	if cfg.RemoteIsPublic(holder) {
		for i, c := range subtasks {
			if private, what := privateWork(c); private {
				return nil, Summary{}, fmt.Errorf("delegate: route \"queue\": the holder (fleet_queue_holder) is declared public in delegate_remote_classes and takes public-data work only, and subtask %d is not (%s); route it local, or point fleet_queue_holder at a private-ok node", i, what)
			}
		}
	}

	results := make([]PlacedResult, len(subtasks))
	// Submit ALL before polling ANY: the queue's value is nodes pulling
	// concurrently; serial submit-poll-submit would serialize the fleet.
	for i, contract := range subtasks {
		jobID := mintJobID()
		results[i].JobID = jobID
		results[i].PlacementReason = "route=queue → holder " + holder
		payload, merr := json.Marshal(contract)
		if merr != nil {
			results[i].Err = "marshaling contract: " + merr.Error()
			continue
		}
		timeoutSec := executionBudgetSec(contract) // the cap for a timeout_auto contract (D-03): the claimer sizes the wall
		if err := queueSubmit(ctx, cfg, holder, jobID, string(core.TaskAgentRun), payload, timeoutSec); err != nil {
			results[i].Err = "queue submit: " + err.Error()
		}
	}

	for i := range subtasks {
		if results[i].Err != "" {
			continue
		}
		queuePoll(ctx, cfg, holder, subtasks[i], &results[i], rescue)
	}

	return results, queueSummary(results), nil
}

// takeQueueAnswer puts the answer a claimant filed for a queued job on pr: its result, the node and seat that gave it, and
// the mark placedWhere reads, so the row says ran_on "node" (D-32). The queue lane records no dispatch of its own (the holder
// places a job and a claim loop takes it), so the only evidence of where it ran is a finished answer, and the dial base of
// this attempt is the holder's: the one URL this side asked. Only a finished answer sets it; a job still queued, a poll that
// ran out and a call deadline's cut leave the row unplaced, so they publish no ran_on. Both arms that read a finished answer
// (queuePoll and the deadline's last look, lookAnswer) come through here.
//
// The base is the holder's, which is not where the job ran: viaQueue says so, and the rescue of a schema miss reads it
// (rescueSchemaMiss), so it dials no origin at the holder with the claimant's seat name.
//
// Node here is the id the claimant stamped on its answer: the delegator has no health view of the claimant (the holder's
// poll returns the job's state and data, not who claimed it), so it cannot name the node by the id health publishes. A
// claimant on this release stamps the id it claims under, so the two agree; one on an older build stamps its OS hostname
// where it was launched with another --node-id, and the row says that until the node is redeployed.
func (pr *PlacedResult) takeQueueAnswer(wire core.AgentWireResult, holder string) {
	pr.Result = wire
	pr.Node = wire.NodeID
	pr.Seat = wire.Seat
	pr.ranBase = holder
	pr.viaQueue = true
}

// queueSummary counts the queue lane's outcomes into the four buckets. runQueued builds
// it once the polls end; the call deadline (calldeadline.go) builds it again after its
// final look at the holder has moved some outcomes.
func queueSummary(results []PlacedResult) Summary {
	var sum Summary
	for i := range results {
		pr := results[i]
		switch {
		case pr.Err != "":
			sum.Failed++
		case pr.Result.Deferred:
			sum.Deferred++
		case len(pr.AcceptanceFailures) > 0:
			sum.FailedVerification++
		default:
			sum.Succeeded++
		}
	}
	return sum
}

func queueSubmit(ctx context.Context, cfg config.Config, holder, jobID, taskType string, payload json.RawMessage, timeoutSec int) error {
	body, _ := json.Marshal(map[string]any{
		"job_id": jobID, "task_type": taskType, "payload": payload, "timeout_sec": timeoutSec,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, holder+"/fleet/queue/submit", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.FleetAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.FleetAuthToken)
	}
	// Who asked, and whether the claiming node must card the job because this box will not: they ride
	// the queued job to its claimant (D7/D9/D11).
	pairworkloads.WireHeadersFor(cfg, req.Header)
	resp, err := fleetClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("holder answered %d", resp.StatusCode)
	}
	return nil
}

// queuePoll drives one subtask to a terminal outcome. The budget mirrors the
// push path's shape: execution timeout + grace, with queue-wait NOT charged —
// the holder reports "accepted" while unclaimed, and that time extends the
// deadline exactly like the push node's backlog credit (bounded the same way).
func queuePoll(ctx context.Context, cfg config.Config, holder string, contract core.AgentContract, pr *PlacedResult, rescue RescueFunc) {
	timeoutSec := executionBudgetSec(contract) // the cap for a timeout_auto contract (D-03)
	budget := time.Duration(timeoutSec)*time.Second + pollGrace
	start := time.Now()
	deadline := start.Add(budget)
	queuedWaitBudget := budget
	if queuedWaitBudget > maxQueuedWait {
		queuedWaitBudget = maxQueuedWait
	}
	var queuedCredit time.Duration
	var lastQueuedAt time.Time
	for {
		if err := ctx.Err(); err != nil {
			pr.Err = "canceled: " + err.Error()
			return
		}
		if time.Now().After(deadline) {
			pr.Result = core.AgentWireResult{
				SchemaVersion: core.AgentWireSchemaVersion, Deferred: true,
				DeferClass: core.DeferClassBudget,
				Reason:     fmt.Sprintf("queue poll deadline after %s%s: no claimant reached a terminal state (job stays on the holder; re-poll or raise the budget)", budget, queuedNote(queuedCredit)),
			}
			return
		}
		// The queue lane keeps the CAP for a timeout_auto contract: the job is
		// pulled by a claimant the delegator never chose, so there is no health
		// view to size a bound from (register D-116 bounds the PUSH path).
		p, perr := pollJobOnceAt(ctx, cfg, holder+"/fleet/queue/jobs/"+pr.JobID)
		state, data, jobErr, status := p.State, p.Data, p.JobErr, p.Status
		prevQueuedAt := lastQueuedAt
		lastQueuedAt = time.Time{}
		switch {
		case perr != nil:
			// transient; the deadline decides
		case status == http.StatusNotFound:
			pr.Err = "holder denies the job (submitted then vanished — holder store reset?)"
			return
		case status == http.StatusUnauthorized:
			pr.Err = "queue poll: 401 unauthorized (fleet_auth_token mismatch)"
			return
		case status == http.StatusOK && state == "done":
			var wire core.AgentWireResult
			if uerr := json.Unmarshal(data, &wire); uerr != nil {
				pr.Err = "job done but data is not an AgentWireResult: " + uerr.Error()
				return
			}
			pr.takeQueueAnswer(wire, holder)
			// A finished answer whose structured re-pack failed is re-packed here, as on
			// every other route (rescue.go); a rescue that cannot leaves the defer.
			*pr = rescueSchemaMiss(ctx, rescue, contract, *pr, start, nil)
			if !pr.Result.Deferred {
				pr.gradeAcceptance(contract, pr.Result)
			}
			return
		case status == http.StatusOK && state == "error":
			pr.Err = "queue job failed: " + jobErr
			return
		case status == http.StatusOK && state == "accepted":
			// Unclaimed: credit the wait like the push path's backlog credit.
			now := time.Now()
			if !prevQueuedAt.IsZero() {
				queuedCredit += now.Sub(prevQueuedAt)
				if queuedCredit > queuedWaitBudget {
					queuedCredit = queuedWaitBudget
				}
				deadline = start.Add(budget + queuedCredit)
			}
			lastQueuedAt = now
			if queuedCredit >= queuedWaitBudget {
				pr.Err = fmt.Sprintf("queue wait deadline after %s: no node claimed the job (job stays on the holder)", budget+queuedCredit)
				return
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(queuePollEvery):
		}
	}
}
