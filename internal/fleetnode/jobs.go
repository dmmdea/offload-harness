// Package fleetnode implements the node side of fleet-dispatcher CONTRACT.md v2:
// health / ack-then-poll dispatch / job status, with measured VRAM footprints.
// This file is the job store — the ack-then-poll heart of the dispatch contract:
// a POST acks in milliseconds (202 accepted) and the render runs in a tracked
// goroutine; pollers read state until it turns terminal (done|error). Once acked,
// a job is OURS FOREVER (never re-dispatched), so shutdown marks survivors
// terminal error:"interrupted" rather than losing them — pollers always reach a
// terminal state.
package fleetnode

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// JobState is the contract's job lifecycle: accepted → running → done|error.
type JobState string

const (
	JobAccepted JobState = "accepted"
	JobRunning  JobState = "running"
	JobDone     JobState = "done"
	JobError    JobState = "error"
)

// JobView is a read-only copy of a job's externally visible state (the wire's
// `state` / `data` / `error` fields are shaped by the server layer).
type JobView struct {
	ID    string
	State JobState
	Data  json.RawMessage
	Error string
}

// job is the mutable store entry; guarded by Jobs.mu.
type job struct {
	state      JobState
	data       json.RawMessage
	err        string
	terminalAt time.Time // set when state turns done|error; drives ttl eviction
}

// Jobs is the concurrency-safe ack-then-poll job store. Terminal results are
// retained for ttl (contract: ≥ a few minutes; we run 1h) and swept by a janitor
// goroutine. DrainAndStop is the shutdown path.
type Jobs struct {
	mu       sync.RWMutex
	m        map[string]*job
	draining bool

	ttl time.Duration
	now func() time.Time // injectable clock (janitor tests)

	wg     sync.WaitGroup  // tracks in-flight run goroutines
	ctx    context.Context // handed to every run; cancelled at drain timeout
	cancel context.CancelFunc

	stopJanitor chan struct{}
	stopOnce    sync.Once
}

// NewJobs builds a store whose terminal entries live for ttl, with the janitor
// sweeping every 5 minutes (the spec's cadence).
func NewJobs(ttl time.Duration) *Jobs {
	return newJobs(ttl, time.Now, 5*time.Minute)
}

// newJobs is NewJobs with an injectable clock + janitor tick (unit-testable).
func newJobs(ttl time.Duration, now func() time.Time, janitorTick time.Duration) *Jobs {
	ctx, cancel := context.WithCancel(context.Background())
	j := &Jobs{
		m:           map[string]*job{},
		ttl:         ttl,
		now:         now,
		ctx:         ctx,
		cancel:      cancel,
		stopJanitor: make(chan struct{}),
	}
	go j.janitor(janitorTick)
	return j
}

// Accept registers id and starts run in a tracked goroutine, returning
// immediately (the ack). Idempotent: an id already present — in ANY state —
// returns false and never starts a second run (the contract's duplicate-POST
// rule). During drain it refuses all new work (false; the server maps that to
// 503 via Draining()).
func (j *Jobs) Accept(id string, run func(context.Context) (json.RawMessage, error)) (created bool) {
	j.mu.Lock()
	if j.draining {
		j.mu.Unlock()
		return false
	}
	if _, exists := j.m[id]; exists {
		j.mu.Unlock()
		return false
	}
	j.m[id] = &job{state: JobAccepted}
	j.wg.Add(1)
	j.mu.Unlock()

	go func() {
		defer j.wg.Done()
		j.markRunning(id)
		data, err := run(j.ctx)
		if err != nil {
			j.finish(id, nil, err.Error())
			return
		}
		j.finish(id, data, "")
	}()
	return true
}

// Get returns a copy of the job's visible state; false = unknown/evicted (404).
func (j *Jobs) Get(id string) (*JobView, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	jb, ok := j.m[id]
	if !ok {
		return nil, false
	}
	return &JobView{ID: id, State: jb.state, Data: jb.data, Error: jb.err}, true
}

