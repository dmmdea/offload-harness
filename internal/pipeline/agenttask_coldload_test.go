package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/seatrate"
)

// Cold-load hold (0.140.0). On 2026-09-23 an offload_review_diff on the
// reference workstation's 3-card seat deferred twice with "stalled: no progress for 60s in prefill":
// step 1 answered, another model's swap evicted the seat, and the re-issue's
// request sat in llama-swap while the seat reloaded (~180 s) — the prefill
// clock ran the whole time. These tests script that shape on a fake
// llama-swap: /running reads `ready` at admission, then `starting` from the
// moment the loop's request arrives until the load finishes.

// compressColdLoad sets the cold-load knobs for one test and returns the
// restore. Production: poll 5 s, ceiling floor 10 min.
func compressColdLoad(t *testing.T, poll, ceiling time.Duration) func() {
	t.Helper()
	p, c := coldLoadPoll, coldLoadCeilingFloor
	coldLoadPoll, coldLoadCeilingFloor = poll, ceiling
	return func() { coldLoadPoll, coldLoadCeilingFloor = p, c }
}

// coldLoadTestPipeline is agentTestPipeline on a private state root whose
// seat-rates.json gives the seat a very fast measured prefill, so the prefill
// allowance is the (compressed) floor and a load visibly outlasts it.
func coldLoadTestPipeline(t *testing.T, base string) *Pipeline {
	t.Helper()
	dir := t.TempDir()
	rates := `{"seats":{"` + agentTestSeat + `":{"tok_s":100,"prefill_tok_s":1000000,"samples":5}}}`
	if err := os.WriteFile(filepath.Join(dir, seatrate.FileName), []byte(rates), 0o644); err != nil {
		t.Fatal(err)
	}
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

// loadingSeatFake is a llama-swap whose seat is ready until the first loop
// request arrives, then `starting` for `load` (or forever when load < 0)
// while that request is held, then ready and answered.
func loadingSeatFake(load time.Duration) *agentFake {
	var loadingUntil atomic.Int64 // unix nanos; 0 = not loading
	return &agentFake{
		rosterIDs: []string{agentTestSeat},
		running: func(int64) string {
			if u := loadingUntil.Load(); u != 0 && time.Now().UnixNano() < u {
				return `{"running":[{"model":"` + agentTestSeat + `","state":"starting","cmd":"x"}]}`
			}
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x"}]}`
		},
		loop: func(n int64) string {
			if n == 1 {
				hold := load
				if hold < 0 {
					hold = 8 * time.Second // "forever" for the test: far past its ceiling
				}
				loadingUntil.Store(time.Now().Add(hold).UnixNano())
				time.Sleep(hold)
			}
			return doneChat("answered once the seat had loaded")
		},
		repack: func(int64) string { return `{"answer":"42"}` },
	}
}

// A seat that loads for 1.5 s while the prefill allowance is 200 ms must NOT
// produce a prefill-stall defer: the stall clock does not run while the seat
// reads `starting`, and starts once it reads ready.
func TestRunAgentTaskColdLoadIsNotAPrefillStall(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	fake := loadingSeatFake(1500 * time.Millisecond)
	srv := fake.server(t)
	defer srv.Close()

	res := coldLoadTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if wire.Deferred {
		t.Fatalf("a seat that was LOADING was filed as a stall: %s / %q", wire.DeferClass, wire.Reason)
	}
	if fake.loopCalls.Load() < 1 {
		t.Fatal("the loop never reached the seat")
	}
}

// A seat stuck in `starting` past the cold-load ceiling defers — bounded,
// never open-ended — with a cold-load reason, as infrastructure (look at the
// seat), never as a prefill stall.
func TestRunAgentTaskSeatStuckStartingDefersWithAColdLoadReason(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 700*time.Millisecond)()
	fake := loadingSeatFake(-1)
	srv := fake.server(t)
	defer srv.Close()

	start := time.Now()
	res := coldLoadTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if !wire.Deferred || !strings.HasPrefix(wire.Reason, "stalled: seat still loading after ") || !strings.Contains(wire.Reason, "in cold-load") {
		t.Fatalf("want a cold-load defer, got deferred=%v reason=%q", wire.Deferred, wire.Reason)
	}
	if !strings.Contains(wire.Reason, `"starting"`) || !strings.Contains(wire.Reason, "cold-load ceiling") {
		t.Fatalf("the reason must name the seat state and the ceiling: %q", wire.Reason)
	}
	if wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("defer_class = %q, want %q", wire.DeferClass, core.DeferClassInfrastructure)
	}
	t.Logf("reason: %s", wire.Reason)
	if el := time.Since(start); el > 6*time.Second {
		t.Fatalf("the cold-load ceiling (700ms) did not bound the wait: %s", el)
	}
}

// A seat that reads READY and still sends nothing is a real prefill stall —
// the cold-load hold must not swallow it.
func TestRunAgentTaskReadySeatSilentIsStillAPrefillStall(t *testing.T) {
	defer compressLiveness(t, 200*time.Millisecond, 100*time.Millisecond, core.AgentCeilingSecCap)()
	defer compressColdLoad(t, 50*time.Millisecond, 5*time.Second)()
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		running: func(int64) string {
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"x"}]}`
		},
		loop: func(int64) string {
			time.Sleep(8 * time.Second)
			return doneChat("never read")
		},
		repack: func(int64) string { return `{"answer":"x"}` },
	}
	srv := fake.server(t)
	defer srv.Close()

	start := time.Now()
	res := coldLoadTestPipeline(t, srv.URL).Run(context.Background(), agentTestRequest(t, testContract()))
	wire := decodeWire(t, res)
	if !wire.Deferred || !strings.HasPrefix(wire.Reason, "stalled: no progress for ") || !strings.Contains(wire.Reason, "in prefill (allowed ") {
		t.Fatalf("want a prefill stall, got deferred=%v reason=%q", wire.Deferred, wire.Reason)
	}
	if wire.DeferClass != core.DeferClassInfrastructure {
		t.Fatalf("defer_class = %q", wire.DeferClass)
	}
	if el := time.Since(start); el > 4*time.Second {
		t.Fatalf("a ready seat's stall was held like a load: %s", el)
	}
}
