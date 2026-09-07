// claimloop.go — the node side of the Option B pull queue (ADR 0030, dark by
// default): a background loop that PULLS eligible work from the config-elected
// holder, runs it through the SAME BuildRequest + jobs.Accept surface a pushed
// dispatch uses, and acks the holder with the result. Started by the serve
// verb only when BOTH fleet_queue_holder and fleet_queue_claim are set.
package fleetnode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/fleetqueue"
	"github.com/dmmdea/offload-harness/internal/netguard"
)

const (
	// claimIdlePoll paces the loop while the holder has nothing for us. Local
	// polls are cheap; 5s keeps queue latency humane without hammering.
	claimIdlePoll = 5 * time.Second
	// claimBusyPoll paces the loop while THIS node is at capacity — no point
	// asking for work we would refuse.
	claimBusyPoll = 15 * time.Second
	claimTimeout  = 10 * time.Second
)

// StartClaimLoop runs until ctx cancels. It never claims while this node's
// backlog is at its cap (the push path's own back-pressure rule), and it
// nacks jobs it cannot build rather than letting the lease expire — a loud
// bounded requeue beats a silent slow one.
func (s *Server) StartClaimLoop(ctx context.Context, cfg config.Config) {
	holder := cfg.FleetQueueHolder
	client := &http.Client{Timeout: claimTimeout, Transport: netguard.SafeTransport(nil)}
	nodeID := s.opts.NodeID
	log.Printf("fleetnode: claim loop up against %s (tasks %v)", holder, s.tasks)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		wait := claimIdlePoll
		if limit := cfg.FleetMaxQueueDepth; limit > 0 && s.jobs.QueueDepth() >= limit {
			wait = claimBusyPoll
		} else if job, ok := s.claimOne(ctx, client, holder, nodeID, cfg); ok {
			_ = job // claimed and admitted; loop immediately for more
			wait = 0
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
}

// claimOne asks the holder for one job and admits it locally. Returns ok=true
// when a job was claimed (whether or not admission succeeded — a nacked claim
// still consumed a poll).
func (s *Server) claimOne(ctx context.Context, client *http.Client, holder, nodeID string, cfg config.Config) (string, bool) {
	body, _ := json.Marshal(map[string]any{"node_id": nodeID, "task_types": s.tasks})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, holder+"/fleet/queue/claim", bytes.NewReader(body))
	if err != nil {
		return "", false
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.FleetAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.FleetAuthToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false // holder unreachable: the idle wait retries
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return "", false
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxDispatchBody))
	if resp.StatusCode != http.StatusOK {
		log.Printf("fleetnode: claim against %s: status %d", holder, resp.StatusCode)
		return "", false
	}
	var job fleetqueue.Job
	if json.Unmarshal(raw, &job) != nil || job.ID == "" {
		return "", false
	}

	breq, cleanup, berr := BuildRequest(ctx, cfg, s.opts.LoopbackListener, job.TaskType, job.Payload)
	if berr != nil {
		cleanup()
		s.settle(ctx, client, holder, cfg, "nack", job.ID, nodeID, nil, "build: "+berr.Error())
		return job.ID, true
	}
	run := func(rctx context.Context) (json.RawMessage, error) {
		defer cleanup()
		res := s.runner.Run(rctx, breq)
		if !res.OK {
			s.settle(ctx, client, holder, cfg, "ack", job.ID, nodeID, nil, res.Reason)
			return nil, fmt.Errorf("%s", res.Reason)
		}
		s.settle(ctx, client, holder, cfg, "ack", job.ID, nodeID, res.Data, "")
		return res.Data, nil
	}
	// The SAME AcceptSpec the push path builds (server.go's handleDispatch),
	// because this is the same surface: before 0.113.27 this called bare
	// Accept, i.e. AcceptSpec{}, and every field it dropped had a consequence.
	//
	//   Agent     — handleJob gates an agent job's state AND RESULT behind the
	//               bearer token by reading this marker. Unset, a PULLED agent
	//               contract's result was readable without the token while the
	//               identical contract arriving by dispatch was gated, and
	//               handleJobs' agent-row error redaction was skipped too.
	//   Uncapped  — a pulled render consumed a concurrency slot the cap does
	//               not exist to protect.
	//   OnDropped — drain's never-started arm could not clean up, stranding
	//               the materialized job dir.
	//   Task/Model— the /fleet/jobs feed showed a pulled job with no task and
	//               no model.
	//
	// Band and Tenant are NOT set: fleetqueue.Job does not carry them, so
	// there is nothing honest to fill them with. A pulled job therefore rides
	// the default band, which is what it did before this change.
	spec := s.claimSpec(job.TaskType, cleanup)
	if created := s.jobs.Admit(job.ID, spec, run); !created {
		// Already known locally (a lease-expiry re-claim of our own job):
		// the original run's settle will ack; nothing to RUN.
		//
		// But this build's materialization is ours and nothing else will free
		// it: OnDropped is deliberately not invoked when Admit REFUSES (see
		// its doc), and the original job's own cleanup closes over the
		// original dir, not this one. Left alone it strands a directory under
		// pipeline-jobs/ until the next fleet-serve start sweeps it
		// (0.113.27; the run closure that would have deferred cleanup never
		// executes on this path).
		cleanup()
		return job.ID, true
	}
	return job.ID, true
}

// claimSpec is the AcceptSpec a PULLED job is admitted with. It exists as its
// own function so the pull path's spec is testable without a live holder — the
// fields it sets are decided at admission and are not observable afterwards on
// a job that was admitted wrong.
//
// Band and Tenant are deliberately absent: fleetqueue.Job does not carry them,
// so there is nothing honest to fill them with, and a pulled job rides the
// default band exactly as it did before 0.113.27.
func (s *Server) claimSpec(taskType string, cleanup func()) AcceptSpec {
	spec := AcceptSpec{
		// Agent gates the bearer check on this job's state AND RESULT
		// (handleJob), and handleJobs' agent-row error redaction. Unset, a
		// pulled agent contract's result was readable without the token while
		// the identical contract arriving by dispatch was gated.
		Agent: taskType == string(core.TaskAgentRun),
		// Uncapped keeps a pulled render off the concurrency cap that exists
		// to protect the shared text endpoint it never touches.
		Uncapped: !s.concurrencyCapped(taskType),
		// OnDropped is the only cleanup a job that is admitted and then
		// drained without ever starting will get.
		OnDropped: cleanup,
		Task:      taskType,
	}
	if spec.Agent {
		spec.Model = s.agentSeat
	}
	return spec
}

// settle acks or nacks the holder, best-effort with one retry: a lost settle
// self-heals via the lease (expiry requeues; the duplicate re-ack is ignored).
func (s *Server) settle(ctx context.Context, client *http.Client, holder string, cfg config.Config, verb, jobID, nodeID string, result json.RawMessage, jobErr string) {
	payload := map[string]any{"job_id": jobID, "node_id": nodeID}
	if verb == "ack" {
		payload["result"], payload["error"] = result, jobErr
	} else {
		payload["reason"] = jobErr
	}
	body, _ := json.Marshal(payload)
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, holder+"/fleet/queue/"+verb, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if cfg.FleetAuthToken != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.FleetAuthToken)
		}
		resp, derr := client.Do(req)
		if derr == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return
			}
		}
	}
	log.Printf("fleetnode: %s of %s against %s failed twice — the lease will requeue it", verb, jobID, holder)
}
