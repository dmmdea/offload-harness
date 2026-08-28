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
	if created := s.jobs.Accept(job.ID, run); !created {
		// Already known locally (a lease-expiry re-claim of our own job):
		// the original run's settle will ack; nothing to do.
		return job.ID, true
	}
	return job.ID, true
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
