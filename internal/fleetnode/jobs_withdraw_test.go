package fleetnode

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file pins the node half of ADR 0064: a delegator that gives an acked job
// up can take it back while it has not started (Withdraw), and a node whose
// delegator vanished cleans up after it (the poll-lease reaper). Before this,
// every give-up left the job to run later on a seat nobody was waiting for.

// leaseClock is a hand-driven clock for the lease tests: the lease is a duration
// measured on the store's injected clock, so the tests never sleep through it.
type leaseClock struct {
	mu sync.Mutex
	t  time.Time
}

func newLeaseClock() *leaseClock { return &leaseClock{t: time.Unix(1_700_000_000, 0)} }

func (c *leaseClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *leaseClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// holdSlot admits a job that takes the store's only execution slot and blocks on
// the returned release; it returns once the job is running.
func holdSlot(t *testing.T, j *Jobs, id string) (release func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	if !j.Accept(id, func(ctx context.Context) (json.RawMessage, error) {
		select {
		case <-ch:
		case <-ctx.Done():
		}
		return json.RawMessage(`{}`), nil
	}) {
		t.Fatalf("slot holder %q was not admitted", id)
	}
	waitJobState(t, j, id, JobRunning)
	return func() { once.Do(func() { close(ch) }) }
}

// TestJobsWithdrawRemovesAnAcceptedJobAndNeverARunningOne is the contract of the
// route: a job that never started can be taken back and then never starts; a
// job a slot already took is not touched (Option A: running work stays
// recoverable), and neither is a finished one.
func TestJobsWithdrawRemovesAnAcceptedJobAndNeverARunningOne(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour, 1) // ONE slot, so the second job waits
	defer j.DrainAndStop(time.Second)

	release := holdSlot(t, j, "running-1")
	var ranQueued, dropped atomic.Int32
	if !j.Admit("queued-1", AcceptSpec{Agent: true, OnDropped: func() { dropped.Add(1) }},
		func(ctx context.Context) (json.RawMessage, error) {
			ranQueued.Add(1)
			return json.RawMessage(`{}`), nil
		}) {
		t.Fatal("queued-1 was not admitted")
	}
	if v, _ := j.Get("queued-1"); v.State != JobAccepted {
		t.Fatalf("queued-1 state = %v, want accepted (the only slot is held)", v.State)
	}

	// A running job is refused and left exactly as it was.
	res := j.Withdraw("running-1")
	if !res.Found || res.Withdrawn || res.State != JobRunning {
		t.Fatalf("Withdraw(running) = %+v, want found, not withdrawn, state running", res)
	}
	if v, _ := j.Get("running-1"); v.State != JobRunning {
		t.Fatalf("a withdraw attempt disturbed a RUNNING job: state = %v", v.State)
	}

	// The never-started job is taken back: terminal with the withdrawn text, its
	// parked resources released once, and it stops counting as load.
	res = j.Withdraw("queued-1")
	if !res.Found || !res.Withdrawn {
		t.Fatalf("Withdraw(accepted) = %+v, want found and withdrawn", res)
	}
	v, ok := j.Get("queued-1")
	if !ok || v.State != JobError || v.Error != ErrWithdrawn {
		t.Fatalf("withdrawn job view = ok=%v %+v, want state error with %q", ok, v, ErrWithdrawn)
	}
	if dropped.Load() != 1 {
		t.Fatalf("OnDropped fired %d times, want exactly once (the temp files of a job that never runs)", dropped.Load())
	}
	if d := j.QueueDepth(); d != 1 {
		t.Fatalf("QueueDepth = %d, want 1 (only the running job is load)", d)
	}

	// Idempotent: a retry after a lost answer reads the same verdict and frees nothing twice.
	res = j.Withdraw("queued-1")
	if !res.Found || !res.Withdrawn {
		t.Fatalf("second Withdraw = %+v, want still withdrawn", res)
	}
	if dropped.Load() != 1 {
		t.Fatalf("OnDropped fired %d times after a repeat withdraw, want 1", dropped.Load())
	}
	if res = j.Withdraw("never-seen"); res.Found || res.Withdrawn {
		t.Fatalf("Withdraw(unknown) = %+v, want not found", res)
	}

	// Free the slot: the withdrawn job must NEVER start.
	release()
	waitJobState(t, j, "running-1", JobDone)
	time.Sleep(60 * time.Millisecond)
	if ranQueued.Load() != 0 {
		t.Fatal("a withdrawn job ran anyway — the ghost the withdraw exists to prevent")
	}
	if v, _ := j.Get("queued-1"); v.State != JobError || v.Error != ErrWithdrawn {
		t.Fatalf("withdrawn job changed state after the slot freed: %+v", v)
	}

	// A finished job cannot be withdrawn either, and keeps its result.
	res = j.Withdraw("running-1")
	if !res.Found || res.Withdrawn || res.State != JobDone {
		t.Fatalf("Withdraw(done) = %+v, want found, not withdrawn, state done", res)
	}
}

