package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
)

// This file pins the 0.100.0 BACKLOG/CONCURRENCY SPLIT. Before it, Accept was
// start: a job launched its own goroutine the instant it was admitted, so
// `accepted` lasted microseconds, there was no waiting state, and
// fleet_max_queue_depth bounded SIMULTANEOUS EXECUTION rather than a backlog.
// The tests here bind the two limits separately — a busy pool must make a job
// WAIT (not run, and not be refused), and the admission cap must refuse only
// when the node is genuinely full.

// waitCount polls f until it returns want, then reports success. Used instead
// of a sleep so a slow box never turns a correct pool into a flake.
func waitCount(t *testing.T, want int, f func() int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("value never reached %d (last: %d)", want, f())
}

// blockingRun returns a run func that reports its entry on entered and blocks
// until release closes (or its context is cancelled — the drain path).
func blockingRun(entered chan<- string, id string, release <-chan struct{}) func(context.Context) (json.RawMessage, error) {
	return func(ctx context.Context) (json.RawMessage, error) {
		select {
		case entered <- id:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
}

// TestJobsQueuedWhileWorkersBusy is THE test the old design could not pass: a
// job admitted while every worker is busy must WAIT in `accepted` — its run
// func must not be entered — rather than starting a second concurrent
// execution against the shared endpoint. Before 0.100.0 Accept was start, so
// both runs entered immediately and `accepted` was never observable.
func TestJobsQueuedWhileWorkersBusy(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour, 1)
	defer j.DrainAndStop(time.Second)

	release := make(chan struct{})
	var entries atomic.Int32
	run := func(ctx context.Context) (json.RawMessage, error) {
		entries.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return json.RawMessage(`1`), nil
	}

	if !j.Accept("busy-1", run) {
		t.Fatal("first Accept must create")
	}
	waitJobState(t, j, "busy-1", JobRunning)

	if !j.Accept("busy-2", run) {
		t.Fatal("second Accept must still ADMIT — a busy worker pool is not a full node")
	}
	// Give a would-be second execution every chance to show itself.
	time.Sleep(100 * time.Millisecond)

	if n := entries.Load(); n != 1 {
		t.Fatalf("%d run funcs entered, want exactly 1 — the pool let a queued job execute", n)
	}
	v, ok := j.Get("busy-2")
	if !ok || v.State != JobAccepted {
		t.Fatalf("queued job: ok=%v view=%+v, want state=accepted (a real waiting state)", ok, v)
	}
	queued, running := j.Counts()
	if queued != 1 || running != 1 {
		t.Fatalf("Counts() = (queued %d, running %d), want (1, 1)", queued, running)
	}
	if d := j.QueueDepth(); d != 2 {
		t.Fatalf("QueueDepth() = %d, want 2 — queue_depth must still count accepted+running", d)
	}

	// And the queued job runs as soon as the worker frees: waiting is not losing.
	close(release)
	waitJobState(t, j, "busy-1", JobDone)
	waitJobState(t, j, "busy-2", JobDone)
	if n := entries.Load(); n != 2 {
		t.Fatalf("%d run funcs entered after release, want 2", n)
	}
}

// TestJobsConcurrencyNeverExceedsLimit is the MUTATION-BOUND test: a burst far
// larger than the pool must never have more than maxConcurrent runs inside
// their run func at the same time. Raising the limit in jobs.go makes this go
// red. It samples the live peak from inside the runs themselves, so it cannot
// be satisfied by a counter the store keeps about itself.
func TestJobsConcurrencyNeverExceedsLimit(t *testing.T) {
	const limit = 3
	const burst = 24

	j := newJobs(time.Hour, time.Now, time.Hour, limit)
	defer j.DrainAndStop(2 * time.Second)

	var mu sync.Mutex
	live, peak := 0, 0
	run := func(ctx context.Context) (json.RawMessage, error) {
		mu.Lock()
		live++
		if live > peak {
			peak = live
		}
		mu.Unlock()
		// Hold the slot long enough that an unbounded design piles up visibly.
		time.Sleep(15 * time.Millisecond)
		mu.Lock()
		live--
		mu.Unlock()
		return json.RawMessage(`1`), nil
	}

	ids := make([]string, 0, burst)
	for i := 0; i < burst; i++ {
		id := "burst-" + strconv.Itoa(i)
		ids = append(ids, id)
		if !j.Accept(id, run) {
			t.Fatalf("Accept(%q) must admit — a burst is backlog, not refusal", id)
		}
	}
	for _, id := range ids {
		waitJobState(t, j, id, JobDone)
	}

	mu.Lock()
	got := peak
	mu.Unlock()
	if got > limit {
		t.Fatalf("peak simultaneous runs = %d, want <= %d — the concurrency limit does not bind", got, limit)
	}
	if got < 2 {
		t.Fatalf("peak simultaneous runs = %d: the burst never actually overlapped, so this test proved nothing", got)
	}
}

// TestJobsUnlimitedConcurrencyControlArm is the control for the test above:
// the same burst with the limit disabled (<= 0 = unlimited) overlaps far past
// 3, proving the bound above is the POOL's doing and not an artifact of how
// fast the fake runs are.
func TestJobsUnlimitedConcurrencyControlArm(t *testing.T) {
	const burst = 24

	j := newJobs(time.Hour, time.Now, time.Hour, 0)
	defer j.DrainAndStop(2 * time.Second)

	var mu sync.Mutex
	live, peak := 0, 0
	release := make(chan struct{})
	run := func(ctx context.Context) (json.RawMessage, error) {
		mu.Lock()
		live++
		if live > peak {
			peak = live
		}
		mu.Unlock()
		<-release
		mu.Lock()
		live--
		mu.Unlock()
		return json.RawMessage(`1`), nil
	}

	ids := make([]string, 0, burst)
	for i := 0; i < burst; i++ {
		id := "unl-" + strconv.Itoa(i)
		ids = append(ids, id)
		j.Accept(id, run)
	}
	waitCount(t, burst, func() int { _, running := j.Counts(); return running })
	close(release)
	for _, id := range ids {
		waitJobState(t, j, id, JobDone)
	}
	mu.Lock()
	got := peak
	mu.Unlock()
	if got != burst {
		t.Fatalf("unlimited peak = %d, want %d — unlimited must keep the pre-0.100.0 goroutine-per-job behaviour", got, burst)
	}
}

// TestJobsDrainDistinguishesNeverStartedFromInterrupted: drain owes every
// non-terminal job a terminal state, but a job that NEVER RAN and a job killed
// mid-render are different facts about the node, and a poller (or an operator
// reading a defer reason) must be able to tell them apart. Before 0.100.0 the
// distinction could not exist — nothing ever waited.
func TestJobsDrainDistinguishesNeverStartedFromInterrupted(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour, 1)

	entered := make(chan string, 4)
	release := make(chan struct{})
	defer close(release)

	if !j.Accept("running-one", blockingRun(entered, "running-one", release)) {
		t.Fatal("Accept(running-one) must create")
	}
	waitJobState(t, j, "running-one", JobRunning)
	if !j.Accept("queued-one", blockingRun(entered, "queued-one", release)) {
		t.Fatal("Accept(queued-one) must create")
	}
	waitCount(t, 1, func() int { q, _ := j.Counts(); return q })

	j.DrainAndStop(50 * time.Millisecond)

	ran, ok := j.Get("running-one")
	if !ok || ran.State != JobError || ran.Error != "interrupted" {
		t.Fatalf("mid-run survivor: ok=%v view=%+v, want state=error error=%q", ok, ran, "interrupted")
	}
	q, ok := j.Get("queued-one")
	if !ok || q.State != JobError {
		t.Fatalf("queued survivor: ok=%v view=%+v, want a terminal error state", ok, q)
	}
	if q.Error == "interrupted" {
		t.Fatalf("queued survivor reports %q — a job that never ran must NOT claim it was interrupted mid-run", q.Error)
	}
	if !strings.Contains(q.Error, "not started") {
		t.Fatalf("queued survivor error = %q, want it to say the job never started", q.Error)
	}
	// The queued job's run func must never have been entered.
	close(entered)
	for id := range entered {
		if id == "queued-one" {
			t.Fatal("a job marked never-started actually executed")
		}
	}
}

