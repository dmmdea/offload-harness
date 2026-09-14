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

// A run that arrives while a text holder is DRAINING the seat holds at the
// cordon for the admission budget — ONE budget, shared with the pre-flight and
// the warm-up, reported as admission time — and defers `capacity` naming the
// holder. It never reaches the loop, and the wait is not doubled (reviewer
// finding, 0.117.0: the cordon had its own full window on top of the
// pre-flight's).
func TestCordonWaitIsChargedToTheAdmissionBudget(t *testing.T) {
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
		AgentAdmissionWaitSec: 1, StateDir: root,
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
	for _, want := range []string{"gpu busy", "draining the seat", "arm B drain", "running work is not interrupted"} {
		if !strings.Contains(wire.Reason, want) {
			t.Errorf("reason must carry %q: %s", want, wire.Reason)
		}
	}
	if spent < 900*time.Millisecond || spent > 2500*time.Millisecond {
		t.Fatalf("the cordon must hold for ONE admission budget (1 s), not the wall and not two budgets: spent %s", spent)
	}
	if wire.AdmissionWaitSec < 0.9 {
		t.Errorf("the cordon wait must be reported as admission time, got %v", wire.AdmissionWaitSec)
	}
	if loops.Load() != 0 {
		t.Errorf("the loop must never run behind the cordon; ran %d time(s)", loops.Load())
	}
}
