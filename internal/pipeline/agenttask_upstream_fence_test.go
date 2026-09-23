// agenttask_upstream_fence_test.go pins the delegation door against the
// 2026-09-22 defect: an agent run admitted JUST BEFORE a media lease kept sending
// llama-swap requests to /upstream/<seat>/{props,tokenize,v1/models}, and
// llama-swap answers that route by STARTING the seat. The 3-card agent seat was
// loaded onto the cards a video render held, mid-render (895 s against a normal
// 228-324 s). The cordon and the fence pre-check only see a lease that exists
// when the run is admitted; these tests take the lease AFTER admission, at the
// two moments the incident had: during admission, and mid-run.

package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// lateLease arms the gate at a private state root with NO lease held yet, and
// returns a func that takes a media lease there — called from inside the fake
// llama-swap, at the moment the test wants the render to start. It records how
// many /upstream requests the fake had seen at that instant.
type lateLease struct {
	m        *gpulease.Manager
	once     sync.Once
	taken    atomic.Bool
	atTaking atomic.Int64
}

func armLateLease(t *testing.T) (*lateLease, string) {
	t.Helper()
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = modelaffinity.SetGPULease("", filepath.Join(t.TempDir(), "My Drive", "x")) })
	return &lateLease{m: m}, root
}

func (l *lateLease) take(t *testing.T, fake *agentFake) {
	l.once.Do(func() {
		l.atTaking.Store(fake.upstreamAll.Load())
		lease, err := l.m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "video render", Origin: "pipeline", TTL: time.Hour})
		if err != nil {
			t.Errorf("acquire the render's lease: %v", err)
			return
		}
		l.taken.Store(true)
		t.Cleanup(func() { _ = lease.Release() })
	})
}

func lateLeasePipeline(t *testing.T, endpoint, root string, admissionSec int) *Pipeline {
	t.Helper()
	cfg := config.Config{
		Endpoint: endpoint, Model: "workhorse", AgentModel: agentTestSeat,
		FleetNodeID: "node-t", Temperature: 0.1,
		AgentAdmissionWaitSec: admissionSec, StateDir: root, Home: t.TempDir(),
	}
	return New(cfg, llamaclient.New(endpoint, "", cfg.Model, 30*time.Second), nil, nil)
}

