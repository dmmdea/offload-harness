// Package fleetnode implements the node side of fleet-dispatcher CONTRACT.md v2:
// health / ack-then-poll dispatch / job status, with measured VRAM footprints.
// This file is the job store — the ack-then-poll heart of the dispatch contract:
// a POST acks in milliseconds (202 accepted) and the render runs in a tracked
// goroutine; pollers read state until it turns terminal (done|error). Once acked,
// a job is OURS FOREVER (never re-dispatched), so shutdown marks survivors
// terminal rather than losing them — pollers always reach a terminal state.
//
// ADMIT-THEN-SCHEDULE (0.100.0). Accept used to BE start: it wrote the record
// and immediately launched the run, so `accepted` lasted microseconds, nothing
// ever waited, and the node's one cap (fleet_max_queue_depth) bounded
// SIMULTANEOUS EXECUTION while being named, documented and read as a backlog.
// A node at "depth 31" was 31 inferences at once against a single llama-swap
// endpoint. Accept now ADMITS — a record plus a place in a FIFO — and a single
// scheduler goroutine claims jobs only while a slot is free, so `accepted` is a
// real state a job can sit in and the two limits are independent:
//
//	backlog     — Jobs never enforces it. Admission is the SERVER's gate
//	              (fleet_max_queue_depth over QueueDepth), unchanged from
//	              0.99.0, so the refusal boundary did not move.
//	concurrency — maxConcurrent here, and only here. <= 0 = unlimited, which
//	              reproduces the pre-0.100.0 goroutine-per-job behaviour
//	              through this same code path (no second branch that only the
//	              unlimited config would ever execute).
//
// Poll semantics are UNTOUCHED: same ids, same idempotent re-ack, same
// write-once terminal states, same TTL janitor. The only difference a poller
// can observe is that `accepted` may now legitimately last a while.
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
	// Agent marks a job created by an AGENT dispatch (AcceptAgent). The server
	// keys /fleet/jobs/{id} bearer auth on it — media-created jobs stay
	// tokenless (auth scope v1 = the agent lane only). Never serialized to the
	// wire: jobWire's shape is unchanged.
	Agent bool
}

// job is the mutable store entry; guarded by Jobs.mu.
type job struct {
	state      JobState
	data       json.RawMessage
	err        string
	agent      bool      // created via AcceptAgent → poll auth applies (server.go handleJob)
	terminalAt time.Time // set when state turns done|error; drives ttl eviction
	// run is the work itself, parked on the RECORD (not alongside the id in
	// the pending FIFO) because the FIFO holds ids: an id whose record was
	// drain-marked or evicted between admission and claim must be skippable by
	// looking the record up, and a closure held in a parallel structure would
	// outlive it. Cleared at claim so a long-lived terminal entry stops
	// pinning the request's materialized payload for the whole TTL window.
	run func(context.Context) (json.RawMessage, error)
}

// Jobs is the concurrency-safe ack-then-poll job store AND its scheduler.
// Terminal results are retained for ttl (contract: ≥ a few minutes; we run 1h)
// and swept by a janitor goroutine. DrainAndStop is the shutdown path.
//
// Every mutable field is guarded by mu — there is exactly one mutex here, and
// cond locks that same one, so no lock ordering exists to get wrong. Three
// goroutine kinds touch this struct: the janitor (sweep), the scheduler
// (schedule), and one execute per running job. The only lock-free read is
// maxConcurrent, which is written once in the constructor before any of them
// is started and never again.
type Jobs struct {
	mu sync.RWMutex
	// cond carries every event the scheduler waits on: work admitted, a slot
	// freed, drain begun. It locks the SAME mu that guards the map, so "is
	// there a claimable job?" is answered against the very state the claim
	// then mutates — there is no window between deciding and claiming.
	cond     *sync.Cond
	m        map[string]*job
	pending  []string // FIFO of admitted-but-unclaimed ids (order = arrival)
	draining bool

	ttl time.Duration
	now func() time.Time // injectable clock (janitor tests)

	// maxConcurrent bounds how many jobs EXECUTE at once; <= 0 = unlimited.
	maxConcurrent int

	wg     sync.WaitGroup  // tracks in-flight run goroutines
	ctx    context.Context // handed to every run; cancelled at drain timeout
	cancel context.CancelFunc

	stopJanitor chan struct{}
	stopOnce    sync.Once
}

// NewJobs builds a store whose terminal entries live for ttl, with the janitor
// sweeping every 5 minutes (the spec's cadence). maxConcurrent bounds how many
// admitted jobs execute at once (<= 0 = unlimited); the rest wait in accepted.
func NewJobs(ttl time.Duration, maxConcurrent int) *Jobs {
	return newJobs(ttl, time.Now, 5*time.Minute, maxConcurrent)
}