// --- server surface ---

// TestDispatchAdmitsWhileWorkersBusyRefusesOnlyWhenFull binds the two limits
// as SEPARATE things over the real HTTP surface: with one worker and an
// admission ceiling of 3, dispatches 2 and 3 are ACCEPTED (202) while the
// single worker is busy — busy is not full — and only the 4th, which would
// exceed the ceiling, is refused 503. Before 0.100.0 the same config ran three
// jobs at once against one endpoint.
func TestDispatchAdmitsWhileWorkersBusyRefusesOnlyWhenFull(t *testing.T) {
	release := make(chan struct{})
	var entries atomic.Int32
	blocked := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		entries.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return core.Result{OK: true, Data: json.RawMessage(`{"image_path":"x.png"}`)}
	}}
	cfg := imageCfg()
	cfg.FleetMaxQueueDepth = 3     // admission ceiling on accepted+running
	cfg.FleetMaxConcurrentJobs = 1 // one execution at a time
	s, _ := newTestServer(t, cfg, blocked, nil)

	for _, id := range []string{"sp-1", "sp-2", "sp-3"} {
		if rec := dispatchImage(t, s, id); rec.Code != http.StatusAccepted {
			t.Fatalf("dispatch %s = %d, want 202 — a busy worker pool must BACKLOG, not refuse (body %s)", id, rec.Code, rec.Body.String())
		}
	}
	wantErrorShape(t, dispatchImage(t, s, "sp-4"), http.StatusServiceUnavailable, "queue full")

	// Exactly one is executing; the other two wait.
	time.Sleep(100 * time.Millisecond)
	if n := entries.Load(); n != 1 {
		t.Fatalf("%d runs entered, want 1 — the concurrency limit did not bind through the server", n)
	}
	m := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
	if m["jobs_running"] != float64(1) || m["jobs_queued"] != float64(2) {
		t.Fatalf("health running/queued = %v/%v, want 1/2", m["jobs_running"], m["jobs_queued"])
	}
	if m["queue_depth"] != float64(3) {
		t.Fatalf("queue_depth = %v, want 3 (accepted+running, unchanged meaning)", m["queue_depth"])
	}

	close(release)
	for _, id := range []string{"sp-1", "sp-2", "sp-3"} {
		pollJob(t, s, id, JobDone)
	}
	if n := entries.Load(); n != 3 {
		t.Fatalf("%d runs entered after release, want 3 — backlogged jobs must eventually run", n)
	}
}

