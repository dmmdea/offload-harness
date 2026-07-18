package fleetnode

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitJobState polls Get until the job reaches want (or the deadline trips).
func waitJobState(t *testing.T, j *Jobs, id string, want JobState) *JobView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := j.Get(id); ok && v.State == want {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	v, ok := j.Get(id)
	t.Fatalf("job %q never reached %q (last: ok=%v view=%+v)", id, want, ok, v)
	return nil
}

// TestJobsStateMachineDone: accepted -> running -> done{data}, and the terminal
// result stays queryable (retention is the janitor's business, not completion's).
func TestJobsStateMachineDone(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour)
	defer j.DrainAndStop(time.Second)

	release := make(chan struct{})
	created := j.Accept("job-1", func(ctx context.Context) (json.RawMessage, error) {
		<-release
		return json.RawMessage(`{"image_path":"x.png"}`), nil
	})
	if !created {
		t.Fatal("first Accept must create")
	}
	// Job is immediately visible as accepted or running (ack-then-poll: the POST
	// handler returns before the run finishes).
	if v, ok := j.Get("job-1"); !ok || (v.State != JobAccepted && v.State != JobRunning) {
		t.Fatalf("freshly accepted job: ok=%v state=%v", ok, v)
	}
	waitJobState(t, j, "job-1", JobRunning)
	close(release)
	v := waitJobState(t, j, "job-1", JobDone)
	if string(v.Data) != `{"image_path":"x.png"}` {
		t.Fatalf("done data = %s", v.Data)
	}
	if v.Error != "" {
		t.Fatalf("done job must carry no error, got %q", v.Error)
	}
}

// TestJobsStateMachineError: a failing run lands in state error with the reason.
func TestJobsStateMachineError(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour)
	defer j.DrainAndStop(time.Second)

	j.Accept("job-e", func(ctx context.Context) (json.RawMessage, error) {
		return nil, errors.New("oom: cudaMalloc failed")
	})
	v := waitJobState(t, j, "job-e", JobError)
	if v.Error != "oom: cudaMalloc failed" {
		t.Fatalf("error = %q", v.Error)
	}
	if v.Data != nil {
		t.Fatalf("error job must carry no data, got %s", v.Data)
	}
}

// TestJobsIdempotentAccept: a duplicate job_id never starts a second run — the
// contract's idempotency rule (once acked, the job is ours; re-POSTs are lookups).
func TestJobsIdempotentAccept(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour)
	defer j.DrainAndStop(time.Second)

	var runs atomic.Int32
	run := func(ctx context.Context) (json.RawMessage, error) {
		runs.Add(1)
		return json.RawMessage(`1`), nil
	}
	if !j.Accept("dup", run) {
		t.Fatal("first Accept must create")
	}
	if j.Accept("dup", run) {
		t.Fatal("second Accept with the same id must NOT create")
	}
	waitJobState(t, j, "dup", JobDone)
	// A duplicate after the terminal state is STILL a duplicate (never re-run).
	if j.Accept("dup", run) {
		t.Fatal("Accept after terminal state must NOT re-create")
	}
	time.Sleep(20 * time.Millisecond)
	if n := runs.Load(); n != 1 {
		t.Fatalf("run executed %d times, want exactly 1", n)
	}
}

// TestJobsConcurrentAcceptExactlyOnce: hammering the same id from many goroutines
// yields exactly one created=true and exactly one run execution.
func TestJobsConcurrentAcceptExactlyOnce(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour)
	defer j.DrainAndStop(time.Second)

	var created, runs atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if j.Accept("race", func(ctx context.Context) (json.RawMessage, error) {
				runs.Add(1)
				return nil, nil
			}) {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	waitJobState(t, j, "race", JobDone)
	if created.Load() != 1 {
		t.Fatalf("created %d times, want 1", created.Load())
	}
	if runs.Load() != 1 {
		t.Fatalf("run executed %d times, want 1", runs.Load())
	}
}

// TestJobsQueueDepth: accepted+running count; terminal jobs do not.
func TestJobsQueueDepth(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour)
	defer j.DrainAndStop(time.Second)

	if d := j.QueueDepth(); d != 0 {
		t.Fatalf("empty store depth = %d", d)
	}
	release := make(chan struct{})
	for _, id := range []string{"a", "b"} {
		j.Accept(id, func(ctx context.Context) (json.RawMessage, error) {
			<-release
			return nil, nil
		})
	}
	if d := j.QueueDepth(); d != 2 {
		t.Fatalf("in-flight depth = %d, want 2", d)
	}
	close(release)
	waitJobState(t, j, "a", JobDone)
	waitJobState(t, j, "b", JobDone)
	if d := j.QueueDepth(); d != 0 {
		t.Fatalf("terminal jobs must not count: depth = %d", d)
	}
}

// TestJobsGetUnknown: unknown id -> not found (the server's 404).
func TestJobsGetUnknown(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour)
	defer j.DrainAndStop(time.Second)
	if _, ok := j.Get("nope"); ok {
		t.Fatal("unknown id must not be found")
	}
}