// newJobs is NewJobs with an injectable clock + janitor tick (unit-testable).
func newJobs(ttl time.Duration, now func() time.Time, janitorTick time.Duration, maxConcurrent int) *Jobs {
	ctx, cancel := context.WithCancel(context.Background())
	j := &Jobs{
		m:             map[string]*job{},
		ttl:           ttl,
		now:           now,
		maxConcurrent: maxConcurrent,
		ctx:           ctx,
		cancel:        cancel,
		stopJanitor:   make(chan struct{}),
	}
	j.cond = sync.NewCond(&j.mu)
	go j.janitor(janitorTick)
	go j.schedule()
	return j
}

// MaxConcurrent reports the store's execution limit; 0 = unlimited. Published
// in health so a delegator can see a node's CAPACITY, not just its depth.
// Lock-free by construction: maxConcurrent is immutable once NewJobs returns.
func (j *Jobs) MaxConcurrent() int {
	if j.maxConcurrent < 0 {
		return 0
	}
	return j.maxConcurrent
}

// Counts splits the in-flight population health used to report as one number:
// queued = admitted but not yet executing (state accepted), running = executing
// (state running). Terminal entries are results awaiting pollers, not load, and
// count as neither. QueueDepth is defined as their sum, so the published
// queue_depth can never drift from the split published beside it.
func (j *Jobs) Counts() (queued, running int) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	for _, jb := range j.m {
		switch jb.state {
		case JobAccepted:
			queued++
		case JobRunning:
			running++
		}
	}
	return queued, running
}

// Accept ADMITS id — it records the job as `accepted` and queues it for the
// scheduler — returning immediately (the ack). It does NOT start the run: that
// happens when an execution slot frees, which may be at once or may be a while
// (see schedule). Before 0.100.0 this function was the start, which is exactly
// why the node had no queue.
//
// Idempotent: an id already present — in ANY state — returns false and never
// starts a second run (the contract's duplicate-POST rule). During drain it
// refuses all new work (false; the server maps that to 503 via Draining()).
// Accept never enforces a backlog limit; admission is the server's gate.
func (j *Jobs) Accept(id string, run func(context.Context) (json.RawMessage, error)) (created bool) {
	return j.accept(id, false, run)
}

// AcceptAgent is Accept for a job created by an AGENT dispatch: identical
// lifecycle, but the record carries the agent marker the server's poll-auth
// gate keys on. The marker lives ON the job record — not in a server-side id
// set — so it is written atomically with creation (no window where a poller
// can observe the job before it is marked) and is evicted WITH the record
// (an id set would outlive the janitor's sweep and leak forever).
func (j *Jobs) AcceptAgent(id string, run func(context.Context) (json.RawMessage, error)) (created bool) {
	return j.accept(id, true, run)
}

func (j *Jobs) accept(id string, agent bool, run func(context.Context) (json.RawMessage, error)) (created bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.draining {
		return false
	}
	if _, exists := j.m[id]; exists {
		return false
	}
	j.m[id] = &job{state: JobAccepted, agent: agent, run: run}
	j.pending = append(j.pending, id)
	j.cond.Broadcast() // wake the scheduler: there is work
	return true
}

// schedule is the ONE goroutine that turns admitted jobs into running ones. It
// blocks on cond until something is claimable (work present AND a slot free),
// claims exactly one job under the lock — dequeue and accepted→running are a
// single atomic step, which is what lets "still accepted at drain time" mean
// "nothing ever began this job" — then hands it to its own goroutine and loops.
//
// Deliberately a scheduler plus a goroutine per RUNNING job, not N parked
// workers: the unlimited setting (<= 0) then costs nothing (no pool sized to
// represent "no limit") and travels this same path as a bounded one, so the
// unlimited config is never an untested second branch.
func (j *Jobs) schedule() {
	for {
		j.mu.Lock()
		for !j.draining && !j.claimableLocked() {
			j.cond.Wait()
		}
		if j.draining {
			j.mu.Unlock()
			return
		}
		id, run, ok := j.claimLocked()
		if !ok {
			// Every pending id was stale (evicted, or already terminal). The
			// FIFO is drained; loop back and wait for real work.
			j.mu.Unlock()
			continue
		}
		// wg.Add happens HERE, under the same mu that guards j.draining, and
		// DrainAndStop sets draining under that mu BEFORE it starts waiting on
		// wg. So an Add can never race a Wait that has already reached zero —
		// the classic WaitGroup reuse hazard is closed by the drain flag, not
		// by timing.
		j.wg.Add(1)
		j.mu.Unlock()
		go j.execute(id, run)
	}
}

// claimableLocked reports whether the scheduler has something to do right now.
// Caller holds mu.
func (j *Jobs) claimableLocked() bool {
	if len(j.pending) == 0 {
		return false
	}
	if j.maxConcurrent <= 0 {
		return true // unlimited
	}
	return j.runningLocked() < j.maxConcurrent
}

