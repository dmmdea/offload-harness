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
)

const queuePollEvery = 3 * time.Second

// runQueued places every subtask through the holder. Contracts must carry an
// OutputSchema (the same remote-placement rule as push: schema-less work
// cannot leave the box) — enforced here so a mixed batch fails loudly before
// anything is submitted.
func runQueued(ctx context.Context, cfg config.Config, subtasks []core.AgentContract) ([]PlacedResult, Summary, error) {
	holder := strings.TrimRight(strings.TrimSpace(cfg.FleetQueueHolder), "/")
	if holder == "" {
		return nil, Summary{}, fmt.Errorf("delegate: route \"queue\" needs fleet_queue_holder configured (ADR 0030)")
	}
	for i, c := range subtasks {
		if len(c.OutputSchema) == 0 {
			return nil, Summary{}, fmt.Errorf("delegate: subtask %d has no output_schema — queue placement, like remote, requires one", i)
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
		timeoutSec := contract.TimeoutSec
		if timeoutSec <= 0 {
			timeoutSec = core.AgentTimeoutSecDefault
		}
		if err := queueSubmit(ctx, cfg, holder, jobID, string(core.TaskAgentRun), payload, timeoutSec); err != nil {
			results[i].Err = "queue submit: " + err.Error()
		}
	}

	for i := range subtasks {
		if results[i].Err != "" {
			continue
		}
		queuePoll(ctx, cfg, holder, subtasks[i], &results[i])
	}

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
	return results, sum, nil
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
func queuePoll(ctx context.Context, cfg config.Config, holder string, contract core.AgentContract, pr *PlacedResult) {
	timeoutSec := contract.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = core.AgentTimeoutSecDefault
	}
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
		state, data, jobErr, status, perr := pollJobOnceAt(ctx, cfg, holder+"/fleet/queue/jobs/"+pr.JobID)
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
			pr.Result = wire
			pr.Node = wire.NodeID
			pr.Seat = wire.Seat
			if !wire.Deferred {
				pr.AcceptanceFailures = EvalAcceptance(contract, wire)
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