// TestJobsJanitorEvictsTerminal (fake clock): terminal entries older than ttl are
// evicted by a sweep; younger ones and in-flight ones survive.
func TestJobsJanitorEvictsTerminal(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	j := newJobs(time.Hour, clock, time.Hour)
	defer j.DrainAndStop(time.Second)

	j.Accept("old", func(ctx context.Context) (json.RawMessage, error) { return json.RawMessage(`1`), nil })
	waitJobState(t, j, "old", JobDone)

	blocked := make(chan struct{})
	j.Accept("inflight", func(ctx context.Context) (json.RawMessage, error) { <-blocked; return nil, nil })

	advance(30 * time.Minute)
	j.sweep()
	if _, ok := j.Get("old"); !ok {
		t.Fatal("terminal job younger than ttl must survive the sweep")
	}

	advance(45 * time.Minute) // now 1h15m past "old"'s completion
	j.Accept("young", func(ctx context.Context) (json.RawMessage, error) { return nil, nil })
	waitJobState(t, j, "young", JobDone)
	j.sweep()
	if _, ok := j.Get("old"); ok {
		t.Fatal("terminal job older than ttl must be evicted")
	}
	if _, ok := j.Get("young"); !ok {
		t.Fatal("fresh terminal job must survive")
	}
	if _, ok := j.Get("inflight"); !ok {
		t.Fatal("in-flight job must NEVER be evicted, regardless of age")
	}
	close(blocked)
}

// TestJobsJanitorGoroutineRuns (live): with a tiny ttl+tick the janitor loop
// itself evicts without any manual sweep call.
func TestJobsJanitorGoroutineRuns(t *testing.T) {
	j := newJobs(time.Millisecond, time.Now, 10*time.Millisecond)
	defer j.DrainAndStop(time.Second)

	j.Accept("gone", func(ctx context.Context) (json.RawMessage, error) { return nil, nil })
	waitJobState(t, j, "gone", JobDone)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := j.Get("gone"); !ok {
			return // evicted by the janitor goroutine
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("janitor goroutine never evicted the expired terminal job")
}

// TestJobsDrainMarksSurvivorsInterrupted: a run that outlives the drain timeout is
// marked terminal error:"interrupted" (pollers always reach a terminal state), its
// context is cancelled, and its late completion must NOT overwrite the mark.
func TestJobsDrainMarksSurvivorsInterrupted(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour)

	finished := make(chan struct{})
	j.Accept("stuck", func(ctx context.Context) (json.RawMessage, error) {
		<-ctx.Done() // a hung render released only by the drain's cancel
		defer close(finished)
		return json.RawMessage(`{"late":true}`), nil // late success must be ignored
	})
	waitJobState(t, j, "stuck", JobRunning)

	start := time.Now()
	j.DrainAndStop(50 * time.Millisecond)
	if e := time.Since(start); e > 3*time.Second {
		t.Fatalf("DrainAndStop hung for %v", e)
	}
	v, ok := j.Get("stuck")
	if !ok || v.State != JobError || v.Error != "interrupted" {
		t.Fatalf("survivor: ok=%v view=%+v, want state=error error=interrupted", ok, v)
	}
	<-finished
	time.Sleep(20 * time.Millisecond)
	if v, _ := j.Get("stuck"); v.State != JobError || v.Error != "interrupted" || v.Data != nil {
		t.Fatalf("late completion overwrote the interrupted mark: %+v", v)
	}
}

// TestJobsDrainWaitsForKillDelivery: after the drain timeout cancels the run
// contexts, DrainAndStop must NOT return until the released run goroutines
// actually finish (bounded by a second 5s wait) — gpugen's killTree is spawned
// by CommandContext on that cancel, and on Windows children survive parent
// death: exiting before the kill lands orphans ComfyUI and pins VRAM.
func TestJobsDrainWaitsForKillDelivery(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour)

	var finished atomic.Bool
	j.Accept("slow-kill", func(ctx context.Context) (json.RawMessage, error) {
		<-ctx.Done()                // a hung render released only by the drain's cancel
		time.Sleep(1 * time.Second) // killTree delivering to the child tree
		finished.Store(true)
		return nil, errors.New("cancelled")
	})
	waitJobState(t, j, "slow-kill", JobRunning)

	start := time.Now()
	j.DrainAndStop(50 * time.Millisecond)
	elapsed := time.Since(start)
	if !finished.Load() {
		t.Fatal("DrainAndStop returned before the released run goroutine finished — the kill was not delivered")
	}
	if elapsed > 4*time.Second {
		t.Fatalf("DrainAndStop took %v, want bounded (~timeout + kill delivery)", elapsed)
	}
	// The survivor is still marked interrupted (the mark precedes the cancel;
	// the run's late outcome never overwrites it).
	if v, ok := j.Get("slow-kill"); !ok || v.State != JobError || v.Error != "interrupted" {
		t.Fatalf("survivor: ok=%v view=%+v, want state=error error=interrupted", ok, v)
	}
}

// TestJobsDrainRejectsNewAccepts: after drain begins, Accept refuses (the server
// turns this into 503 node draining) and Draining() reports it.
func TestJobsDrainRejectsNewAccepts(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour)
	j.DrainAndStop(10 * time.Millisecond)
	if !j.Draining() {
		t.Fatal("Draining() must be true after DrainAndStop")
	}
	if j.Accept("late", func(ctx context.Context) (json.RawMessage, error) { return nil, nil }) {
		t.Fatal("Accept during/after drain must not create")
	}
	if _, ok := j.Get("late"); ok {
		t.Fatal("rejected job must not exist")
	}
}

// TestJobsDrainWaitsForFastJobs: a run that completes inside the timeout drains
// clean — done with data, never "interrupted".
func TestJobsDrainWaitsForFastJobs(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour)
	j.Accept("quick", func(ctx context.Context) (json.RawMessage, error) {
		time.Sleep(30 * time.Millisecond)
		return json.RawMessage(`"ok"`), nil
	})
	j.DrainAndStop(5 * time.Second)
	v, ok := j.Get("quick")
	if !ok || v.State != JobDone || string(v.Data) != `"ok"` {
		t.Fatalf("fast job must drain clean: ok=%v view=%+v", ok, v)
	}
}
