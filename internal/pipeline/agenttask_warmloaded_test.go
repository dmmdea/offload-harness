package pipeline

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
)

// The warm-up's fourth answer (register C-66): the seat is CONFIRMED loaded. A load
// that was attempted and failed is the cost of a failed start, never a measurement
// of a cold load — during the 2026-09-29 outage llama-swap answered 500 to every
// start for 18 minutes and each admission ended with a "cold load" of a few
// seconds in the store.
func TestWarmSeatOutcomeSaysWhetherTheLoadWasConfirmed(t *testing.T) {
	listedReady := `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x"}]}`
	upstreamOK := func(int64) string { return `{"object":"list","data":[{"id":"` + agentTestSeat + `"}]}` }
	for _, tc := range []struct {
		name       string
		fake       *agentFake
		budget     time.Duration
		wantTried  bool
		wantLoaded bool
	}{
		{
			name: "the passthrough answered 200 and /running lists the seat ready",
			fake: &agentFake{rosterIDs: []string{agentTestSeat}, upstreamModels: upstreamOK,
				running: func(n int64) string {
					if n == 1 {
						return `{"running":[]}`
					}
					return listedReady
				}},
			budget: 30 * time.Second, wantTried: true, wantLoaded: true,
		},
		{
			name: "the passthrough answered 200 and the seat is listed under another id: a 200 IS the confirmation",
			fake: &agentFake{rosterIDs: []string{agentTestSeat}, upstreamModels: upstreamOK,
				running: func(int64) string { return `{"running":[]}` }},
			budget: 30 * time.Second, wantTried: true, wantLoaded: true,
		},
		{
			name: "a non-200 answer, then /running lists the seat ready",
			fake: &agentFake{rosterIDs: []string{agentTestSeat},
				running: func(n int64) string {
					if n == 1 {
						return `{"running":[]}`
					}
					return listedReady
				}},
			budget: 30 * time.Second, wantTried: true, wantLoaded: true,
		},
		{
			name: "a non-200 answer and the seat is never listed: a start that failed",
			fake: &agentFake{rosterIDs: []string{agentTestSeat},
				running: func(int64) string { return `{"running":[]}` }},
			budget: 30 * time.Second, wantTried: true, wantLoaded: false,
		},
		{
			name: "the load outlasts the admission budget: attempted, not confirmed",
			fake: &agentFake{rosterIDs: []string{agentTestSeat},
				running: func(int64) string { return `{"running":[]}` },
				upstreamModels: func(int64) string {
					time.Sleep(admissionPoll + time.Second)
					return `{"object":"list","data":[{"id":"` + agentTestSeat + `"}]}`
				}},
			budget: admissionPoll, wantTried: true, wantLoaded: false,
		},
		{
			name:   "the seat is already ready: nothing was loaded for this run",
			fake:   &agentFake{rosterIDs: []string{agentTestSeat}, running: func(int64) string { return listedReady }},
			budget: 30 * time.Second, wantTried: false, wantLoaded: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.fake.server(t)
			defer srv.Close()
			_, note, tried, loaded := warmSeatOutcome(context.Background(), srv.URL, agentTestSeat, tc.budget)
			if tried != tc.wantTried || loaded != tc.wantLoaded {
				t.Fatalf("attempted=%v loaded=%v (note %q), want attempted=%v loaded=%v", tried, loaded, note, tc.wantTried, tc.wantLoaded)
			}
		})
	}
}

// The passthrough request itself failing (the connection is dropped, llama-swap is
// down) is an attempted load and not a confirmed one.
func TestWarmSeatOutcomeARequestThatFailedIsNotALoad(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/running") {
			_, _ = io.WriteString(w, `{"running":[]}`)
			return
		}
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				_ = c.Close() // the connection drops under the warm-up's request
			}
		}
	}))
	defer srv.Close()
	_, note, tried, loaded := warmSeatOutcome(context.Background(), srv.URL, agentTestSeat, 30*time.Second)
	if !tried || loaded || !strings.Contains(note, "warm request failed") {
		t.Fatalf("attempted=%v loaded=%v note=%q, want an attempted load that was not confirmed", tried, loaded, note)
	}
}