// runningLocked counts jobs currently EXECUTING. Derived from the map rather
// than kept as a side counter on purpose: the number the scheduler admits
// against and the number health publishes are then the same fact and cannot
// drift. Caller holds mu.
func (j *Jobs) runningLocked() int {
	n := 0
	for _, jb := range j.m {
		if jb.state == JobRunning {
			n++
		}
	}
	return n
}

// claimLocked pops the oldest genuinely-runnable job, marking it running in the
// same critical section, and returns its work. Stale ids (evicted, or marked
// terminal by drain) are discarded as it goes. Caller holds mu.
func (j *Jobs) claimLocked() (string, func(context.Context) (json.RawMessage, error), bool) {
	for len(j.pending) > 0 {
		id := j.pending[0]
		j.pending = j.pending[1:]
		if len(j.pending) == 0 {
			j.pending = nil // drop the advanced backing array
		}
		jb, ok := j.m[id]
		if !ok || jb.state != JobAccepted || jb.run == nil {
			continue
		}
		run := jb.run
		jb.run = nil
		jb.state = JobRunning
		return id, run, true
	}
	return "", nil, false
}

// execute runs one claimed job to completion and then frees its slot. The
// Broadcast happens AFTER finish has written the terminal state, so the
// scheduler's next runningLocked() already sees the slot as free.
func (j *Jobs) execute(id string, run func(context.Context) (json.RawMessage, error)) {
	defer func() {
		j.wg.Done()
		j.mu.Lock()
		j.cond.Broadcast() // a slot freed
		j.mu.Unlock()
	}()
	data, err := run(j.ctx)
	if err != nil {
		j.finish(id, nil, err.Error())
		return
	}
	j.finish(id, data, "")
}

// Get returns a copy of the job's visible state; false = unknown/evicted (404).
func (j *Jobs) Get(id string) (*JobView, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	jb, ok := j.m[id]
	if !ok {
		return nil, false
	}
	return &JobView{ID: id, State: jb.state, Data: jb.data, Error: jb.err, Agent: jb.agent}, true
}

// QueueDepth counts accepted+running jobs (the health field). Terminal entries
// are results awaiting pollers, not load. Its meaning is UNCHANGED across the
// 0.100.0 queue split — it is now expressed as queued+running so the wire's
// queue_depth and the jobs_queued/jobs_running pair beside it are arithmetic,
// not two independent walks that could disagree.
func (j *Jobs) QueueDepth() int {
	queued, running := j.Counts()
	return queued + running
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

// ErrInterrupted / ErrNeverStarted are the two shutdown verdicts. They are
// DIFFERENT FACTS about the node and must never be collapsed: "interrupted"
// means this node began the work and was cut off — the result may be partially
// materialized, and a caller re-dispatching elsewhere is redoing work that was
// underway. "not started" means the job sat in the backlog and no inference
// ever ran for it, which is the cheapest possible thing to re-dispatch and the
// clearest thing to tell an operator. Before 0.100.0 the second case could not
// exist (Accept was start), so every survivor honestly said "interrupted";
// now that jobs really do wait, reporting a never-run job as interrupted would
// be the store inventing history.
const (
	ErrInterrupted  = "interrupted"
	ErrNeverStarted = "not started: node shut down while this job was queued"
)

// DrainAndStop stops accepting, waits up to timeout for in-flight runs, marks
// every non-terminal survivor terminal — the contract's shutdown obligation
// (once acked, a job is never re-dispatched, so pollers must still reach a
// terminal state) — then cancels their context and waits (bounded by
// killDeliveryGrace) for the released run goroutines to actually return, so
// the cancel-triggered kill of child process trees lands before the process
// exits. The mark happens BEFORE the cancel so a run released by the cancel
// can never race its late completion past the mark (finish never overwrites a
// terminal state). Total bound: timeout + killDeliveryGrace. Also stops the
// janitor and the scheduler. Safe to call once per store.
//
// Survivors are marked by STATE, which is exactly the distinction the
// scheduler's atomic claim makes available: still `accepted` = the scheduler
// never took it (ErrNeverStarted); `running` = a real execution was cut off
// (ErrInterrupted). The pending FIFO is emptied up front so nothing new can
// start during the wait — drain lets running work finish, it does not work
// through the backlog.
func (j *Jobs) DrainAndStop(timeout time.Duration) {
	j.mu.Lock()
	j.draining = true
	j.pending = nil    // nothing queued will ever start now
	j.cond.Broadcast() // release the scheduler so it can exit
	j.mu.Unlock()

	done := make(chan struct{})
	go func() { j.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}

	j.mu.Lock()
	for _, jb := range j.m {
		switch jb.state {
		case JobRunning:
			jb.state = JobError
			jb.err = ErrInterrupted
			jb.terminalAt = j.now()
		case JobAccepted:
			jb.state = JobError
			jb.err = ErrNeverStarted
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
