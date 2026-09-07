package fleetnode

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// health decodes /fleet/health for the assertions below.
func healthOf(t *testing.T, s *Server) struct {
	Lease *struct {
		Held         bool   `json:"held"`
		Class        string `json:"class"`
		Busy         bool   `json:"busy"`
		RemainingSec int    `json:"remaining_sec"`
	} `json:"lease"`
	Saturation *struct {
		Score    float64 `json:"score"`
		High     bool    `json:"high"`
		IdleSlot bool    `json:"idle_slot"`
	} `json:"saturation"`
	JobsRunning int `json:"jobs_running"`
} {
	t.Helper()
	rec := do(t, s, http.MethodGet, "/fleet/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health %d", rec.Code)
	}
	var out struct {
		Lease *struct {
			Held         bool   `json:"held"`
			Class        string `json:"class"`
			Busy         bool   `json:"busy"`
			RemainingSec int    `json:"remaining_sec"`
		} `json:"lease"`
		Saturation *struct {
			Score    float64 `json:"score"`
			High     bool    `json:"high"`
			IdleSlot bool    `json:"idle_slot"`
		} `json:"saturation"`
		JobsRunning int `json:"jobs_running"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A LONG lease of ANY class makes the node say so: busy on the lease block,
// high saturation, and NO idle slot. A SHORT one does not — a render arbitrated
// on the node must never refuse fleet work (the whole point of the duration
// rule, 2026-09-07 audit).
func TestHealthLongLeaseOfAnyClassRefusesAndClearsIdleSlot(t *testing.T) {
	cases := []struct {
		name      string
		class     gpulease.Class
		remaining time.Duration
		wantBusy  bool
	}{
		{"long media (a render, a training run)", gpulease.ClassMedia, 6 * time.Hour, true},
		{"long text (a measurement window)", gpulease.ClassText, 2 * time.Hour, true},
		{"short media (an ordinary image render)", gpulease.ClassMedia, 20 * time.Second, false},
		{"just under the threshold", gpulease.ClassMedia, 119 * time.Second, false},
		{"just over the threshold", gpulease.ClassMedia, 121 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{}
			s, _ := newTestServer(t, cfg, &fakeRunner{}, &Options{
				NodeID:     "testnode",
				Snapshot:   goodSnapshot,
				Footprints: func() []FootprintEntry { return nil },
				Lease: func() gpulease.Info {
					return gpulease.Info{Held: true, Class: tc.class, PID: 42, ExpiresAt: time.Now().Add(tc.remaining)}
				},
			})
			h := healthOf(t, s)
			if h.Lease == nil || !h.Lease.Held {
				t.Fatalf("lease block = %+v", h.Lease)
			}
			if h.Lease.Busy != tc.wantBusy {
				t.Fatalf("lease.busy = %v want %v (remaining %s, class %s)", h.Lease.Busy, tc.wantBusy, tc.remaining, tc.class)
			}
			if h.Lease.RemainingSec <= 0 {
				t.Fatalf("remaining_sec = %d, want the declared window", h.Lease.RemainingSec)
			}
			// A text lease refused before this change too; the new rule is the
			// media one. Either way, refusing and idle_slot must AGREE.
			refusing := tc.wantBusy || tc.class == gpulease.ClassText
			if h.Saturation == nil {
				t.Fatal("no saturation block")
			}
			if h.Saturation.High != refusing {
				t.Fatalf("saturation.high = %v want %v", h.Saturation.High, refusing)
			}
			if h.Saturation.IdleSlot == refusing {
				t.Fatalf("idle_slot = %v while refusing = %v — a node that would turn work away must not advertise a free slot", h.Saturation.IdleSlot, refusing)
			}
		})
	}
}

// fleet_busy_lease_sec is honoured, and a NEGATIVE value restores the
// pre-0.113.27 text-only behaviour for an operator who wants it back.
func TestBusyLeaseThresholdIsConfigurable(t *testing.T) {
	for _, tc := range []struct {
		cfgSec  int
		wantOn  bool
		wantThr time.Duration
	}{
		{0, true, FleetBusyLeaseSecDefault * time.Second},
		{600, true, 600 * time.Second},
		{-1, false, 0},
	} {
		thr, on := busyLeaseThreshold(tc.cfgSec)
		if on != tc.wantOn || (on && thr != tc.wantThr) {
			t.Fatalf("busyLeaseThreshold(%d) = %s,%v want %s,%v", tc.cfgSec, thr, on, tc.wantThr, tc.wantOn)
		}
	}
	// off: a six-hour MEDIA lease no longer marks the node busy
	h := leaseHealthOf(gpulease.Info{Held: true, Class: gpulease.ClassMedia, ExpiresAt: time.Now().Add(6 * time.Hour)}, time.Now(), -1)
	if h.Busy {
		t.Fatal("a negative fleet_busy_lease_sec must disable the duration rule")
	}
	if h.RemainingSec < int((6*time.Hour - time.Minute).Seconds()) {
		t.Fatalf("remaining_sec still reported when the rule is off: %d", h.RemainingSec)
	}
}

// The concurrency score must be computed from the CAPPED running set, the same
// set idle_slot compares against. Before the fix a node executing uncapped work
// could publish score 1.0 and idle_slot true in one payload.
func TestSaturationScoreUsesCappedRunning(t *testing.T) {
	// 3 running, of which 1 is capped, cap = 1.
	sat := saturationOf(0 /*queued*/, 3 /*running*/, 1 /*runningCapped*/, 1 /*maxConcurrent*/, 0, false, false)
	if sat.Score != 1 {
		t.Fatalf("score = %v want 1 (1 capped job against a cap of 1)", sat.Score)
	}
	// 3 running, NONE capped: the cap is untouched, so the node is not saturated.
	sat = saturationOf(0, 3, 0, 1, 0, false, true)
	if sat.Score != 0 {
		t.Fatalf("score = %v want 0 — three uncapped jobs do not fill a concurrency cap they are exempt from", sat.Score)
	}
	if !sat.IdleSlot {
		t.Fatal("idle_slot must survive: the capped slot really is free")
	}
	// The depth term still counts EVERY admitted job.
	sat = saturationOf(1, 3, 0, 8, 4, false, true)
	if sat.Score != 1 || !sat.High {
		t.Fatalf("depth term = %+v: max_queue_depth bounds all admitted jobs, capped or not", sat)
	}
}

// The wiring, not just the arithmetic: with an UNCAPPED job executing, health
// must publish score 0 against the cap AND an idle slot. Before the fix the
// handler fed saturationOf the all-jobs count while feeding it the capped-only
// IdleSlot, so this exact payload read as fully saturated and free at once.
func TestHealthScoreAndIdleSlotAgreeWithAnUncappedJobRunning(t *testing.T) {
	cfg := config.Config{FleetMaxConcurrentJobs: 1}
	s, jobs := newTestServer(t, cfg, &fakeRunner{}, nil)

	block := make(chan struct{})
	defer close(block)
	started := make(chan struct{})
	// An uncapped job (a render): exempt from the cap by construction.
	if !jobs.Admit("render-1", AcceptSpec{Uncapped: true, Task: "image-gen"}, func(ctx context.Context) (json.RawMessage, error) {
		close(started)
		<-block
		return nil, nil
	}) {
		t.Fatal("admit failed")
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the uncapped job never started")
	}

	h := healthOf(t, s)
	if h.JobsRunning != 1 {
		t.Fatalf("jobs_running = %d, want the executing job reported honestly", h.JobsRunning)
	}
	if h.Saturation == nil {
		t.Fatal("no saturation block")
	}
	// The CONCURRENCY term must contribute nothing: the job is exempt from the
	// cap. What remains is the depth term (1 admitted job of max_queue_depth),
	// which legitimately counts every admitted job. Before the fix the
	// concurrency term read 1/1 and the score was a flat 1.0 — saturated and
	// idle in the same payload.
	if h.Saturation.Score >= 1 {
		t.Fatalf("score = %v with only an UNCAPPED job running against a cap of 1 — the cap it is exempt from cannot be full", h.Saturation.Score)
	}
	if !h.Saturation.IdleSlot {
		t.Fatal("idle_slot false while the single capped slot is genuinely free")
	}
	if h.Saturation.High {
		t.Fatal("high true: nothing is refusing and the depth is nowhere near the limit")
	}

	// And the counterpart: a CAPPED job on the same node does fill the cap, so
	// score 1.0 and no idle slot — the two agree in that direction too.
	startedCapped := make(chan struct{})
	if !jobs.Admit("agent-1", AcceptSpec{Task: "agent"}, func(ctx context.Context) (json.RawMessage, error) {
		close(startedCapped)
		<-block
		return nil, nil
	}) {
		t.Fatal("admit failed")
	}
	select {
	case <-startedCapped:
	case <-time.After(5 * time.Second):
		t.Fatal("the capped job never started")
	}
	h = healthOf(t, s)
	if h.Saturation.Score != 1 {
		t.Fatalf("score = %v with the single capped slot taken, want 1", h.Saturation.Score)
	}
	if h.Saturation.IdleSlot {
		t.Fatal("idle_slot true while the only capped slot is occupied")
	}
}

// animate runs a ComfyUI render under the media lease and never touches the
// shared text endpoint the cap protects, so it must not burn a fleet slot.
func TestAnimateIsExemptFromTheConcurrencyCap(t *testing.T) {
	s, _ := newTestServer(t, config.Config{}, &fakeRunner{}, nil)
	for _, task := range []string{"image-gen", "video-gen", "animate", "audio-gen", "run-graph", "stt"} {
		if s.concurrencyCapped(task) {
			t.Errorf("%s is capped: it takes the media lease and would hold a fleet slot while parked in the single media slot", task)
		}
	}
	for _, task := range []string{"agent", "summarize"} {
		if !s.concurrencyCapped(task) {
			t.Errorf("%s must stay capped — it uses the shared text endpoint the cap exists to protect", task)
		}
	}
}