// TestJobsWithdrawVsClaimIsAtomic: the scheduler flips accepted→running and
// Withdraw flips accepted→withdrawn under the SAME mutex, so of the two exactly
// one wins per job. 64 jobs wait behind a held slot; when it frees the scheduler
// claims them one after another while 64 goroutines withdraw them at once. The
// invariant is checked per job: it either ran (and was not withdrawn) or was
// withdrawn (and never ran) — never both, never neither.
//
// The race detector is not runnable on every box (it needs cgo), so the
// invariant is asserted directly rather than left to -race.
func TestJobsWithdrawVsClaimIsAtomic(t *testing.T) {
	const rounds, jobs = 12, 64
	var withdrawnTotal, ranTotal int
	for round := 0; round < rounds; round++ {
		j := newJobs(time.Hour, time.Now, time.Hour, 1)
		release := holdSlot(t, j, "holder")

		ran := make([]atomic.Bool, jobs)
		for k := 0; k < jobs; k++ {
			k := k
			id := fmt.Sprintf("job-%d", k)
			if !j.Accept(id, func(ctx context.Context) (json.RawMessage, error) {
				ran[k].Store(true)
				time.Sleep(200 * time.Microsecond) // a slow claim order: the scheduler works through the queue while the withdrawals land
				return json.RawMessage(`{}`), nil
			}) {
				t.Fatalf("%s was not admitted", id)
			}
		}

		results := make([]WithdrawResult, jobs)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for k := 0; k < jobs; k++ {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				<-start
				results[k] = j.Withdraw(fmt.Sprintf("job-%d", k))
			}(k)
		}
		close(start)
		release() // the scheduler begins claiming while the withdrawals are in flight
		wg.Wait()

		// Let every job the scheduler took finish before reading the verdicts.
		deadline := time.Now().Add(5 * time.Second)
		for {
			pending := 0
			for k := 0; k < jobs; k++ {
				if v, _ := j.Get(fmt.Sprintf("job-%d", k)); v.State == JobAccepted || v.State == JobRunning {
					pending++
				}
			}
			if pending == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d: %d job(s) never settled", round, pending)
			}
			time.Sleep(2 * time.Millisecond)
		}

		for k := 0; k < jobs; k++ {
			id := fmt.Sprintf("job-%d", k)
			v, _ := j.Get(id)
			didRun, withdrawn := ran[k].Load(), results[k].Withdrawn
			switch {
			case didRun && withdrawn:
				t.Fatalf("round %d: %s BOTH ran and was reported withdrawn (view %+v) — the withdraw raced the claim", round, id, v)
			case !didRun && !withdrawn:
				t.Fatalf("round %d: %s neither ran nor was withdrawn (result %+v, view %+v)", round, id, results[k], v)
			case withdrawn && (v.State != JobError || v.Error != ErrWithdrawn):
				t.Fatalf("round %d: %s reported withdrawn but its record says %+v", round, id, v)
			case didRun && v.State != JobDone:
				t.Fatalf("round %d: %s ran but its record says %+v", round, id, v)
			}
			if withdrawn {
				withdrawnTotal++
			} else {
				ranTotal++
			}
		}
		j.DrainAndStop(time.Second)
	}
	if withdrawnTotal == 0 {
		t.Fatal("no withdraw ever won across every round: 64 jobs waited behind a held slot, so a working Withdraw takes most of them")
	}
	if ranTotal == 0 {
		t.Logf("note: every job was withdrawn (%d); the invariant held, but the claim never won a race", withdrawnTotal)
	}
	t.Logf("across %d rounds of %d jobs: %d withdrawn, %d ran, 0 both", rounds, jobs, withdrawnTotal, ranTotal)
}