// QueueDepth counts accepted+running jobs (the health field). Terminal entries
// are results awaiting pollers, not load.
func (j *Jobs) QueueDepth() int {
	j.mu.RLock()
	defer j.mu.RUnlock()
	n := 0
	for _, jb := range j.m {
		if jb.state == JobAccepted || jb.state == JobRunning {
			n++
		}
	}
	return n
}

// Draining reports whether DrainAndStop has begun (the server's 503 gate).
func (j *Jobs) Draining() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.draining
}

// killDeliveryGrace bounds DrainAndStop's SECOND wait: after the drain timeout
// cancels the run contexts, gpugen's killTree (spawned by CommandContext's
// Cancel) needs real time to land on the child tree. On Windows children
// survive parent death — if the process exits before the kill is delivered, an
// orphaned ComfyUI keeps pinning VRAM. 5s is ample for TerminateProcess over a
// process tree while keeping the total drain bound at timeout + 5s.
const killDeliveryGrace = 5 * time.Second

// DrainAndStop stops accepting, waits up to timeout for in-flight runs, marks
// every non-terminal survivor error:"interrupted" — the contract's shutdown
// obligation (once acked, a job is never re-dispatched, so pollers must still
// reach a terminal state) — then cancels their context and waits (bounded by
// killDeliveryGrace) for the released run goroutines to actually return, so
// the cancel-triggered kill of child process trees lands before the process
// exits. The mark happens BEFORE the cancel so a run released by the cancel
// can never race its late completion past the mark (finish never overwrites a
// terminal state). Total bound: timeout + killDeliveryGrace. Also stops the
// janitor. Safe to call once per store.
func (j *Jobs) DrainAndStop(timeout time.Duration) {
	j.mu.Lock()
	j.draining = true
	j.mu.Unlock()

	done := make(chan struct{})
	go func() { j.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}

	j.mu.Lock()
	for _, jb := range j.m {
		if jb.state == JobAccepted || jb.state == JobRunning {
			jb.state = JobError
			jb.err = "interrupted"
			jb.terminalAt = j.now()
		}
	}
	j.mu.Unlock()

	j.cancel() // release runs blocked on ctx so their goroutines can exit
	// Second bounded wait: give the released runs time to return — i.e. for
	// gpugen's killTree to finish killing the render's child tree — before we
	// let the process exit (done is already closed if everything drained in
	// the first wait, making this a no-op).
	select {
	case <-done:
	case <-time.After(killDeliveryGrace):
	}

	j.stopOnce.Do(func() { close(j.stopJanitor) })
}

// markRunning moves accepted → running. A no-op when drain already marked the
// job terminal (never resurrect a terminal state).
func (j *Jobs) markRunning(id string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if jb, ok := j.m[id]; ok && jb.state == JobAccepted {
		jb.state = JobRunning
	}
}

// finish records the run's outcome. Terminal states are write-once: a late
// completion after a drain-mark (or an eviction) is dropped.
func (j *Jobs) finish(id string, data json.RawMessage, errStr string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	jb, ok := j.m[id]
	if !ok || jb.state == JobDone || jb.state == JobError {
		return
	}
	if errStr != "" {
		jb.state = JobError
		jb.err = errStr
	} else {
		jb.state = JobDone
		jb.data = data
	}
	jb.terminalAt = j.now()
}

// janitor sweeps expired terminal entries until DrainAndStop closes stopJanitor.
func (j *Jobs) janitor(tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-j.stopJanitor:
			return
		case <-t.C:
			j.sweep()
		}
	}
}

// sweep evicts terminal entries older than ttl. In-flight jobs are never
// evicted, whatever their age (they are still ours to finish).
func (j *Jobs) sweep() {
	cutoff := j.now().Add(-j.ttl)
	j.mu.Lock()
	defer j.mu.Unlock()
	for id, jb := range j.m {
		if (jb.state == JobDone || jb.state == JobError) && jb.terminalAt.Before(cutoff) {
			delete(j.m, id)
		}
	}
}
