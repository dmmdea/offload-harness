package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// jobs_running counts every job on the node and max_concurrent_jobs bounds only the jobs that count against
// it, so a delegator deciding whether a job still `accepted` is waiting for a worker (stayInLine, register C16)
// needs the count that is comparable to the ceiling. A render running next to a free agent worker must
// publish jobs_running 1 and jobs_running_capped 0; an agent job running must publish both as 1. The key is
// an agent-lane fact and a pointer, so a published 0 is not an absent key.
func TestHealthPublishesTheRunningJobsThatCountAgainstTheWorkers(t *testing.T) {
	cfg := agentCfg()
	cfg.FleetMaxConcurrentJobs = 2
	cfg.StateDir = t.TempDir()
	s, jobs := newTestServer(t, cfg, &fakeRunner{}, nil)

	read := func() (running int, capped *int) {
		rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
		var h struct {
			JobsRunning       int  `json:"jobs_running"`
			JobsRunningCapped *int `json:"jobs_running_capped"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
			t.Fatal(err)
		}
		return h.JobsRunning, h.JobsRunningCapped
	}
	if _, capped := read(); capped == nil || *capped != 0 {
		t.Fatalf("an idle node published jobs_running_capped = %v, want a published 0", capped)
	}

	block := make(chan struct{})
	defer close(block)
	run := func(started chan struct{}) func(context.Context) (json.RawMessage, error) {
		return func(ctx context.Context) (json.RawMessage, error) {
			close(started)
			<-block
			return nil, nil
		}
	}
	render, agent := make(chan struct{}), make(chan struct{})
	if !jobs.Admit("render-1", AcceptSpec{Uncapped: true, Task: "image-gen"}, run(render)) {
		t.Fatal("render not admitted")
	}
	select {
	case <-render:
	case <-time.After(5 * time.Second):
		t.Fatal("the render never started")
	}
	if running, capped := read(); running != 1 || capped == nil || *capped != 0 {
		t.Fatalf("with a render running: jobs_running %d, jobs_running_capped %v, want 1 and a published 0", running, capped)
	}
	if !jobs.Admit("agent-1", AcceptSpec{Agent: true}, run(agent)) {
		t.Fatal("agent job not admitted")
	}
	select {
	case <-agent:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent job never started")
	}
	if running, capped := read(); running != 2 || capped == nil || *capped != 1 {
		t.Fatalf("with a render and an agent job running: jobs_running %d, jobs_running_capped %v, want 2 and 1", running, capped)
	}

	// Lane off: the key is not published at all.
	plain, _ := newTestServer(t, imageCfg(), &fakeRunner{}, nil)
	if _, capped := func() (int, *int) {
		rec := do(t, plain, http.MethodGet, "/fleet/health", "", nil)
		var h struct {
			JobsRunning       int  `json:"jobs_running"`
			JobsRunningCapped *int `json:"jobs_running_capped"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &h)
		return h.JobsRunning, h.JobsRunningCapped
	}(); capped != nil {
		t.Fatalf("a node with the agent lane off published jobs_running_capped = %d", *capped)
	}
}