// compressReaper shortens the reaper's tick for one test. It must run BEFORE the
// store is built: reapLoop reads the tick when it starts.
func compressReaper(t *testing.T, every time.Duration) {
	t.Helper()
	old := reapEvery
	reapEvery = every
	t.Cleanup(func() { reapEvery = old })
}

// TestJobsReapsAnAcceptedJobNobodyPollsAnymore runs the LIVE reaper goroutine
// against a compressed lease: a pushed agent job that sits accepted with nobody
// polling it is reaped, its resources are released, and it never starts — while a
// job whose poller keeps polling survives the same wait.
func TestJobsReapsAnAcceptedJobNobodyPollsAnymore(t *testing.T) {
	compressReaper(t, 10*time.Millisecond)
	j := newJobs(time.Hour, time.Now, time.Hour, 1)
	defer j.DrainAndStop(time.Second)
	j.SetPollLease(80 * time.Millisecond)
	release := holdSlot(t, j, "holder")

	var ranGhost, ranPolled, dropped atomic.Int32
	admit := func(id string, ran *atomic.Int32) {
		if !j.Admit(id, AcceptSpec{Agent: true, PollLeased: true, OnDropped: func() { dropped.Add(1) }},
			func(ctx context.Context) (json.RawMessage, error) {
				ran.Add(1)
				return json.RawMessage(`{}`), nil
			}) {
			t.Fatalf("%s was not admitted", id)
		}
	}
	admit("ghost", &ranGhost)
	admit("polled", &ranPolled)

	stopPolling := make(chan struct{})
	polling := make(chan struct{})
	go func() {
		defer close(polling)
		for {
			select {
			case <-stopPolling:
				return
			case <-time.After(10 * time.Millisecond):
				j.Touch("polled")
			}
		}
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if v, _ := j.Get("ghost"); v.State == JobError {
			if v.Error != ErrReaped {
				t.Fatalf("ghost error = %q, want %q", v.Error, ErrReaped)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the ghost (accepted, never polled) was never reaped by the live reaper")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // well past the lease: the polled job must still be waiting
	if v, _ := j.Get("polled"); v.State != JobAccepted {
		t.Fatalf("the polled job was reaped (state %v): a job its poller is looking at must survive the lease", v.State)
	}
	if dropped.Load() != 1 {
		t.Fatalf("OnDropped fired %d times, want once (for the ghost only)", dropped.Load())
	}

	close(stopPolling)
	<-polling
	release()
	waitJobState(t, j, "polled", JobDone)
	if ranGhost.Load() != 0 {
		t.Fatal("the reaped ghost started anyway")
	}
	if ranPolled.Load() != 1 {
		t.Fatalf("the polled job ran %d times, want 1", ranPolled.Load())
	}
}

// TestJobsNeverReapsARunningJob: the reaper takes an `accepted` job and nothing
// else. A job a slot already took keeps running however long nobody polls it —
// that work stays recoverable (Option A), and a delegator's recovery pass may
// still collect it.
func TestJobsNeverReapsARunningJob(t *testing.T) {
	clk := newLeaseClock()
	j := newJobs(time.Hour, clk.Now, time.Hour, 0)
	defer j.DrainAndStop(time.Second)
	j.SetPollLease(time.Minute)

	release := make(chan struct{})
	if !j.Admit("long-run", AcceptSpec{Agent: true, PollLeased: true}, func(ctx context.Context) (json.RawMessage, error) {
		<-release
		return json.RawMessage(`{"ok":true}`), nil
	}) {
		t.Fatal("not admitted")
	}
	waitJobState(t, j, "long-run", JobRunning)

	clk.Advance(3 * time.Hour) // three hours with no poll at all
	if n := j.reap(); n != 0 {
		t.Fatalf("reap took %d job(s), want 0: a running job is never reaped", n)
	}
	if v, _ := j.Get("long-run"); v.State != JobRunning {
		t.Fatalf("running job state = %v after a reap pass, want running", v.State)
	}
	close(release)
	if v := waitJobState(t, j, "long-run", JobDone); string(v.Data) != `{"ok":true}` {
		t.Fatalf("result = %s, want the run's own", v.Data)
	}
}

// TestJobsReaperOnlyOwnsPolledJobs: media, vision and pulled-queue jobs are
// admitted without PollLeased — their pollers are other clients (or nobody:
// a pulled job's result travels by ack) — so no amount of silence reaps them.
func TestJobsReaperOnlyOwnsPolledJobs(t *testing.T) {
	clk := newLeaseClock()
	j := newJobs(time.Hour, clk.Now, time.Hour, 1)
	defer j.DrainAndStop(time.Second)
	j.SetPollLease(time.Minute)
	release := holdSlot(t, j, "holder")
	defer release()

	j.Admit("media-1", AcceptSpec{}, func(ctx context.Context) (json.RawMessage, error) { return nil, nil })
	j.Admit("pulled-1", AcceptSpec{Agent: true}, func(ctx context.Context) (json.RawMessage, error) { return nil, nil })
	j.Admit("pushed-1", AcceptSpec{Agent: true, PollLeased: true}, func(ctx context.Context) (json.RawMessage, error) { return nil, nil })

	clk.Advance(2 * time.Hour)
	if n := j.reap(); n != 1 {
		t.Fatalf("reap took %d job(s), want exactly the pushed agent job", n)
	}
	for id, want := range map[string]JobState{"media-1": JobAccepted, "pulled-1": JobAccepted, "pushed-1": JobError} {
		if v, _ := j.Get(id); v.State != want {
			t.Errorf("%s state = %v, want %v", id, v.State, want)
		}
	}
}

// TestJobsClaimSkipsAGhostBeforeTheReaperRuns: the reaper ticks every few
// seconds, but a slot can free at any instant. A job that is already past the
// lease must not be handed that slot in the gap before the next tick — that
// start is exactly how a ghost occupies a seat for nobody.
func TestJobsClaimSkipsAGhostBeforeTheReaperRuns(t *testing.T) {
	clk := newLeaseClock()
	j := newJobs(time.Hour, clk.Now, time.Hour, 1) // reapEvery is 5 s: no tick can fire in this test
	defer j.DrainAndStop(time.Second)
	j.SetPollLease(time.Minute)
	release := holdSlot(t, j, "holder")

	var ranGhost atomic.Int32
	j.Admit("ghost", AcceptSpec{Agent: true, PollLeased: true}, func(ctx context.Context) (json.RawMessage, error) {
		ranGhost.Add(1)
		return json.RawMessage(`{}`), nil
	})
	clk.Advance(61 * time.Second) // past the lease, and nobody polled
	release()                     // the slot frees: the scheduler now scans the queue
	waitJobState(t, j, "holder", JobDone)
	time.Sleep(80 * time.Millisecond)
	if ranGhost.Load() != 0 {
		t.Fatal("the scheduler started a job whose delegator had gone — a ghost, one reaper tick before it would have been cleaned up")
	}
	if v, _ := j.Get("ghost"); v.State != JobAccepted {
		t.Fatalf("ghost state = %v before any reap pass, want it left accepted for the reaper", v.State)
	}
	if n := j.reap(); n != 1 {
		t.Fatalf("reap took %d job(s), want the ghost", n)
	}
	// A late poll rescues a job that was only skipped, never reaped: the poller
	// came back inside the window.
	j.Admit("late", AcceptSpec{Agent: true, PollLeased: true}, func(ctx context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	})
	clk.Advance(61 * time.Second)
	j.Touch("late")
	waitJobState(t, j, "late", JobDone)
}

// TestJobsAParkedLongPollKeepsAJobAlive: a poller blocked in WaitTerminal is
// polling by definition, whatever the lease says — a lease shorter than one long
// poll must not reap the job under the poller's feet.
func TestJobsAParkedLongPollKeepsAJobAlive(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour, 1)
	defer j.DrainAndStop(time.Second)
	j.SetPollLease(30 * time.Millisecond)
	release := holdSlot(t, j, "holder")
	defer release()
	j.Admit("waited-on", AcceptSpec{Agent: true, PollLeased: true}, func(ctx context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	})

	parked := make(chan struct{})
	go func() {
		defer close(parked)
		j.WaitTerminal(context.Background(), "waited-on", 250*time.Millisecond)
	}()
	time.Sleep(20 * time.Millisecond) // the waiter is parked
	for i := 0; i < 10; i++ {
		time.Sleep(20 * time.Millisecond) // > the lease in total
		if n := j.reap(); n != 0 {
			t.Fatalf("reap took a job a long poll was parked on (pass %d)", i)
		}
	}
	<-parked
	// The wait's own end counts as a poll: the lease starts over from there.
	if n := j.reap(); n != 0 {
		t.Fatal("reap took the job right after its long poll returned; the lease must restart when the wait ends")
	}
}

// TestJobsAbandonedRunWallDoesNotFeedRetryAfter: a run whose poller left before
// it finished is a ghost's wall — 4 to 64 minutes of stall on a seat nobody was
// waiting for. Fed into recent_agent_wall_sec it inflated the node's own
// Retry-After, which sent callers away for longer, which produced more ghosts.
func TestJobsAbandonedRunWallDoesNotFeedRetryAfter(t *testing.T) {
	clk := newLeaseClock()
	j := newJobs(time.Hour, clk.Now, time.Hour, 0) // unlimited: both jobs start at once
	defer j.DrainAndStop(time.Second)
	j.SetPollLease(time.Minute)

	relAbandoned, relPolled := make(chan struct{}), make(chan struct{})
	spec := AcceptSpec{Agent: true, PollLeased: true}
	j.Admit("abandoned", spec, func(ctx context.Context) (json.RawMessage, error) { <-relAbandoned; return json.RawMessage(`{}`), nil })
	j.Admit("polled", spec, func(ctx context.Context) (json.RawMessage, error) { <-relPolled; return json.RawMessage(`{}`), nil })
	waitJobState(t, j, "abandoned", JobRunning)
	waitJobState(t, j, "polled", JobRunning)

	// The abandoned job's poller looked once, early, and then went away; the
	// other job's poller stayed to the end.
	clk.Advance(3 * time.Second)
	j.Touch("abandoned")
	j.Touch("polled")
	clk.Advance(2000 * time.Second)
	j.Touch("polled")
	clk.Advance(5 * time.Second)
	close(relAbandoned)
	close(relPolled)
	waitJobState(t, j, "abandoned", JobDone)
	waitJobState(t, j, "polled", JobDone)

	walls := j.FinishedAgentWalls(8)
	if len(walls) != 1 {
		t.Fatalf("FinishedAgentWalls = %v, want exactly the polled job's wall (the abandoned run's is not a sample)", walls)
	}

	// A look AFTER the job finished (a recovery pass, an operator's curl) must not
	// retroactively make the ghost's wall a sample.
	j.Touch("abandoned")
	if walls = j.FinishedAgentWalls(8); len(walls) != 1 {
		t.Fatalf("after a late Touch FinishedAgentWalls = %v, want still one sample", walls)
	}

	// With the lease off nothing is discounted: today's behaviour.
	j.SetPollLease(0)
	if walls = j.FinishedAgentWalls(8); len(walls) != 2 {
		t.Fatalf("with no lease FinishedAgentWalls = %v, want both walls", walls)
	}
}
