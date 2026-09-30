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

// seatCapFixture is a cap-1 box whose one slot is held by another registered
// run, with the given admission budget; the seat is not resident, and the fake
// answers any contract that gets in.
func seatCapFixture(t *testing.T, admissionSec int) (*Pipeline, *gpuactivity.Handle, *atomic.Int64, *agentFake) {
	t.Helper()
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
	t.Cleanup(first.End)
	loops := &atomic.Int64{}
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { loops.Add(1); return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		running:   func(int64) string { return `{"running":[]}` },
	}
	srv := fake.server(t)
	t.Cleanup(srv.Close)
	cfg := config.Config{
		Endpoint: srv.URL, Model: "workhorse", AgentModel: agentTestSeat, FleetNodeID: "node-t", Temperature: 0.1,
		AgentAdmissionWaitSec: admissionSec, StateDir: root, FleetMaxConcurrentJobs: 1,
	}
	return New(cfg, nil, nil, nil), first, loops, fake
}

// Register C-42 (W-09's gate) and C-60: cap 1, one run already registered on
// the seat. A second contract does not reach the engine until the first ends,
// and waits IN LINE for as long as its own wall — not the admission budget,
// which refused 88 cap waits after exactly 5m0s on 2026-09-29 — and when no
// slot frees inside its wall it defers as capacity, re-placeable, without one
// chat request.
func TestAContractWaitsAtTheSeatCapForItsWallAndDefersWhenNoSlotFrees(t *testing.T) {
	p, _, loops, _ := seatCapFixture(t, 1)
	contract := testContract()
	contract.TimeoutSec = 3 // the wall: 3x the 1 s admission budget
	start := time.Now()
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
	spent := time.Since(start)
	if !wire.Deferred || wire.DeferClass != core.DeferClassCapacity || !strings.Contains(wire.Reason, "seat busy") || !strings.Contains(wire.Reason, "cap 1") {
		t.Fatalf("a full seat must defer capacity naming the cap; got deferred=%v class=%q reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if loops.Load() != 0 {
		t.Fatalf("no chat request may reach the engine while the seat is at its cap (got %d)", loops.Load())
	}
	if spent < 2500*time.Millisecond || spent > 10*time.Second {
		t.Fatalf("the wait in line must be the run's wall (3 s), not the 1 s admission budget: %s", spent)
	}
}

// The slot frees after twice the admission budget: the contract that waited in
// line is admitted and runs — and the pre-flight after the cap still has its
// budget (the admission deadline moved out by the time spent in line).
func TestAContractThatWaitedPastTheAdmissionBudgetAtTheSeatCapRuns(t *testing.T) {
	p, first, loops, _ := seatCapFixture(t, 1)
	go func() {
		time.Sleep(2 * time.Second)
		first.End()
	}()
	contract := testContract()
	contract.TimeoutSec = 30
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred {
		t.Fatalf("a run whose slot freed inside its wall must be admitted: %s (%s)", wire.Reason, wire.DeferClass)
	}
	if loops.Load() == 0 {
		t.Fatal("the admitted contract never reached the engine")
	}
	if wire.AdmissionWaitSec < 1.5 {
		t.Fatalf("admission_wait_sec = %.2f: the time spent in line must still be reported as admission", wire.AdmissionWaitSec)
	}
}

// The time spent in line is not charged to the admission budget: after a cap
// wait longer than that budget, the cold-load warm-up still runs (on a spent
// budget it is skipped and "a cold seat will load inside the wall").
func TestTheWarmUpKeepsItsBudgetAfterAWaitInLine(t *testing.T) {
	p, first, _, fake := seatCapFixture(t, 4) // the warm-up needs >= one 3 s admission poll left
	go func() {
		time.Sleep(4500 * time.Millisecond) // longer than the whole admission budget
		first.End()
	}()
	contract := testContract()
	contract.TimeoutSec = 30
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
	if wire.Deferred {
		t.Fatalf("deferred: %s (%s)", wire.Reason, wire.DeferClass)
	}
	if fake.upstreamCNT.Load() == 0 {
		t.Fatal("the warm-up never ran: the wait in line spent the admission budget the cold load needs")
	}
}