// The render takes the card while the run is still being admitted (the first
// /running read of the pre-flight), over a cold seat. The warm-up and the
// served-window probe would each LOAD the seat; both must hold behind the fence,
// and the run must defer `capacity` naming the render — before any wall, with
// nothing loaded onto the held card.
func TestALeaseTakenDuringAdmissionLoadsNothingAndDefersCapacity(t *testing.T) {
	ll, root := armLateLease(t)
	var loops atomic.Int64
	fake := &agentFake{rosterIDs: []string{agentTestSeat}}
	fake.loop = func(int64) string { loops.Add(1); return doneChat("must never run") }
	fake.repack = func(int64) string { return `{"answer":"never"}` }
	fake.running = func(int64) string { ll.take(t, fake); return `{"running":[]}` }
	srv := fake.server(t)
	defer srv.Close()
	// 5 s: over one admission poll (3 s), so the warm-up runs — and holds.
	p := lateLeasePipeline(t, srv.URL, root, 5)

	contract := testContract()
	wire, err := p.RunAgentContract(context.Background(), contract, AgentContractOptions{})
	if err != nil {
		t.Fatalf("RunAgentContract: %v", err)
	}
	if !ll.taken.Load() {
		t.Fatal("the fake never took the lease: the test did not exercise a late lease")
	}
	if got := fake.upstreamAll.Load() - ll.atTaking.Load(); got != 0 {
		t.Fatalf("%d request(s) reached /upstream after the render took the card — each one starts the seat on it", got)
	}
	if !wire.Deferred || wire.DeferClass != core.DeferClassCapacity {
		t.Fatalf("want a capacity defer; got deferred=%v class=%q reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	for _, want := range []string{"gpu busy", "gpu-lease timeout", "video render"} {
		if !strings.Contains(wire.Reason, want) {
			t.Errorf("reason must carry %q: %s", want, wire.Reason)
		}
	}
	if loops.Load() != 0 {
		t.Errorf("the loop ran %d time(s) behind the fence", loops.Load())
	}
	// The warm-up held behind the fence, and that wait is admission time.
	if !strings.Contains(wire.AdmissionNote, "warm-up held behind the GPU lease") {
		t.Errorf("admission_note must say the warm-up held behind the lease: %q", wire.AdmissionNote)
	}
	if wire.AdmissionWaitSec < 4 {
		t.Errorf("admission_wait_sec = %v, want the ~5 s the warm-up spent behind the fence", wire.AdmissionWaitSec)
	}
}

// The render takes the card MID-RUN: after the first completion, with the seat
// then unloaded (the render's runner clears it). The run's next completion must
// not reach /upstream or load the seat; it waits for the card, the wait runs out,
// and the run defers `capacity` — not a stall blamed on the seat, and not a
// budget defer the delegator would size contracts from. (The tokenizer's own
// fenced path is pinned in internal/tokclient and internal/agent.)
func TestALeaseTakenMidRunLoadsNothingAndDefersCapacity(t *testing.T) {
	ll, root := armLateLease(t)
	var resident atomic.Bool
	resident.Store(true)
	fake := &agentFake{rosterIDs: []string{agentTestSeat}}
	fake.running = func(int64) string {
		if resident.Load() {
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}`
		}
		return `{"running":[]}`
	}
	fake.loop = func(n int64) string {
		if n == 1 {
			ll.take(t, fake)
			resident.Store(false)
		}
		return toolChat(n)
	}
	fake.repack = func(int64) string { return `{"answer":"never"}` }
	var tokenizes atomic.Int64
	fake.tokenize = func(w http.ResponseWriter, r *http.Request) {
		tokenizes.Add(1)
		fourBytePieces(w, r)
	}
	srv := fake.server(t)
	defer srv.Close()
	p := lateLeasePipeline(t, srv.URL, root, 5)

	contract := testContract()
	contract.TimeoutSec = 3
	wire, err := p.RunAgentContract(context.Background(), contract, AgentContractOptions{})
	if err != nil {
		t.Fatalf("RunAgentContract: %v", err)
	}
	if !ll.taken.Load() {
		t.Fatal("the loop never reached its first completion: the test did not exercise a mid-run lease")
	}
	if tokenizes.Load() == 0 {
		t.Error("the warm seat's tokenizer was never asked before the lease: the fake is not exercising the passthrough")
	}
	if got := fake.upstreamAll.Load() - ll.atTaking.Load(); got != 0 {
		t.Fatalf("%d request(s) reached /upstream after the render took the card mid-run", got)
	}
	if !wire.Deferred || wire.DeferClass != core.DeferClassCapacity {
		t.Fatalf("want a capacity defer; got deferred=%v class=%q reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if !strings.Contains(wire.Reason, "gpu busy") || !strings.Contains(wire.Reason, "video render") {
		t.Errorf("reason must name the held card and the render: %s", wire.Reason)
	}
}

// fourBytePieces is a llama.cpp-shaped /tokenize: one token per four bytes,
// pieces that reconstruct the input exactly (the contract tokclient enforces).
func fourBytePieces(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Content    string `json:"content"`
		WithPieces bool   `json:"with_pieces"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	var toks []map[string]any
	for i := 0; i < len(req.Content); i += 4 {
		j := i + 4
		if j > len(req.Content) {
			j = len(req.Content)
		}
		if req.WithPieces {
			toks = append(toks, map[string]any{"id": len(toks), "piece": req.Content[i:j]})
		} else {
			toks = append(toks, map[string]any{"id": len(toks)})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if !req.WithPieces {
		ids := make([]int, len(toks))
		_ = json.NewEncoder(w).Encode(map[string]any{"tokens": ids})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"tokens": toks})
}
