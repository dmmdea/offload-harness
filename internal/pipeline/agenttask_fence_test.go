// agenttask_fence_test.go pins the delegation door's behaviour under a GPU lease
// this process does not hold (register S-26 / W-03).
//
// The defect: the door held at the affinity cordon for the WHOLE admission budget
// (300 s by default) and only then deferred `capacity`. The verdict was on disk at
// the first read — an exclusive or draining lease refuses a new run for as long as
// it is held, and nothing the door can do inside its budget changes that. 47 live
// rows paid 300 s each for an answer that was already known, while the delegator
// sat on the re-placement path that answer exists to trigger.
//
// delegate.ForeignFence is the same read the review lane has made since 0.125.0
// (register D-110); this wires it into the door that pays for it most. An
// INHERITED lease (GPU_LEASE_EPOCH) is deliberately NOT a fence: `gpu reserve …
// -- <session>` runs its own work on the cards it cleared, and ADR 0032's "a
// peer-held seat is waited for" still governs every hold that does not fence.

package pipeline

import (
	"context"
	"path/filepath"
	"strconv"
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

// fencedPipeline arms a machine-wide EXCLUSIVE text lease in a state dir private
// to the test, points the affinity gate at it, and returns a pipeline configured
// for that dir plus the held lease's epoch (what an INHERITED lease is proved by).
func fencedPipeline(t *testing.T, endpoint string, admissionSec int) (*Pipeline, uint64) {
	t.Helper()
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
	holder, err := m.TryAcquire(gpulease.ClassText, gpulease.Options{Reason: "5070 Ti bench", Exclusive: true, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Release() })
	cfg := config.Config{
		Endpoint: endpoint, Model: "workhorse", AgentModel: agentTestSeat,
		FleetNodeID: "node-t", Temperature: 0.1,
		AgentAdmissionWaitSec: admissionSec, StateDir: root, Home: t.TempDir(),
	}
	info := gpulease.InspectDir(mustLeaseDir(t, root))
	if !info.Held || !info.Exclusive {
		t.Fatalf("the test lease did not take: %+v", info)
	}
	return New(cfg, llamaclient.New(endpoint, "", cfg.Model, 30*time.Second), nil, nil), info.Epoch
}

func mustLeaseDir(t *testing.T, root string) string {
	t.Helper()
	dir, err := gpulease.LeaseDir("", root)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRunAgentContractDefersAtOnceUnderAForeignFence: an exclusive lease held by
// another process refuses this run for its whole declared window. The door must
// say so in milliseconds, name the fence and the holder, and class it `capacity`
// so the delegator re-places the contract on another node — not spend the
// admission budget re-reading a file whose answer cannot change.
func TestRunAgentContractDefersAtOnceUnderAForeignFence(t *testing.T) {
	var loops atomic.Int64
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { loops.Add(1); return doneChat("must never run") },
		repack:    func(int64) string { return `{"answer":"never"}` },
		running:   func(int64) string { return `{"running":[]}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	p, _ := fencedPipeline(t, srv.URL, 5)

	contract := testContract()
	contract.TimeoutSec = 30
	start := time.Now()
	wire, err := p.RunAgentContract(context.Background(), contract, AgentContractOptions{})
	spent := time.Since(start)
	if err != nil {
		t.Fatalf("RunAgentContract: %v", err)
	}
	if !wire.Deferred || wire.DeferClass != core.DeferClassCapacity {
		t.Fatalf("want a capacity defer under a foreign fence; got deferred=%v class=%q reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if spent > time.Second {
		t.Errorf("the fence's verdict is on disk before the dial: deferred after %s, want milliseconds", spent)
	}
	for _, want := range []string{"gpu busy", "exclusive text lease", "gpu lease class=text", `reason="5070 Ti bench"`} {
		if !strings.Contains(wire.Reason, want) {
			t.Errorf("reason must carry %q so the caller can decide whether to wait or re-place: %s", want, wire.Reason)
		}
	}
	if loops.Load() != 0 {
		t.Errorf("the loop must never run behind the fence; ran %d time(s)", loops.Load())
	}
	if fake.runningCNT.Load() != 0 {
		t.Errorf("a fenced seat must not be probed at all; %d /running reads", fake.runningCNT.Load())
	}
}

// TestRunAgentContractRunsUnderItsOwnInheritedLease: `gpu reserve --drain
// --unload-seat -- <session>` runs this process as the holder's child with
// GPU_LEASE_EPOCH set. The measured work the lease was taken FOR must keep
// running on the cards it cleared — routing it away would defeat the reservation.
func TestRunAgentContractRunsUnderItsOwnInheritedLease(t *testing.T) {
	var loops atomic.Int64
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { loops.Add(1); return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		running:   func(int64) string { return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	p, epoch := fencedPipeline(t, srv.URL, 5)
	t.Setenv("GPU_LEASE_EPOCH", strconv.FormatUint(epoch, 10))

	wire, err := p.RunAgentContract(context.Background(), testContract(), AgentContractOptions{})
	if err != nil {
		t.Fatalf("RunAgentContract: %v", err)
	}
	if wire.Deferred {
		t.Fatalf("an INHERITED lease is not a fence for its own holder; deferred: %s", wire.Reason)
	}
	if loops.Load() == 0 {
		t.Error("the holder's own contract must run on the cards its lease cleared")
	}
}
