package pipeline

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// End-to-end pins for the load-honest arithmetic (ADR 0066, register C-66): a
// run that saw the seat shared never teaches the store its prefill rate, and a
// load a run waited out is recorded from the moment the run first saw it.

// sharedStateDir seeds a state root with a very fast measured prefill (so the
// allowance is the compressed floor) and returns it: several pipelines built on
// it share ONE run registry and ONE seat-rates store, which is what makes them
// see each other on the seat.
func sharedStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	rates := `{"seats":{"` + agentTestSeat + `":{"tok_s":100,"prefill_tok_s":1000000,"samples":5}}}`
	if err := os.WriteFile(filepath.Join(dir, seatrate.FileName), []byte(rates), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func pipelineOn(t *testing.T, base, dir string) *Pipeline {
	t.Helper()
	cfg := config.Config{
		Endpoint:    base,
		Model:       "workhorse",
		AgentModel:  agentTestSeat,
		FleetNodeID: "node-t",
		Temperature: 0.1,
		StateDir:    dir,
	}
	return New(cfg, llamaclient.New(base, "", cfg.Model, 30*time.Second), nil, nil)
}

func storedSeat(t *testing.T, dir string) seatrate.Seat {
	t.Helper()
	store, err := seatrate.Load(filepath.Join(dir, seatrate.FileName))
	if err != nil {
		raw, _ := os.ReadFile(filepath.Join(dir, seatrate.FileName))
		t.Fatalf("seat-rates store: %v (%s)", err, raw)
	}
	return store.Seats[agentTestSeat]
}

// Two runs on one seat time each other's prefill: neither saw the seat to itself,
// so neither may move prefill_tok_s. The control — the same run alone — records
// its sample, so the test can tell the gate from a dead observation path. No
// busy hold is involved (the allowance is longer than either wait): only the
// registry's word that the other run was on the seat.
func TestRunAgentTaskConcurrentRunsDoNotFeedThePrefillRate(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	slow := func(n int64, body map[string]any, w http.ResponseWriter, r *http.Request) {
		wait := 1500 * time.Millisecond // the first request in
		if n >= 2 {
			wait = 3 * time.Second // the second, still prefilling when the first one's first delta arrives
		}
		slowFirstDeltaLoop(wait)(n, body, w, r)
	}
	contract := testContract()
	contract.OutputSchema = nil

	t.Run("two runs on the seat: no sample is recorded", func(t *testing.T) {
		fake := &agentFake{rosterIDs: []string{agentTestSeat}, loopStream: slow}
		srv := fake.server(t)
		defer srv.Close()
		dir := sharedStateDir(t)
		var wg sync.WaitGroup
		wires := make([]core.AgentWireResult, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if i == 1 {
					time.Sleep(300 * time.Millisecond) // the second run joins while the first prefills
				}
				wires[i] = decodeWire(t, pipelineOn(t, srv.URL, dir).Run(context.Background(), agentTestRequest(t, contract)))
			}(i)
		}
		wg.Wait()
		for i, w := range wires {
			if w.Deferred {
				t.Fatalf("run %d deferred: %s (%s)", i, w.Reason, w.DeferClass)
			}
			if w.QueuedMs != 0 {
				t.Fatalf("run %d was held (queued_ms %d): the hold, not the registry, skipped its sample", i, w.QueuedMs)
			}
		}
		if got := storedSeat(t, dir).PrefillTokS; got != 1000000 {
			t.Fatalf("prefill_tok_s = %v: a run that shared the seat moved the published rate (want the stored 1000000)", got)
		}
	})

	t.Run("control: a run alone records its sample", func(t *testing.T) {
		fake := &agentFake{rosterIDs: []string{agentTestSeat}, loopStream: slow}
		srv := fake.server(t)
		defer srv.Close()
		dir := sharedStateDir(t)
		wire := decodeWire(t, pipelineOn(t, srv.URL, dir).Run(context.Background(), agentTestRequest(t, contract)))
		if wire.Deferred {
			t.Fatalf("deferred: %s", wire.Reason)
		}
		if got := storedSeat(t, dir).PrefillTokS; got == 1000000 {
			t.Fatalf("a solo run did not record its prefill sample (rate still %v): this test cannot tell the gate from a dead observation path", got)
		}
	})
}

// startingSeat is a llama-swap whose seat reads `starting` for `load` from the
// FIRST time anything asks, then ready: a load someone else's request began.
func startingSeat(load time.Duration) *agentFake {
	var readyAt atomic.Int64
	return &agentFake{
		rosterIDs: []string{agentTestSeat},
		running: func(int64) string {
			now := time.Now().UnixNano()
			readyAt.CompareAndSwap(0, now+int64(load))
			if now < readyAt.Load() {
				return `{"running":[{"model":"` + agentTestSeat + `","state":"starting","cmd":"x"}]}`
			}
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x"}]}`
		},
		loop: func(int64) string { return doneChat("answered once the seat had loaded") },
	}
}

