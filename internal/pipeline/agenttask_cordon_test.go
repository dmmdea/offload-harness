package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// A run that arrives while a text holder is DRAINING the seat defers `capacity`
// naming the holder, never reaches the loop, and — since W-08/W-03 (register
// S-26) — does so in MILLISECONDS rather than at the end of the admission budget.
//
// This test used to pin the opposite: that the run held at the cordon for one
// whole admission budget and that the wait was charged to admission (0.117.0,
// reviewer finding — before that the cordon had its own full window on top of the
// pre-flight's). The budget bookkeeping it guarded is unchanged and still pinned
// by the admission suite; what changed is that a hold this process cannot outwait
// is no longer waited on at all. A draining or exclusive lease refuses a new run
// for as long as it is held, so polling it to the end of the budget produced the
// same `capacity` verdict 300 s later — 47 rows, 3.92 h, in three days — while
// the delegator sat on the re-placement path that verdict exists to trigger.
func TestADrainingHoldDefersAtTheFenceNotAtTheCordon(t *testing.T) {
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	// Disarm on the way out: a resolution failure disarms the gate, and a
	// cloud-synced root is exactly that failure.
	t.Cleanup(func() { _ = modelaffinity.SetGPULease("", filepath.Join(t.TempDir(), "My Drive", "x")) })
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "arm B drain", Draining: true, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	var loops atomic.Int64
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { loops.Add(1); return doneChat("must never run") },
		repack:    func(int64) string { return `{"answer":"never"}` },
		running:   func(int64) string { return `{"running":[]}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	cfg := config.Config{
		Endpoint: srv.URL, Model: "workhorse", AgentModel: agentTestSeat, FleetNodeID: "node-t", Temperature: 0.1,
		AgentAdmissionWaitSec: 5, StateDir: root,
	}
	p := New(cfg, llamaclient.New(srv.URL, "", cfg.Model, 30*time.Second), nil, nil)
	contract := testContract()
	contract.TimeoutSec = 30 // a wall the cordon must not touch
	start := time.Now()
	res := p.Run(context.Background(), agentTestRequest(t, contract))
	spent := time.Since(start)
	wire := decodeWire(t, res)
	if !wire.Deferred || wire.DeferClass != core.DeferClassCapacity {
		t.Fatalf("a run under a draining hold must defer capacity; got deferred=%v class=%q reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	// The class is what the delegator branches on; the reason is what a human
	// reads to decide whether to wait, and it must still name the fence AND the
	// holder's own line — its class, pid, declared reason and expiry.
	for _, want := range []string{"gpu busy", "draining text lease", "gpu lease class=text", `reason="arm B drain"`, "expires="} {
		if !strings.Contains(wire.Reason, want) {
			t.Errorf("reason must carry %q: %s", want, wire.Reason)
		}
	}
	if spent > time.Second {
		t.Fatalf("the fence's verdict was on disk before the first poll: deferred after %s, want milliseconds (the budget is 5 s)", spent)
	}
	if loops.Load() != 0 {
		t.Errorf("the loop must never run behind the cordon; ran %d time(s)", loops.Load())
	}
	if fake.runningCNT.Load() != 0 {
		t.Errorf("a fenced seat must not be probed, warmed or dialled; %d /running read(s)", fake.runningCNT.Load())
	}
}