// TestHealthPublishesCapacity: the delegator could see a node's DEPTH but
// never its CAPACITY, so it could not tell a node with room from one at its
// ceiling. Health now publishes both limits alongside the split counters, and
// the limits are the node's own resolved values (not the raw config ints).
func TestHealthPublishesCapacity(t *testing.T) {
	cfg := imageCfg()
	cfg.FleetMaxQueueDepth = 9
	cfg.FleetMaxConcurrentJobs = 2
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)

	m := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
	for field, want := range map[string]float64{
		"max_queue_depth":     9,
		"max_concurrent_jobs": 2,
		"jobs_queued":         0,
		"jobs_running":        0,
		"queue_depth":         0,
	} {
		if m[field] != want {
			t.Fatalf("health %s = %v, want %v (body: %v)", field, m[field], want, m)
		}
	}
}

// TestHealthPublishesUnlimitedAsZeroNotAbsent: 0 must be EMITTED for an
// unlimited limit, never omitted. omitempty here would make "unlimited"
// indistinguishable from a pre-0.100.0 node that publishes no limit at all,
// and those two route in opposite directions.
func TestHealthPublishesUnlimitedAsZeroNotAbsent(t *testing.T) {
	cfg := imageCfg()
	cfg.FleetMaxQueueDepth = -1     // unlimited
	cfg.FleetMaxConcurrentJobs = -1 // unlimited
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)

	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	body := rec.Body.String()
	for _, field := range []string{`"max_queue_depth":0`, `"max_concurrent_jobs":0`, `"jobs_queued":0`, `"jobs_running":0`} {
		if !strings.Contains(body, field) {
			t.Fatalf("health body missing %s (unlimited must be an explicit 0, not an absent key): %s", field, body)
		}
	}
}

// TestFleetConcurrencyLimitResolution pins the config resolver's three cases
// against FleetQueueLimit's identical convention — one rule for both caps.
func TestFleetConcurrencyLimitResolution(t *testing.T) {
	for _, tc := range []struct {
		raw, want int
		why       string
	}{
		{0, 4, "0 = the built-in default"},
		{-1, 0, "negative = unlimited (0 to callers, matching FleetQueueLimit)"},
		{1, 1, "an explicit value is honoured verbatim"},
		{16, 16, "an explicit value is honoured verbatim"},
	} {
		c := config.Config{FleetMaxConcurrentJobs: tc.raw}
		if got := c.FleetConcurrencyLimit(); got != tc.want {
			t.Fatalf("FleetConcurrencyLimit(%d) = %d, want %d (%s)", tc.raw, got, tc.want, tc.why)
		}
	}
}
