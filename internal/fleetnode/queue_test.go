package fleetnode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// agentQueueCfg opts the node into the AGENT lane — the one task type
// fleet_max_concurrent_jobs actually caps (see Server.concurrencyCapped) — with
// the two limits set for a queueing test.
func agentQueueCfg(t *testing.T, depth, concurrency int) config.Config {
	t.Helper()
	return config.Config{
		Home:                   t.TempDir(),
		FleetAgentEnabled:      true,
		AgentModel:             "agent-seat",
		FleetMaxQueueDepth:     depth,
		FleetMaxConcurrentJobs: concurrency,
	}
}

// loopbackAgentServer builds a server whose listener posture admits the
// tokenless agent lane (loopback), over the given runner.
func loopbackAgentServer(t *testing.T, cfg config.Config, r Runner) (*Server, *Jobs) {
	t.Helper()
	return newTestServer(t, cfg, r, &Options{
		NodeID:           "testnode",
		Snapshot:         goodSnapshot,
		Footprints:       func() []FootprintEntry { return nil },
		GpuVendor:        "nvidia",
		GpuArch:          "ampere",
		LoopbackListener: true,
	})
}

// dispatchAgent posts one valid agent dispatch with the given job id.
func dispatchAgent(t *testing.T, s *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"job_id":"` + id + `","task_type":"agent","payload":` +
		`{"schema_version":1,"goal":"summarize","output_schema":{"properties":{"answer":{"type":"string"}}}}}`
	return do(t, s, http.MethodPost, "/fleet/dispatch", body, nil)
}

// TestDispatchAdmitsWhileWorkersBusyRefusesOnlyWhenFull binds the two limits
// as SEPARATE things over the real HTTP surface, on the lane the concurrency
// cap governs: with one execution slot and an admission ceiling of 3,
// dispatches 2 and 3 are ACCEPTED (202) while the slot is busy — busy is not
// full — and only the 4th, which would exceed the ceiling, is refused 503.
// Before 0.100.0 the same config ran three jobs at once against one endpoint.
func TestDispatchAdmitsWhileWorkersBusyRefusesOnlyWhenFull(t *testing.T) {
	release := make(chan struct{})
	var entries atomic.Int32
	blocked := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		entries.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return core.Result{OK: true, Data: json.RawMessage(`{"schema_version":1,"output":"ok"}`)}
	}}
	s, _ := loopbackAgentServer(t, agentQueueCfg(t, 3, 1), blocked)

	for _, id := range []string{"sp-1", "sp-2", "sp-3"} {
		if rec := dispatchAgent(t, s, id); rec.Code != http.StatusAccepted {
			t.Fatalf("dispatch %s = %d, want 202 — a busy execution slot must BACKLOG, not refuse (body %s)", id, rec.Code, rec.Body.String())
		}
	}
	wantErrorShape(t, dispatchAgent(t, s, "sp-4"), http.StatusServiceUnavailable, "queue full")

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

// TestDispatchMediaLaneIsNotConcurrencyCapped is the other half of the lane
// decision, and it is the one that would silently regress: with
// fleet_max_concurrent_jobs at 1, three MEDIA dispatches must all execute
// concurrently. Media serializes itself far harder than this cap would (the
// in-process mediaSlot has capacity ONE, under a machine-wide gpulease), so a
// media job held in `accepted` is not protecting anything — it is occupying a
// fleet execution slot while doing no work, and with the cap applied four such
// jobs would starve the agent lane the cap exists to protect.
func TestDispatchMediaLaneIsNotConcurrencyCapped(t *testing.T) {
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
	cfg.FleetMaxConcurrentJobs = 1 // would cap the agent lane to one
	s, _ := newTestServer(t, cfg, blocked, nil)

	for _, id := range []string{"ml-1", "ml-2", "ml-3"} {
		if rec := dispatchImage(t, s, id); rec.Code != http.StatusAccepted {
			t.Fatalf("dispatch %s = %d, want 202", id, rec.Code)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for entries.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if n := entries.Load(); n != 3 {
		t.Fatalf("%d media runs entered, want 3 — media was held behind the text lane's concurrency cap", n)
	}
	m := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
	if m["jobs_running"] != float64(3) || m["jobs_queued"] != float64(0) {
		t.Fatalf("health running/queued = %v/%v, want 3/0", m["jobs_running"], m["jobs_queued"])
	}
	close(release)
	for _, id := range []string{"ml-1", "ml-2", "ml-3"} {
		pollJob(t, s, id, JobDone)
	}
}

// TestConcurrencyCappedRule pins the lane rule task type by task type, so an
// exemption can never be added or lost silently. The DEFAULT matters as much
// as the list: an unrecognized task type must be CAPPED, because being wrong
// that way costs queue latency and a visible `queue deadline`, while being
// wrong the other way silently reinstates unbounded inference against one
// llama-swap — the defect this release exists to remove.
func TestConcurrencyCappedRule(t *testing.T) {
	cfg := imageCfg()
	cfg.Pipelines = map[string]config.PipelineSpec{"scene-swap": {}}
	s, _ := newTestServer(t, cfg, &fakeRunner{}, nil)

	for _, tc := range []struct {
		task string
		want bool
		why  string
	}{
		{"agent", true, "the llama-swap text lane — the only thing the cap was written for"},
		{"image-gen", false, "acquireMediaLease: mediaSlot (cap 1) + gpulease ClassMedia"},
		{"video-gen", false, "acquireMediaLease"},
		{"audio-gen", false, "acquireMediaLease"},
		{"run-graph", false, "acquireMediaLease"},
		{"stt", false, "whisper-server: a different process on a different endpoint"},
		{"scene-swap", false, "a configured pipeline route — runPipelineJob takes the same mediaSlot"},
		{"some-task-from-2027", true, "unknown task types are capped by default (fail safe for the text endpoint)"},
	} {
		if got := s.concurrencyCapped(tc.task); got != tc.want {
			t.Errorf("concurrencyCapped(%q) = %v, want %v — %s", tc.task, got, tc.want, tc.why)
		}
	}
}

// TestDrainReleasesNeverStartedMaterialization is the leak test. handleDispatch
// parks its temp-file cleanup in a `defer` INSIDE the run closure, which was
// airtight while Accept was start — every admitted job's closure ran. Once a
// job can be admitted and then marked terminal without its closure ever
// executing, that deferred cleanup never fires and the materialized request
// leaks for good. This drives the real HTTP surface and asserts on the real
// filesystem: an agent dispatch materializes a job dir under
// BaseDir()/pipeline-jobs/, and after a drain that never started it, the dir
// must be gone.
func TestDrainReleasesNeverStartedMaterialization(t *testing.T) {
	cfg := agentQueueCfg(t, 8, 1) // one slot: the second dispatch is guaranteed to queue
	started := make(chan struct{})
	var once sync.Once
	blocked := &fakeRunner{fn: func(ctx context.Context, req core.Request) core.Result {
		once.Do(func() { close(started) })
		<-ctx.Done() // released only by the drain's cancel
		return core.Result{OK: false, Reason: "cancelled"}
	}}
	s, jobs := loopbackAgentServer(t, cfg, blocked)

	if rec := dispatchAgent(t, s, "leak-running"); rec.Code != http.StatusAccepted {
		t.Fatalf("first dispatch = %d (body %s)", rec.Code, rec.Body.String())
	}
	<-started // the first job OWNS the only slot
	if rec := dispatchAgent(t, s, "leak-queued"); rec.Code != http.StatusAccepted {
		t.Fatalf("second dispatch = %d (body %s)", rec.Code, rec.Body.String())
	}
	waitCount(t, 1, func() int { q, _ := jobs.Counts(); return q })

	jobsRoot := filepath.Join(cfg.BaseDir(), "pipeline-jobs")
	before, err := os.ReadDir(jobsRoot)
	if err != nil {
		t.Fatalf("reading %s: %v", jobsRoot, err)
	}
	if len(before) != 2 {
		t.Fatalf("%d materialized job dir(s), want 2 — the fixture must have materialized BOTH jobs for this test to mean anything", len(before))
	}

	jobs.DrainAndStop(2 * time.Second)

	// The running job's own deferred cleanup fires when its cancelled run
	// returns; the QUEUED job's never ran, so only the drop hook can release it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		after, rerr := os.ReadDir(jobsRoot)
		if rerr != nil {
			t.Fatalf("reading %s: %v", jobsRoot, rerr)
		}
		if len(after) == 0 {
			break
		}
		if time.Now().After(deadline) {
			names := make([]string, 0, len(after))
			for _, e := range after {
				names = append(names, e.Name())
			}
			t.Fatalf("%d materialized job dir(s) survived the drain (%v) — a job that never started leaked its materialization", len(after), names)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// And the queued job still reports the honest terminal state.
	if v, ok := jobs.Get("leak-queued"); !ok || v.State != JobError || !strings.Contains(v.Error, "not started") {
		t.Fatalf("queued job: ok=%v view=%+v, want error/not started", ok, v)
	}
}

// TestJobsDropHookFiresOnceAndOnlyForNeverStarted pins the hook's contract at
// the store level, where both arms are observable: a job that RAN must never
// have its drop hook called (its own deferred cleanup owns that, and calling
// both would double-free), and a job that never ran must have it called
// exactly once even if DrainAndStop is invoked twice.
func TestJobsDropHookFiresOnceAndOnlyForNeverStarted(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour, 1)

	var ranDrops, queuedDrops atomic.Int32
	started := make(chan struct{})
	j.Admit("ran", AcceptSpec{OnDropped: func() { ranDrops.Add(1) }},
		func(ctx context.Context) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			return nil, errors.New("cancelled")
		})
	<-started
	j.Admit("never-ran", AcceptSpec{OnDropped: func() { queuedDrops.Add(1) }},
		func(ctx context.Context) (json.RawMessage, error) {
			t.Error("the queued job's run must never execute")
			return nil, nil
		})
	waitCount(t, 1, func() int { q, _ := j.Counts(); return q })

	j.DrainAndStop(50 * time.Millisecond)
	j.DrainAndStop(50 * time.Millisecond) // idempotent: must not double-drop

	if n := queuedDrops.Load(); n != 1 {
		t.Fatalf("never-started drop hook fired %d time(s), want exactly 1", n)
	}
	if n := ranDrops.Load(); n != 0 {
		t.Fatalf("a job that RAN had its drop hook fired %d time(s); its own deferred cleanup owns that, so this is a double free", n)
	}
}

// TestJobsUncappedNeverQueuesBehindCapped: the exemption must not be undone by
// FIFO position. With one execution slot held by a capped job and more capped
// jobs waiting ahead of it in arrival order, an uncapped job admitted LAST must
// still run immediately — otherwise media dispatches would stall behind the
// text lane, which is the exact coupling the exemption removes.
func TestJobsUncappedNeverQueuesBehindCapped(t *testing.T) {
	j := newJobs(time.Hour, time.Now, time.Hour, 1)
	defer j.DrainAndStop(time.Second)

	release := make(chan struct{})
	block := func(ctx context.Context) (json.RawMessage, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return json.RawMessage(`1`), nil
	}
	// Fill the single capped slot, then pile three more capped jobs behind it.
	for _, id := range []string{"cap-0", "cap-1", "cap-2", "cap-3"} {
		if !j.Admit(id, AcceptSpec{}, block) {
			t.Fatalf("Admit(%s) must create", id)
		}
	}
	waitCount(t, 3, func() int { q, _ := j.Counts(); return q })

	// Admitted LAST, behind three blocked capped jobs.
	uncappedRunning := make(chan struct{})
	if !j.Admit("media-late", AcceptSpec{Uncapped: true}, func(ctx context.Context) (json.RawMessage, error) {
		close(uncappedRunning)
		<-ctx.Done()
		return nil, errors.New("cancelled")
	}) {
		t.Fatal("Admit(media-late) must create")
	}
	select {
	case <-uncappedRunning:
	case <-time.After(5 * time.Second):
		q, r := j.Counts()
		t.Fatalf("the uncapped job never started (queued %d, running %d) — it is stuck behind the capped lane's FIFO position", q, r)
	}
	// And it did NOT consume the capped lane's slot: still exactly one capped
	// job running.
	if _, running := j.Counts(); running != 2 { // cap-0 + media-late
		t.Fatalf("running = %d, want 2 (one capped + the uncapped one)", running)
	}
	close(release)
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
