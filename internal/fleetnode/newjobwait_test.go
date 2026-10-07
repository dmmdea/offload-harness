package fleetnode

// new_job_wait_sec: the wait of a job submitted NOW (ADR 0073, the diagnosis' F10).
//
// queue_wait_estimate_sec is the wait of the deepest job ALREADY queued: excess x wall / workers. A delegator
// reading it as the wait of the job it is about to send under-prices it by one wall / workers, and at
// depth == max_concurrent_jobs it is 0 and omitted although a new job still waits for a worker to retire.
// The node now publishes the arrival's wait beside it, additive; the old field is untouched, so a
// delegator that predates the new one keeps working. A genuine 0 is published too (a worker is free): a
// delegator that finds the field absent prices the node from counters that include uncapped renders. internal/delegate pins the same figures on its
// side (startwait_test.go).

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestNewJobWaitSecIsOneSlotBeyondTheDeepestQueuedJob pins the node's arithmetic, row by row, against the
// older estimate it sits beside. The first row is the diagnosis' own example: 4 running + 3 queued behind
// a 300 s wall on 4 workers, where the deepest queued job waits 225 s and a new job 300 s.
func TestNewJobWaitSecIsOneSlotBeyondTheDeepestQueuedJob(t *testing.T) {
	for _, tc := range []struct {
		name              string
		depth, workers    int
		wall              float64
		wantDeepestQueued float64
		wantNewJob        float64
	}{
		{"4 running + 3 queued, 4 workers, 300 s", 7, 4, 300, 225, 300},
		{"every worker busy, nothing queued: the estimate was 0 and omitted", 4, 4, 300, 0, 75},
		{"a worker free", 3, 4, 300, 0, 0},
		{"one worker, two queued behind one running", 3, 1, 60, 120, 180},
		{"one worker, idle", 0, 1, 60, 0, 0},
		{"unlimited workers: nothing ever waits for one", 9, 0, 60, 0, 0},
		{"no recent wall: no claim", 7, 4, 0, 0, 0},
	} {
		if got := queueWaitEstimateSec(tc.depth, tc.workers, tc.wall); got != tc.wantDeepestQueued {
			t.Errorf("%s: queueWaitEstimateSec = %v, want %v (the old field's meaning is unchanged)", tc.name, got, tc.wantDeepestQueued)
		}
		if got := newJobWaitSec(tc.depth, tc.workers, tc.wall); got != tc.wantNewJob {
			t.Errorf("%s: newJobWaitSec = %v, want %v", tc.name, got, tc.wantNewJob)
		}
	}
}

// healthWithCappedBacklog builds a node with `workers` capped slots, one finished agent job of the given
// wall, and the given capped jobs running and queued, and returns its decoded /fleet/health.
func healthWithCappedBacklog(t *testing.T, workers, running, queued int, wall time.Duration) map[string]any {
	t.Helper()
	base := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	jobs := newJobs(time.Hour, func() time.Time { return base }, time.Hour, workers)
	t.Cleanup(func() { jobs.DrainAndStop(time.Second) })
	jobs.mu.Lock()
	jobs.m["done-agent"] = &job{state: JobDone, agent: true, startedAt: base, finishedAt: base.Add(wall)}
	for i := 0; i < running; i++ {
		jobs.m["running-"+string(rune('a'+i))] = &job{state: JobRunning, capped: true}
	}
	for i := 0; i < queued; i++ {
		jobs.m["queued-"+string(rune('a'+i))] = &job{state: JobAccepted, capped: true}
	}
	jobs.mu.Unlock()
	cfg := imageCfg()
	cfg.FleetMaxConcurrentJobs = workers
	cfg.FleetMaxQueueDepth = -1
	s := New(&fakeRunner{}, jobs, Options{NodeID: "testnode", Snapshot: goodSnapshot,
		Footprints: func() []FootprintEntry { return nil }, GpuVendor: "nvidia", GpuArch: "ampere", Cfg: cfg})
	return decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
}

// TestHealthPublishesTheNewJobWaitBesideTheQueueEstimate: the same states the existing estimate tests
// build, now with both numbers on the wire, one slot apart. 4 running + 2 queued, 4 workers, 60 s: the
// deepest queued job waits 30 s, a new job 45 s.
func TestHealthPublishesTheNewJobWaitBesideTheQueueEstimate(t *testing.T) {
	m := healthWithCappedBacklog(t, 4, 4, 2, 60*time.Second)
	if v, ok := m["queue_wait_estimate_sec"].(float64); !ok || v != 30 {
		t.Fatalf("queue_wait_estimate_sec = %v (%v), want the unchanged 30 (excess 2 x 60 s / 4)", m["queue_wait_estimate_sec"], ok)
	}
	if v, ok := m["new_job_wait_sec"].(float64); !ok || v != 45 {
		t.Fatalf("new_job_wait_sec = %v (%v), want 45 ((excess 2 + 1) x 60 s / 4)", m["new_job_wait_sec"], ok)
	}
}

// TestHealthPublishesTheNewJobWaitWhenEveryWorkerIsBusyAndNothingIsQueued: the discontinuity. The old
// estimate is absent at depth == workers (excess 0) while a new job waits for a worker to retire, so a
// delegator had to fall back to its own arithmetic there; the new field is continuous.
func TestHealthPublishesTheNewJobWaitWhenEveryWorkerIsBusyAndNothingIsQueued(t *testing.T) {
	m := healthWithCappedBacklog(t, 4, 4, 0, 60*time.Second)
	if _, ok := m["queue_wait_estimate_sec"]; ok {
		t.Fatalf("queue_wait_estimate_sec = %v with nothing queued: its meaning (the deepest QUEUED job) is unchanged, so it stays absent", m["queue_wait_estimate_sec"])
	}
	if v, ok := m["new_job_wait_sec"].(float64); !ok || v != 15 {
		t.Fatalf("new_job_wait_sec = %v (%v), want 15 (one slot: 60 s / 4 workers)", m["new_job_wait_sec"], ok)
	}
}

