// agenttask_seatscope_test.go pins the delegation door's fence check under a CARD-SCOPED
// lease (plan P4, register C-86): a media render on one card is a fence on the seats pinned
// to that card and nothing else. The lease is real (the lease package's own write path in a
// scratch root); only the card table is synthetic.

package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/gpuprobe"
	"github.com/dmmdea/offload-harness/internal/llamaclient"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
	"github.com/dmmdea/offload-harness/internal/seatload"
)

// card2RenderPipeline holds a media lease on card 2 of a synthetic 3-card box, pins the
// agent test seat to the cards in pins, and returns a pipeline over a fake llama-swap.
func card2RenderPipeline(t *testing.T, endpoint string, pins []string) *Pipeline {
	t.Helper()
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = modelaffinity.SetGPULease("", filepath.Join(t.TempDir(), "My Drive", "x")) })
	modelaffinity.SetSeatPins(func(model string) ([]string, bool) {
		if model == agentTestSeat {
			return pins, true
		}
		return nil, false
	})
	t.Cleanup(func() { modelaffinity.SetSeatPins(nil) })
	restore := modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		return []gpuprobe.Card{
			{UUID: "GPU-aaaa0000", NvidiaIndex: 0, ComfyOrder: -1},
			{UUID: "GPU-bbbb0000", NvidiaIndex: 1, Display: true, ComfyOrder: -1},
			{UUID: "GPU-cccc0000", NvidiaIndex: 2, ComfyOrder: -1},
		}, "", nil
	})
	t.Cleanup(restore)
	holder, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "hero render", TTL: time.Hour, Devices: []string{"gpu-cccc0000"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Release() })
	cfg := config.Config{
		Endpoint: endpoint, Model: "workhorse", AgentModel: agentTestSeat,
		FleetNodeID: "node-t", Temperature: 0.1,
		AgentAdmissionWaitSec: 5, StateDir: root, Home: t.TempDir(),
	}
	return New(cfg, llamaclient.New(endpoint, "", cfg.Model, 30*time.Second), nil, nil)
}