// A warm-up whose seat start FAILED is attempted (the coherence probe still keys on
// that) and is not a cold load: the store hears nothing from it. Five failed
// starts must not push a real load out of the window of five.
func TestAWarmUpThatFailedIsNotRecordedAsAColdLoad(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running:   func(int64) string { return `{"running":[]}` }, // never listed: the start keeps failing
		loop:      func(int64) string { return doneChat("answered") },
	} // no upstreamModels: the warm-up passthrough answers 404
	srv := fake.server(t)
	defer srv.Close()
	dir := sharedStateDir(t)
	contract := testContract()
	contract.OutputSchema = nil
	_ = decodeWire(t, pipelineOn(t, srv.URL, dir).Run(context.Background(), agentTestRequest(t, contract)))
	if fake.upstreamCNT.Load() == 0 {
		t.Fatal("the warm-up never asked llama-swap to load the seat (premise)")
	}
	if got := storedSeat(t, dir); len(got.ColdLoads) != 0 || got.ColdLoadSec != 0 {
		t.Fatalf("cold_loads=%v cold_load_sec=%.1f: a start that failed was recorded as a cold load", got.ColdLoads, got.ColdLoadSec)
	}
}

// The pre-flight's budget ran out while the seat still read `starting`: the run saw
// the START of a load and never its end, so what it measured is a lower bound and
// not a load. Nothing may be recorded as one.
func TestAPreflightThatRanOutOfBudgetOnAStartingSeatRecordsNoColdLoad(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running: func(int64) string {
			return `{"running":[{"model":"` + agentTestSeat + `","state":"starting","cmd":"x"}]}` // a load that outlasts the budget
		},
		loop: func(int64) string { return doneChat("answered anyway") },
	}
	srv := fake.server(t)
	defer srv.Close()
	dir := sharedStateDir(t)
	cfg := config.Config{Endpoint: srv.URL, Model: "workhorse", AgentModel: agentTestSeat, FleetNodeID: "node-t", Temperature: 0.1,
		StateDir: dir, AgentAdmissionWaitSec: 3}
	p := New(cfg, llamaclient.New(srv.URL, "", cfg.Model, 30*time.Second), nil, nil)
	contract := testContract()
	contract.OutputSchema = nil
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
	if wire.AdmissionWaitSec < 2.9 {
		t.Fatalf("admission_wait_sec = %.1f: the pre-flight did not spend its 3 s budget on the starting seat (premise)", wire.AdmissionWaitSec)
	}
	if got := storedSeat(t, dir); len(got.ColdLoads) != 0 || got.ColdLoadSec != 0 {
		t.Fatalf("cold_loads=%v cold_load_sec=%.1f: a load that had not finished when the budget ran out was recorded as if it had", got.ColdLoads, got.ColdLoadSec)
	}
}

// Fan-out lands its subtasks on a cold seat together, and every one of them
// measures the SAME load in its warm-up. A load ends at one instant for everyone
// waiting on it: the end instant is what lets the store see ONE load.
// TestConcurrentRunsRecordOneColdLoad covers the pre-flight branch (a seat that
// reads `starting`); this one covers the warm-up branch.
func TestConcurrentWarmUpsOfOneLoadRecordOneColdLoad(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	var loaded atomic.Bool
	var readyAt atomic.Int64
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running: func(int64) string {
			if loaded.Load() {
				return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x"}]}`
			}
			return `{"running":[]}` // nothing listed and nothing loading: the warm-up's passthrough starts it
		},
		upstreamModels: func(int64) string {
			// Every warm-up joins ONE load, which ends at a single instant for all of them.
			readyAt.CompareAndSwap(0, time.Now().Add(1500*time.Millisecond).UnixNano())
			time.Sleep(time.Until(time.Unix(0, readyAt.Load())))
			loaded.Store(true)
			return `{"object":"list","data":[{"id":"` + agentTestSeat + `"}]}`
		},
		loop: func(int64) string { return doneChat("answered") },
	}
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
	if fake.upstreamCNT.Load() < 2 {
		t.Fatalf("passthrough GETs = %d: both runs must have gone through the warm-up branch (this test's premise)", fake.upstreamCNT.Load())
	}
	got := storedSeat(t, dir)
	if len(got.ColdLoads) != 1 || got.ColdLoadSec < 1.0 {
		t.Fatalf("cold_loads=%v cold_load_sec=%.1f: two runs whose warm-ups joined one load must leave ONE entry", got.ColdLoads, got.ColdLoadSec)
	}
}