// TestHealthPublishesAZeroNewJobWaitWithAFreeWorker: "0 when a worker is free" is a number, and it is
// published. Hidden by omitempty (the field beside it is), it read as "no claim", and a delegator with no
// claim derives the wait from jobs_running + jobs_queued, which count uncapped renders too: an idle agent
// lane priced from a dozen renders. A node that can make no claim (no wall sample) still says nothing.
func TestHealthPublishesAZeroNewJobWaitWithAFreeWorker(t *testing.T) {
	m := healthWithCappedBacklog(t, 4, 3, 0, 60*time.Second)
	if v, ok := m["new_job_wait_sec"]; !ok || v != 0.0 {
		t.Fatalf("a node with a free worker and a wall sample published new_job_wait_sec = %v (present %v), want an explicit 0", v, ok)
	}
	body := do(t, healthServerOnly(t), http.MethodGet, "/fleet/health", "", nil).Body.String()
	if strings.Contains(body, "new_job_wait_sec") {
		t.Fatalf("a node with no wall sample at all published new_job_wait_sec: %s", body)
	}
}

// TestPublishedNewJobWaitSaysNothingOnlyWhenItCannotSayIt pins the rule that decides between an absent
// field and a zero: no ceiling or no wall sample is no claim; everything else, free worker included, is one.
func TestPublishedNewJobWaitSaysNothingOnlyWhenItCannotSayIt(t *testing.T) {
	for _, tc := range []struct {
		name           string
		depth, workers int
		wall           float64
		want           *float64
	}{
		{"unlimited workers: nothing ever waits for one", 9, 0, 60, nil},
		{"a negative ceiling is unlimited too", 9, -1, 60, nil},
		{"no wall sample: no claim", 7, 4, 0, nil},
		{"a worker free: an explicit zero", 3, 4, 300, ptrTo(0.0)},
		{"an idle node with a sample: an explicit zero", 0, 1, 60, ptrTo(0.0)},
		{"every worker busy, nothing queued: one slot", 4, 4, 300, ptrTo(75.0)},
		{"the diagnosis' example, rounded to the hundredth", 7, 4, 300.004, ptrTo(300.0)},
	} {
		got := publishedNewJobWait(tc.depth, tc.workers, tc.wall)
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("%s: published %v, want nothing", tc.name, *got)
		case tc.want != nil && (got == nil || *got != *tc.want):
			t.Errorf("%s: published %v, want %v", tc.name, got, *tc.want)
		}
	}
}

func ptrTo(v float64) *float64 { return &v }

// healthServerOnly is a node with no jobs at all: no wall sample, no claim.
func healthServerOnly(t *testing.T) *Server {
	t.Helper()
	jobs := NewJobs(time.Hour, 4)
	t.Cleanup(func() { jobs.DrainAndStop(time.Second) })
	cfg := imageCfg()
	cfg.FleetMaxConcurrentJobs = 4
	return New(&fakeRunner{}, jobs, Options{NodeID: "testnode", Snapshot: goodSnapshot,
		Footprints: func() []FootprintEntry { return nil }, GpuVendor: "nvidia", GpuArch: "ampere", Cfg: cfg})
}

// TestHealthIgnoresUncappedJobsInTheNewJobWait: like the estimate beside it, the arrival's wait counts the
// CAPPED backlog only. Ten uncapped renders never wait behind max_concurrent_jobs, and an agent lane whose
// slots are idle must not read as backlogged because of them.
func TestHealthIgnoresUncappedJobsInTheNewJobWait(t *testing.T) {
	base := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	jobs := newJobs(time.Hour, func() time.Time { return base }, time.Hour, 4)
	t.Cleanup(func() { jobs.DrainAndStop(time.Second) })
	jobs.mu.Lock()
	jobs.m["done-agent"] = &job{state: JobDone, agent: true, startedAt: base, finishedAt: base.Add(60 * time.Second)}
	for i := 0; i < 10; i++ {
		jobs.m["render-"+string(rune('a'+i))] = &job{state: JobRunning}
	}
	jobs.mu.Unlock()
	cfg := imageCfg()
	cfg.FleetMaxConcurrentJobs = 4
	s := New(&fakeRunner{}, jobs, Options{NodeID: "testnode", Snapshot: goodSnapshot,
		Footprints: func() []FootprintEntry { return nil }, GpuVendor: "nvidia", GpuArch: "ampere", Cfg: cfg})
	m := decodeMap(t, do(t, s, http.MethodGet, "/fleet/health", "", nil))
	if v, ok := m["new_job_wait_sec"]; !ok || v != 0.0 {
		t.Fatalf("new_job_wait_sec = %v (present %v) with 4 idle capped slots (10 running jobs are all UNCAPPED renders), want an explicit 0: absent, the delegator derives the wait from jobs_running = 10 against 4 workers", v, ok)
	}
	if v, _ := m["jobs_running"].(float64); v != 10 {
		t.Fatalf("jobs_running = %v, want the 10 renders this test counts: the fixture no longer exercises the counters a delegator would misread", v)
	}
}
