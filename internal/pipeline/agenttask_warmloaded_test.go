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
// seconds in the store. A non-200 answer that leaves the seat never listed loaded
// nothing at all, so it is not even an attempted load (register C-76).
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
			name: "a non-200 answer and the seat is never listed: nothing was loaded, so nothing was attempted",
			fake: &agentFake{rosterIDs: []string{agentTestSeat},
				running: func(int64) string { return `{"running":[]}` }},
			budget: 30 * time.Second, wantTried: false, wantLoaded: false,
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

// A warm-up whose seat start FAILED loaded nothing, so it is not a cold load: the
// store hears nothing from it (and, since C-76, the coherence probe is not asked
// either — nothing was loaded for this run). Five failed starts must not push a
// real load out of the window of five. This is the arm where the failed start is not
// even an attempt (a non-200 answer, the seat never listed); the arms where a load WAS
// attempted and not confirmed are the two tests after it.
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

// A load that OUTLASTS the admission budget is attempted and not confirmed: the warm-up
// waited its whole budget on a seat that was still loading, which is the cost of a wait
// that failed and not the length of a load. Since a non-200 answer with a never-listed
// seat stopped being an attempt (C-76), this is one of the two arms of the run's
// `if warmLoaded` guard that only a run-level test reaches.
func TestAWarmUpWhoseLoadOutlastedTheBudgetIsNotRecordedAsAColdLoad(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running:   func(int64) string { return `{"running":[]}` }, // never listed: nothing is resident while the load runs
		upstreamModels: func(n int64) string {
			if n == 1 { // the warm-up's request; the probes after it are answered at once
				time.Sleep(admissionPoll + 3*time.Second) // the load outlasts the 5 s admission budget
			}
			return `{"object":"list","data":[{"id":"` + agentTestSeat + `"}]}`
		},
		loop: func(int64) string { return doneChat("answered") },
	}
	srv := fake.server(t)
	defer srv.Close()
	dir := sharedStateDir(t)
	cfg := config.Config{Endpoint: srv.URL, Model: "workhorse", AgentModel: agentTestSeat, FleetNodeID: "node-t", Temperature: 0.1,
		StateDir: dir, AgentAdmissionWaitSec: 5}
	p := New(cfg, llamaclient.New(srv.URL, "", cfg.Model, 30*time.Second), nil, nil)
	contract := testContract()
	contract.OutputSchema = nil
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
	if !strings.Contains(wire.AdmissionNote, "cold load exceeded the admission budget") {
		t.Fatalf("admission_note = %q: the warm-up did not wait out its budget on the load (premise)", wire.AdmissionNote)
	}
	if got := storedSeat(t, dir); len(got.ColdLoads) != 0 || got.ColdLoadSec != 0 {
		t.Fatalf("cold_loads=%v cold_load_sec=%.1f: a load that outlasted the admission budget was recorded as a cold load", got.ColdLoads, got.ColdLoadSec)
	}
}

// The other arm: the warm-up's own request fails (llama-swap drops the connection under
// it) after the seat was asked to load. Attempted, not confirmed, and so not a cold load.
func TestAWarmRequestThatFailedIsNotRecordedAsAColdLoad(t *testing.T) {
	defer compressLiveness(t, 5*time.Second, 100*time.Millisecond, core.AgentCeilingSecCap)()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running:   func(int64) string { return `{"running":[]}` }, // never listed
		loop:      func(int64) string { return doneChat("answered") },
	}
	inner := fake.server(t)
	defer inner.Close()
	var passthroughs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/upstream/") && strings.HasSuffix(r.URL.Path, "/v1/models") {
			// Every request on the warm-up's route is dropped, not only the first: the HTTP
			// client replays an idempotent GET once after a dropped connection, and the replay
			// has to fail too for the warm-up to see its request fail.
			passthroughs.Add(1)
			time.Sleep(300 * time.Millisecond) // long enough that a recorded load would not round to nothing
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
				}
			}
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	dir := sharedStateDir(t)
	contract := testContract()
	contract.OutputSchema = nil
	wire := decodeWire(t, pipelineOn(t, srv.URL, dir).Run(context.Background(), agentTestRequest(t, contract)))
	if passthroughs.Load() == 0 || !strings.Contains(wire.AdmissionNote, "warm request failed") {
		t.Fatalf("passthrough requests=%d admission_note=%q: the warm-up's request did not fail (premise)", passthroughs.Load(), wire.AdmissionNote)
	}
	if got := storedSeat(t, dir); len(got.ColdLoads) != 0 || got.ColdLoadSec != 0 {
		t.Fatalf("cold_loads=%v cold_load_sec=%.1f: a warm request that failed was recorded as a cold load", got.ColdLoads, got.ColdLoadSec)
	}
}

// The pre-flight's budget ran out while the seat still read `starting`: the run saw
// the START of a load and never its end, so what it measured is a lower bound and
// not a load. Nothing may be recorded as one.
//
// The budget is 4 s, not the 3 s of one poll interval, on purpose. The pre-flight
// sleeps a poll only when the whole interval fits in what is LEFT of its budget, and
// what is left is the budget minus the microseconds the cordon and the run registry
// took before the pre-flight started. A budget of exactly one poll is therefore a
// coin flip on the clock: a coarse Windows clock reads those microseconds as zero
// and the pre-flight sleeps its poll, a nanosecond Linux clock charges them, leaves
// the pre-flight a few microseconds under 3 s, and it returns at once with nothing
// waited (the test failed on Linux CI while passing on Windows). 4 s fits one poll and
// never a second, so the pre-flight waits exactly 3 s on both.
// agenttask_windowprobe_test.go met the same coin flip.
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
		StateDir: dir, AgentAdmissionWaitSec: 4}
	p := New(cfg, llamaclient.New(srv.URL, "", cfg.Model, 30*time.Second), nil, nil)
	contract := testContract()
	contract.OutputSchema = nil
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
	if wire.AdmissionWaitSec < 2.9 || !strings.Contains(wire.AdmissionNote, "budget spent while "+agentTestSeat+":starting") {
		t.Fatalf("admission_wait_sec = %.1f, admission_note = %q: the pre-flight did not wait a poll interval and run out of its budget on the starting seat (premise)", wire.AdmissionWaitSec, wire.AdmissionNote)
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