func scopedAgentFake(loops *atomic.Int64) *agentFake {
	return &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { loops.Add(1); return doneChat("The answer is 42.") },
		repack:    func(int64) string { return `{"answer":"42"}` },
		running:   func(int64) string { return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}` },
	}
}

// A seat on card 0 runs under a render on card 2: no fence, no cordon, the loop runs.
func TestRunAgentContractRunsOnAFreeCardUnderACard2Render(t *testing.T) {
	var loops atomic.Int64
	fake := scopedAgentFake(&loops)
	srv := fake.server(t)
	defer srv.Close()
	p := card2RenderPipeline(t, srv.URL, []string{"0"})

	contract := testContract()
	contract.TimeoutSec = 30
	start := time.Now()
	wire, err := p.RunAgentContract(context.Background(), contract, AgentContractOptions{})
	if err != nil {
		t.Fatalf("RunAgentContract: %v", err)
	}
	if wire.Deferred {
		t.Fatalf("a seat on a free card must not be fenced by a render on another card; deferred: %s", wire.Reason)
	}
	if loops.Load() == 0 {
		t.Error("the loop never ran on the free card")
	}
	if spent := time.Since(start); spent > 10*time.Second {
		t.Errorf("the run waited %s: it was cordoned behind a render that is not on its card", spent)
	}
}

// The same render fences the seat that lives on its card: the verdict is on disk, so the
// door defers at once, naming the holder.
func TestRunAgentContractDefersOnTheHeldCardUnderACard2Render(t *testing.T) {
	var loops atomic.Int64
	fake := scopedAgentFake(&loops)
	srv := fake.server(t)
	defer srv.Close()
	p := card2RenderPipeline(t, srv.URL, []string{"2"})

	contract := testContract()
	contract.TimeoutSec = 30
	start := time.Now()
	wire, err := p.RunAgentContract(context.Background(), contract, AgentContractOptions{})
	if err != nil {
		t.Fatalf("RunAgentContract: %v", err)
	}
	if !wire.Deferred || wire.DeferClass != core.DeferClassCapacity {
		t.Fatalf("want a capacity defer for a seat on the rendering card; got deferred=%v class=%q reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if spent := time.Since(start); spent > time.Second {
		t.Errorf("the fence's verdict is on disk before the dial: deferred after %s", spent)
	}
	if !strings.Contains(wire.Reason, "media render holds the cards") {
		t.Errorf("the defer must name the render: %s", wire.Reason)
	}
	if loops.Load() != 0 {
		t.Errorf("the loop must never run behind the fence; ran %d time(s)", loops.Load())
	}
}

// THE SEAT RACE RULE through the delegation door's cold-load warm-up (plan P4): the warm-up
// passed the lease fence, and a device lease on the seat's card is claimed while the seat is
// loading. The seat is resident by the time the warm-up answers, on a card a render now holds:
// it yields (unloads itself) and the contract is a re-placeable capacity defer, never a run
// beside the render.
func TestWarmUpRacingADeviceLeaseYieldsTheSeat(t *testing.T) {
	root := t.TempDir()
	m, err := gpulease.OpenAt("", root)
	if err != nil {
		t.Fatal(err)
	}
	m.SetCardScoped(true)
	if err := modelaffinity.SetGPULease("", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = modelaffinity.SetGPULease("", filepath.Join(t.TempDir(), "My Drive", "x")) })
	modelaffinity.SetSeatPins(func(model string) ([]string, bool) {
		if model == agentTestSeat {
			return []string{"0"}, true
		}
		return nil, false
	})
	t.Cleanup(func() { modelaffinity.SetSeatPins(nil) })
	t.Cleanup(modelaffinity.SetCardTableReader(func(context.Context, string) ([]gpuprobe.Card, string, error) {
		return []gpuprobe.Card{
			{UUID: "GPU-aaaa0000", NvidiaIndex: 0, ComfyOrder: -1},
			{UUID: "GPU-bbbb0000", NvidiaIndex: 1, Display: true, ComfyOrder: -1},
			{UUID: "GPU-cccc0000", NvidiaIndex: 2, ComfyOrder: -1},
		}, "", nil
	}))
	var unloaded []string
	t.Cleanup(modelaffinity.SetYieldSeams(modelaffinity.YieldSeams{
		Read: func(_ context.Context, _, model string) (seatload.Reading, error) {
			return seatload.Reading{Loaded: true, Canonical: model}, nil
		},
		Unload: func(_ context.Context, _, model string) error { unloaded = append(unloaded, model); return nil },
	}))

	var loops atomic.Int64
	var grant sync.Once
	var holder *gpulease.Lease
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop:      func(int64) string { loops.Add(1); return doneChat("never") },
		repack:    func(int64) string { return `{"answer":"never"}` },
		running: func(n int64) string {
			if n <= 2 {
				return `{"running":[]}`
			}
			return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}`
		},
		upstreamModels: func(int64) string {
			// The load finishes, and a render's device lease lands on the seat's card in the
			// same instant: after the fence the warm-up passed, before the seat is resident.
			grant.Do(func() {
				var gerr error
				holder, gerr = m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "hero render", TTL: time.Hour, Devices: []string{"gpu-aaaa0000"}})
				if gerr != nil {
					t.Errorf("the racing device grant: %v", gerr)
				}
			})
			return `{"object":"list","data":[{"id":"` + agentTestSeat + `","max_model_len":131072}]}`
		},
	}
	srv := fake.server(t)
	defer srv.Close()
	t.Cleanup(func() {
		if holder != nil {
			_ = holder.Release()
		}
	})

	contract := testContract()
	contract.TimeoutSec = 30
	p := admissionTestPipeline(t, srv.URL, 30)
	p.cfg.StateDir = root
	wire := decodeWire(t, p.Run(context.Background(), agentTestRequest(t, contract)))
	if !wire.Deferred || wire.DeferClass != core.DeferClassCapacity {
		t.Fatalf("a seat that yielded the cards to a render is a capacity defer; got deferred=%v class=%q reason=%q", wire.Deferred, wire.DeferClass, wire.Reason)
	}
	if !strings.Contains(wire.Reason, "unloaded itself") {
		t.Errorf("the defer says the seat yielded: %s", wire.Reason)
	}
	if len(unloaded) != 1 || unloaded[0] != agentTestSeat {
		t.Fatalf("the seat must unload itself exactly once, got %v", unloaded)
	}
	if loops.Load() != 0 {
		t.Errorf("the loop must never run beside the render; ran %d time(s)", loops.Load())
	}
}

// The delegation door registers its run with the pins of its seat (plan P5): a drain waits only
// for runs on the cards a lease holds, and a single-card seat's run-cap line is its card's.
func TestAgentRunRecordsItsSeatPins(t *testing.T) {
	var seen atomic.Value // []string, the pins of the run the loop saw registered
	var regDir string
	var loops atomic.Int64
	fake := &agentFake{
		rosterIDs: []string{agentTestSeat},
		loop: func(int64) string {
			loops.Add(1)
			if reg := gpuactivity.OpenAt(regDir); reg != nil {
				for _, r := range reg.List(time.Now()) {
					if r.Seat == agentTestSeat {
						seen.Store(append([]string(nil), r.Devices...))
					}
				}
			}
			return doneChat("The answer is 42.")
		},
		repack:  func(int64) string { return `{"answer":"42"}` },
		running: func(int64) string { return `{"running":[{"model":"` + agentTestSeat + `","state":"ready","cmd":"y"}]}` },
	}
	srv := fake.server(t)
	defer srv.Close()
	p := card2RenderPipeline(t, srv.URL, []string{"0"})
	regDir = gpuactivity.DirBeside(mustLeaseDir(t, p.cfg.StateDir))

	contract := testContract()
	contract.TimeoutSec = 30
	if _, err := p.RunAgentContract(context.Background(), contract, AgentContractOptions{}); err != nil {
		t.Fatalf("RunAgentContract: %v", err)
	}
	if loops.Load() == 0 {
		t.Fatal("the loop never ran, so no registered run was observed")
	}
	got, _ := seen.Load().([]string)
	if len(got) != 1 || got[0] != "0" {
		t.Fatalf("the registered run carries the pins of its seat, got %v", got)
	}
}
