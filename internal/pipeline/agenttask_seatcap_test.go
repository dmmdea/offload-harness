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
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// Register C-42 (W-09's gate): cap 1, one run already registered on the seat
// → a second contract does not reach the engine until the first ends; when
// the first never ends inside the admission budget the second defers as
// capacity, re-placeable, without one chat request.
func TestAContractWaitsAtTheSeatCapAndDefersWhenNoSlotFrees(t *testing.T) {
	root := t.TempDir()
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = modelaffinity.SetGPULease("", filepath.Join(t.TempDir(), "My Drive", "x")) })
	reg, err := gpuactivity.Open("", root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := reg.Begin(gpuactivity.Run{Seat: agentTestSeat, Kind: "contract"})
	if err != nil {
		t.Fatal(err)
	}
	defer first.End()

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
		AgentAdmissionWaitSec: 2, StateDir: root, FleetMaxConcurrentJobs: 1,
	}
	p := New(cfg, nil, nil, nil)
	contract := testContract()
	contract.TimeoutSec = 30
	start := time.Now()
	res := p.Run(context.Background(), agentTestRequest(t, contract))
	spent := time.Since(start)
	wire := decodeWire(t, res)
	if !wire.Deferred || wire.DeferClass != core.DeferClassCapacity || !strings.Contains(wire.Reason, "seat busy") || !strings.Contains(wire.Reason, "cap 1") {
		t.Fatalf("a full seat must defer capacity naming the cap; got deferred=%v class=%q reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if loops.Load() != 0 {
		t.Fatalf("no chat request may reach the engine while the seat is at its cap (got %d)", loops.Load())
	}
	if spent < 1500*time.Millisecond || spent > 10*time.Second {
		t.Fatalf("the wait must be the admission budget, not the wall: %s", spent)
	}
	// The slot frees: the same contract runs.
	first.End()
	loops.Store(0)
	res = p.Run(context.Background(), agentTestRequest(t, contract))
	wire = decodeWire(t, res)
	if wire.Deferred && strings.Contains(wire.Reason, "seat busy") {
		t.Fatalf("with the slot free the contract must be admitted, got %q", wire.Reason)
	}
}