// A run that arrives while the seat is loading waits it out in the admission
// pre-flight. That wait IS the cold load as far as the run saw it — from the
// first poll that found the seat starting to the seat reading ready — and it is
// recorded (0.144.0). Until then the pre-flight's wait was admission time and
// nothing else, so a seat whose loads other requests always start fed the
// store's cold-load figure nothing (agent-pool held 12.4 against real loads of
// 178-271 s).
func TestColdLoadSecondsAreRecordedFromTheRealStart(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	fake := startingSeat(2500 * time.Millisecond)
	srv := fake.server(t)
	defer srv.Close()
	dir := sharedStateDir(t)
	contract := testContract()
	contract.OutputSchema = nil

	wire := decodeWire(t, pipelineOn(t, srv.URL, dir).Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred {
		t.Fatalf("deferred: %s (%s)", wire.Reason, wire.DeferClass)
	}
	if wire.AdmissionWaitSec < 2.9 {
		t.Fatalf("admission_wait_sec = %.1f: the pre-flight did not wait for the load (this test's premise)", wire.AdmissionWaitSec)
	}
	got := storedSeat(t, dir)
	// The pre-flight polls every 3 s: the run saw the seat starting at t0 and ready at t0+3 s.
	if got.ColdLoadSec < 2.9 || got.ColdLoadSec > 6 || len(got.ColdLoads) != 1 {
		t.Fatalf("cold_load_sec=%.1f cold_loads=%v, want ONE load of ~3 s recorded from the first sighting", got.ColdLoadSec, got.ColdLoads)
	}
}

// The pre-flight saw the seat starting, the start then failed (llama-swap dropped
// the seat), and the run's own warm-up loaded it again in milliseconds: the load
// this run paid is measured from the FIRST sighting, not from the warm-up's own
// start — the warm-up alone would have recorded the last few milliseconds.
func TestColdLoadIncludesThePreflightWaitBeforeTheWarmUp(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	var polls atomic.Int64
	var loaded atomic.Bool
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running: func(n int64) string {
			polls.Add(1)
			switch {
			case n == 1:
				return `{"running":[{"model":"` + agentTestSeat + `","state":"starting","cmd":"x"}]}` // someone's load, seen first
			case loaded.Load():
				return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x"}]}`
			}
			return `{"running":[]}` // the start failed: nothing is listed, nothing is loading
		},
		upstreamModels: func(int64) string {
			loaded.Store(true) // the warm-up's passthrough loads the seat at once
			return `{"object":"list","data":[{"id":"` + agentTestSeat + `"}]}`
		},
		loop: func(int64) string { return doneChat("answered") },
	}
	srv := fake.server(t)
	defer srv.Close()
	dir := sharedStateDir(t)
	contract := testContract()
	contract.OutputSchema = nil

	wire := decodeWire(t, pipelineOn(t, srv.URL, dir).Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred {
		t.Fatalf("deferred: %s (%s)", wire.Reason, wire.DeferClass)
	}
	if fake.upstreamCNT.Load() == 0 {
		t.Fatal("the warm-up never loaded the seat: this test is not exercising the warm-up branch")
	}
	got := storedSeat(t, dir)
	if got.ColdLoadSec < 2.9 || got.ColdLoadSec > 6 {
		t.Fatalf("cold_load_sec = %.2f cold_loads=%v: want the ~3 s from the first sighting through the warm-up, not the warm-up's own milliseconds", got.ColdLoadSec, got.ColdLoads)
	}
}

// Several runs wait on one load and each of them saw it: the store holds ONE load
// (the longest view of it), never one entry per waiting run.
func TestConcurrentRunsRecordOneColdLoad(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	fake := startingSeat(2500 * time.Millisecond)
	srv := fake.server(t)
	defer srv.Close()
	dir := sharedStateDir(t)
	contract := testContract()
	contract.OutputSchema = nil

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := decodeWire(t, pipelineOn(t, srv.URL, dir).Run(context.Background(), agentTestRequest(t, contract))); w.Deferred {
				t.Errorf("deferred: %s", w.Reason)
			}
		}()
	}
	wg.Wait()
	got := storedSeat(t, dir)
	if len(got.ColdLoads) != 1 || got.ColdLoadSec < 2.9 {
		t.Fatalf("cold_loads=%v cold_load_sec=%.1f: two runs that waited on one load must leave ONE entry", got.ColdLoads, got.ColdLoadSec)
	}
}

// seatLoadOf counts the runs the registry lists on the seat that are past
// admission — the same rule the local run cap counts by — and this run itself.
func TestSeatLoadOfCountsTheRunsPastAdmission(t *testing.T) {
	reg := gpuactivity.OpenAt(filepath.Join(t.TempDir(), "activity"))
	begin := func(seat, phase string) *gpuactivity.Handle {
		h, err := reg.Begin(gpuactivity.Run{Seat: seat, Kind: "contract", Phase: phase})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.End)
		return h
	}
	self := begin("seat-a", gpuactivity.PhaseRunning)
	load := seatLoadOf(reg, "seat-a", self.ID())
	if load == nil {
		t.Fatal("no sampler for a registry and a seat")
	}
	if n := load(); n != 1 {
		t.Fatalf("load with nobody else on the seat = %d, want 1 (this run)", n)
	}
	begin("seat-a", gpuactivity.PhaseRunning)
	begin("seat-a", gpuactivity.PhaseRepack)
	begin("seat-a", gpuactivity.PhaseAdmission) // still admitting: not at the engine yet
	begin("seat-b", gpuactivity.PhaseRunning)   // another seat
	if n := load(); n != 3 {
		t.Fatalf("load = %d, want this run plus the two others past admission on the seat", n)
	}
	if seatLoadOf(nil, "seat-a", "x") != nil || seatLoadOf(reg, " ", "x") != nil {
		t.Fatal("no registry or no seat must mean no sampler")
	}
}
