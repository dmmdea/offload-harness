// deal_layer_test.go: register A-108. A contract that names a layer (`layer` on the delegation door,
// register A-100) is never handed to the LOCAL seat of a box that does not declare that layer.
//
// Before this, route=spread dealt the local rotation slot without looking at the contract's layer (a
// ledger of 2026-09-19 shows six `declares no layers` defers, all on slot 1 of 2) and route=auto's
// idle-local shortcut kept the subtask unconditionally, so a contract that a node on the fleet could
// have run ended as a defer naming the layer while that node sat idle. The A-100 rule lived in
// Place alone, and neither deal calls Place.
//
// The end-to-end tests drive Run / RunWith, the production entry. A white-box runOne would bypass the
// joint deal (autoDeal stays nil) and exercise the per-subtask Place path instead, which is the one
// path that was already right.

package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// fastSeat is the model the one-card box's `fast` layer places on (oneCardRows / oneCardConfig).
const fastSeat = "qwen36-35b-a3b-gsq-vllm"

// layerContract is a digest-shaped contract: a schema (so a remote may take it), an acceptance every
// fixture's output passes, and the layer it names ("" = none).
func layerContract(goal, layer string) core.AgentContract {
	return core.AgentContract{
		SchemaVersion: core.AgentWireSchemaVersion,
		Goal:          goal,
		OutputSchema:  json.RawMessage(`{"properties":{"answer":{"type":"string"}}}`),
		Acceptance:    []string{"contains:digest", "nonempty:answer"},
		MaxSteps:      4,
		TimeoutSec:    30,
		Layer:         layer,
	}
}

func layerContracts(n int, layer string) []core.AgentContract {
	out := make([]core.AgentContract, n)
	for i := range out {
		out[i] = layerContract("digest page "+string(rune('A'+i)), layer)
	}
	return out
}

// layerTape records the layer each dispatched contract carried, in arrival order.
type layerTape struct {
	mu     sync.Mutex
	layers []string
}

func (l *layerTape) add(c core.AgentContract) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.layers = append(l.layers, c.Layer)
}

func (l *layerTape) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.layers...)
}

// declaringNode is an accepting fleet node that advertises the one-card box's rows, so it DECLARES the
// `fast` layer (and `single`). It answers every job with output that passes layerContract's acceptance.
func declaringNode(t *testing.T, id string, tape *layerTape, tune func(*fakeNode)) (*fakeNode, string) {
	t.Helper()
	rows := oneCardRows(t)
	return acceptingNode(t, id, "digest from "+id, func(f *fakeNode) {
		f.layers = rows
		f.onDispatch = func(_ string, c core.AgentContract) { tape.add(c) }
		if tune != nil {
			tune(f)
		}
	})
}

// oneCardConfig is a delegator box that DECLARES the `fast` layer: testCfg plus the one-card
// tier's two layers, the config twin of oneCardRows.
func oneCardConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := testCfg(t)
	cfg.TierProfile = "ampere-16"
	cfg.Layers = []config.LayerSpec{
		{Name: placetable.LayerSingle, Tier: "ampere-16", Devices: []string{"0"}, Seats: []config.LayerSeat{
			{Role: placetable.RoleAgent, Model: "qwen38-27b-gsq-vllm", Device: "0", CtxTokens: 32768},
		}},
		{Name: "fast", Tier: "ampere-16", Devices: []string{"0"}, Seats: []config.LayerSeat{
			{Role: placetable.RoleAgent, Model: fastSeat, Device: "0", CtxTokens: 32768, MaxInflight: 8},
		}},
	}
	if err := cfg.ValidateLayers(); err != nil {
		t.Fatalf("one-card layers must validate: %v", err)
	}
	return cfg
}

// layerDecider is the production placement table over cfg's layers with admitting readers, named-layer
// rule included: what runner.decide does with a Snapshot, minus nvidia-smi. A hand-written Decision
// would pin the runner against a decision the table never makes.
func layerDecider(cfg config.Config) func(context.Context, core.AgentContract, Subtask) placetable.Decision {
	live := placetable.Live{
		Seat:        func(string, string) placetable.SeatState { return placetable.SeatState{} },
		DeviceFree:  func(string) (float64, bool) { return 15, true },
		DeviceIndex: func(d string) (string, bool) { return d, true },
	}
	return func(_ context.Context, c core.AgentContract, st Subtask) placetable.Decision {
		req := placetable.RequestForContract(c, st.EstTokens, cfg.AgentMaxTokens)
		if c.Layer != "" {
			return placetable.DecideOnLayer(req, cfg.Layers, c.Layer, live)
		}
		return placetable.Decide(req, cfg.Layers, live)
	}
}

// digestLocal is a local seat that answers on the seat it was told to (the pipeline's own behaviour) with
// output that passes layerContract's acceptance. got, when non-nil, receives the options of the last run.
func digestLocal(got *LocalOptions, calls *atomic.Int64) LocalRunner {
	return func(_ context.Context, _ core.AgentContract, opts LocalOptions) (core.AgentWireResult, error) {
		calls.Add(1)
		if got != nil {
			*got = opts
		}
		seat := opts.Seat
		if seat == "" {
			seat = "local-seat"
		}
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: seat,
			Output: "digest from the local seat", Structured: json.RawMessage(`{"answer":"digest"}`), StopReason: "done", Placed: opts.Placed}, nil
	}
}

// ---- route=spread --------------------------------------------------------------------------------

// TestSpreadNeverDealsANamedLayerToALocalBoxThatDoesNotDeclareIt is the ledger pattern: two nodes, the
// rotation gives the local seat slots 1 and 3 (indexes 0 and 2), and the box declares no layers, so
// those two ended `layer fast requested; this box declares no layers` while the node that declares
// `fast` took the other two.
func TestSpreadNeverDealsANamedLayerToALocalBoxThatDoesNotDeclareIt(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	tape := &layerTape{}
	node, url := declaringNode(t, "fast-node", tape, nil)
	var localCalls atomic.Int64

	results, sum, err := Run(t.Context(), testCfg(t), digestLocal(nil, &localCalls), layerContracts(4, "fast"), "spread", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, pr := range results {
		if pr.Result.Deferred || pr.Err != "" || pr.Node != "fast-node" {
			t.Errorf("subtask %d: node=%q deferred=%v err=%q reason=%q placement=%q - want it run by fast-node, the only node that declares the layer",
				i, pr.Node, pr.Result.Deferred, pr.Err, pr.Result.Reason, pr.PlacementReason)
		}
	}
	if sum.Succeeded != 4 || sum.Deferred != 0 {
		t.Fatalf("summary = %+v, want 4 succeeded", sum)
	}
	if localCalls.Load() != 0 || node.dispatches.Load() != 4 {
		t.Fatalf("local runs = %d, dispatches to the declaring node = %d, want 0 and 4", localCalls.Load(), node.dispatches.Load())
	}
	for i, l := range tape.all() {
		if l != "fast" {
			t.Errorf("dispatch %d carried layer %q, want fast", i, l)
		}
	}
}

// TestSpreadKeepsTheLocalSlotForContractsThatNameNoLayer: the layer rule is per CONTRACT. Interleaved
// with named ones, the unnamed contracts keep the local rotation slots they always had (indexes 0 and 2).
func TestSpreadKeepsTheLocalSlotForContractsThatNameNoLayer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	tape := &layerTape{}
	node, url := declaringNode(t, "fast-node", tape, nil)
	var localCalls atomic.Int64
	subtasks := []core.AgentContract{
		layerContract("digest page A", ""), layerContract("digest page B", "fast"),
		layerContract("digest page C", ""), layerContract("digest page D", "fast"),
	}

	results, sum, err := Run(t.Context(), testCfg(t), digestLocal(nil, &localCalls), subtasks, "spread", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 4 {
		t.Fatalf("summary = %+v, want 4 succeeded", sum)
	}
	for i, pr := range results {
		wantLocal := subtasks[i].Layer == ""
		if wantLocal != pr.ranLocal {
			t.Errorf("subtask %d (layer %q): ranLocal=%v node=%q, want ranLocal=%v", i, subtasks[i].Layer, pr.ranLocal, pr.Node, wantLocal)
		}
	}
	if localCalls.Load() != 2 || node.dispatches.Load() != 2 {
		t.Fatalf("local runs = %d, dispatches = %d, want 2 and 2", localCalls.Load(), node.dispatches.Load())
	}
}

// TestSpreadDefersANamedLayerNoNodeDeclaresAndSaysSo: nobody on the fleet declares `fast`, so every
// subtask defers naming it - and the placement reason says why in words, never that "placement and gate
// disagree - please report" (the sentence for a bug in this package).
func TestSpreadDefersANamedLayerNoNodeDeclaresAndSaysSo(t *testing.T) {
	plain, url := acceptingNode(t, "plain-node", "digest from plain-node", nil)

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), layerContracts(2, "fast"), "spread", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Deferred != 2 || plain.dispatches.Load() != 0 {
		t.Fatalf("summary = %+v dispatches = %d, want 2 deferred and nothing dispatched", sum, plain.dispatches.Load())
	}
	for i, pr := range results {
		if !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassContract || !strings.Contains(pr.Result.Reason, "fast") {
			t.Errorf("subtask %d: result = %+v, want a contract defer naming the layer", i, pr.Result)
		}
		if !strings.Contains(pr.PlacementReason, "fast") || strings.Contains(pr.PlacementReason, "please report") {
			t.Errorf("subtask %d: placement reason = %q, want it to name the layer and not claim a bug", i, pr.PlacementReason)
		}
	}
}

// TestSpreadWaitsInLineForTheNodeThatDeclaresTheLayer: the one node that declares `fast` can start one job,
// and three contracts name it. The two it cannot start yet are a place in line for IT - not a defer from
// the local seat, which declares nothing - and land on it once it has room.
func TestSpreadWaitsInLineForTheNodeThatDeclaresTheLayer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	tape := &layerTape{}
	node, url := declaringNode(t, "fast-node", tape, func(f *fakeNode) { f.maxConcurrentJobs = 1 })
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 5
	var localCalls atomic.Int64

	results, sum, err := RunWith(t.Context(), cfg, digestLocal(nil, &localCalls), layerContracts(3, "fast"), "spread", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, pr := range results {
		if pr.Result.Deferred || pr.Err != "" || pr.Node != "fast-node" {
			t.Errorf("subtask %d: node=%q deferred=%v err=%q reason=%q placement=%q - want it to wait for fast-node and run there",
				i, pr.Node, pr.Result.Deferred, pr.Err, pr.Result.Reason, pr.PlacementReason)
		}
	}
	if sum.Succeeded != 3 || localCalls.Load() != 0 || node.dispatches.Load() != 3 {
		t.Fatalf("summary = %+v local runs = %d dispatches = %d, want 3 succeeded, 0 local, 3 dispatched", sum, localCalls.Load(), node.dispatches.Load())
	}
}

// TestSpreadHandsTheLocalSlotToABoxThatDeclaresTheLayer is the control: the same fleet, but the delegator
// box declares `fast` itself, so it keeps its slot and runs the contract on the layer's seat.
func TestSpreadHandsTheLocalSlotToABoxThatDeclaresTheLayer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	cfg := oneCardConfig(t)
	tape := &layerTape{}
	node, url := declaringNode(t, "fast-node", tape, nil)
	var got LocalOptions
	var localCalls atomic.Int64

	results, sum, err := RunWith(t.Context(), cfg, digestLocal(&got, &localCalls), layerContracts(2, "fast"),
		"spread", []string{url}, &RunOptions{LocalDecider: layerDecider(cfg)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 2 || localCalls.Load() != 1 || node.dispatches.Load() != 1 {
		t.Fatalf("summary = %+v local runs = %d dispatches = %d, want 2 succeeded: one local, one remote", sum, localCalls.Load(), node.dispatches.Load())
	}
	if !results[0].ranLocal || results[1].Node != "fast-node" {
		t.Fatalf("placement: subtask 0 ranLocal=%v, subtask 1 node=%q, want local then fast-node", results[0].ranLocal, results[1].Node)
	}
	if got.Seat != fastSeat || got.Placed == nil || got.Placed.Layer != "fast" {
		t.Fatalf("local run options = %+v, want the fast layer's seat %s and a placed block naming the layer", got, fastSeat)
	}
}

// TestSpreadDealsAroundTheLocalSlotOfACompositeBoxThatLacksTheLayer: declaring SOME layers is not declaring
// this one. A composite delegator whose layers do not include `fast` is as layerless for this contract as a plain
// box, and the node that declares it takes every subtask.
func TestSpreadDealsAroundTheLocalSlotOfACompositeBoxThatLacksTheLayer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	cfg := compositeTestCfg(t)
	if _, declared := cfg.Layer("fast"); declared {
		t.Fatal("fixture: the reference composite box must not declare fast")
	}
	tape := &layerTape{}
	node, url := declaringNode(t, "fast-node", tape, nil)
	var localCalls atomic.Int64

	results, sum, err := RunWith(t.Context(), cfg, digestLocal(nil, &localCalls), layerContracts(2, "fast"), "spread", []string{url},
		&RunOptions{LocalDecider: layerDecider(cfg)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 2 || localCalls.Load() != 0 || node.dispatches.Load() != 2 {
		t.Fatalf("summary = %+v local runs = %d dispatches = %d (results %+v), want both subtasks on fast-node", sum, localCalls.Load(), node.dispatches.Load(), results)
	}
}

// ---- route=auto ----------------------------------------------------------------------------------

// TestAutoNeverKeepsANamedLayerOnAnIdleLocalBoxThatDoesNotDeclareIt: the idle-local shortcut is for work
// the local box can run. A contract naming a layer it does not declare goes to the node that does -
// which means the roster must be read although nothing is busy.
func TestAutoNeverKeepsANamedLayerOnAnIdleLocalBoxThatDoesNotDeclareIt(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	tape := &layerTape{}
	node, url := declaringNode(t, "fast-node", tape, nil)

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), layerContracts(1, "fast"), "auto", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pr := results[0]
	if pr.Result.Deferred || pr.Err != "" || pr.Node != "fast-node" || sum.Succeeded != 1 {
		t.Fatalf("result = node %q deferred=%v err=%q reason=%q placement=%q summary=%+v, want it run by fast-node",
			pr.Node, pr.Result.Deferred, pr.Err, pr.Result.Reason, pr.PlacementReason, sum)
	}
	if node.dispatches.Load() != 1 || len(tape.all()) != 1 || tape.all()[0] != "fast" {
		t.Fatalf("dispatches = %d layers = %v, want one dispatch carrying layer fast", node.dispatches.Load(), tape.all())
	}
}

// TestAutoDefersANamedLayerNoNodeDeclaresNamingTheLayerAndTheFleet: nothing declares it. The defer names
// the layer, and the placement reason carries the fleet's verdict - not "local idle", which would say the
// box took the work, and not a bug claim.
func TestAutoDefersANamedLayerNoNodeDeclaresNamingTheLayerAndTheFleet(t *testing.T) {
	plain, url := acceptingNode(t, "plain-node", "digest from plain-node", nil)

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), layerContracts(1, "fast"), "auto", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pr := results[0]
	if sum.Deferred != 1 || plain.dispatches.Load() != 0 {
		t.Fatalf("summary = %+v dispatches = %d, want one defer and nothing dispatched", sum, plain.dispatches.Load())
	}
	if pr.Result.DeferClass != core.DeferClassContract || !strings.Contains(pr.Result.Reason, "fast") {
		t.Errorf("result = %+v, want a contract defer naming the layer", pr.Result)
	}
	for _, want := range []string{"fast", "no eligible remote"} {
		if !strings.Contains(pr.PlacementReason, want) {
			t.Errorf("placement reason = %q, want it to contain %q", pr.PlacementReason, want)
		}
	}
	for _, bad := range []string{"please report", "local idle", "local busy"} {
		if strings.Contains(pr.PlacementReason, bad) {
			t.Errorf("placement reason = %q, must not contain %q", pr.PlacementReason, bad)
		}
	}
}

// TestAutoDefersARemoteRouteNamedLayerNoNodeDeclares: route=remote never falls local; the defer names the
// layer rather than reporting a disagreement between the gate and the placement.
func TestAutoDefersARemoteRouteNamedLayerNoNodeDeclares(t *testing.T) {
	plain, url := acceptingNode(t, "plain-node", "digest from plain-node", nil)

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), layerContracts(1, "fast"), "remote", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := results[0].Result
	if sum.Deferred != 1 || plain.dispatches.Load() != 0 || !strings.Contains(r.Reason, "fast") || strings.Contains(r.Reason, "please report") {
		t.Fatalf("summary = %+v dispatches = %d reason = %q, want one defer that names the layer and claims no bug", sum, plain.dispatches.Load(), r.Reason)
	}
	if r.DeferClass != core.DeferClassContract {
		t.Fatalf("defer_class = %q, want contract: the caller named a layer nobody declares", r.DeferClass)
	}
}

// TestAutoHandsAnIdleBoxThatDeclaresTheLayerItsOwnContract is the control: the idle-local rule is
// untouched for a box that declares the layer.
func TestAutoHandsAnIdleBoxThatDeclaresTheLayerItsOwnContract(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	cfg := oneCardConfig(t)
	tape := &layerTape{}
	node, url := declaringNode(t, "fast-node", tape, nil)
	var got LocalOptions
	var localCalls atomic.Int64

	results, sum, err := RunWith(t.Context(), cfg, digestLocal(&got, &localCalls), layerContracts(1, "fast"),
		"auto", []string{url}, &RunOptions{LocalDecider: layerDecider(cfg)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 || !results[0].ranLocal || localCalls.Load() != 1 || node.dispatches.Load() != 0 {
		t.Fatalf("summary = %+v ranLocal=%v local runs = %d dispatches = %d, want the idle box to run it itself", sum, results[0].ranLocal, localCalls.Load(), node.dispatches.Load())
	}
	if got.Seat != fastSeat || got.Placed == nil || got.Placed.Layer != "fast" {
		t.Fatalf("local run options = %+v, want the fast layer's seat %s", got, fastSeat)
	}
}

// TestAutoReadsTheFleetForAnIdleBoxOnlyWhenAContractNeedsIt: an idle local box ignores the fleet, as it
// always did - the roster is read for a contract that names a layer the box does not declare, and for
// nothing else.
func TestAutoReadsTheFleetForAnIdleBoxOnlyWhenAContractNeedsIt(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	var healthReads atomic.Int64
	tape := &layerTape{}
	_, url := declaringNode(t, "fast-node", tape, func(f *fakeNode) {
		f.healthDelayFn = func() time.Duration { healthReads.Add(1); return 0 }
	})
	var localCalls atomic.Int64

	if _, sum, err := Run(t.Context(), testCfg(t), digestLocal(nil, &localCalls), layerContracts(2, ""), "auto", []string{url}); err != nil || sum.Succeeded != 2 {
		t.Fatalf("unnamed run: err=%v summary=%+v, want 2 succeeded locally", err, sum)
	}
	if n := healthReads.Load(); n != 0 || localCalls.Load() != 2 {
		t.Fatalf("an idle box with no named layer read the fleet %d time(s) and ran %d locally, want 0 and 2", n, localCalls.Load())
	}

	if _, sum, err := Run(t.Context(), testCfg(t), digestLocal(nil, &localCalls), layerContracts(1, "fast"), "auto", []string{url}); err != nil || sum.Succeeded != 1 {
		t.Fatalf("named run: err=%v summary=%+v, want 1 succeeded", err, sum)
	}
	if healthReads.Load() == 0 {
		t.Fatal("a contract naming a layer the idle box does not declare must read the roster to find the node that does")
	}
}

// ---- the other places the local seat is chosen --------------------------------------------------

// TestAReplacementNeverFallsBackToALocalBoxThatDoesNotDeclareTheLayer: the node that declares the layer
// answers its first dispatch with a 503. The local seat is the last resort for a refused subtask - but it
// cannot take this one, so the subtask waits in line for the node (INV-4) instead of ending as a defer
// from a seat that declares nothing.
func TestAReplacementNeverFallsBackToALocalBoxThatDoesNotDeclareTheLayer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 20*time.Millisecond, 0)
	tape := &layerTape{}
	node, url := declaringNode(t, "fast-node", tape, func(f *fakeNode) { f.dispatchHook = freesAfter(1, http.StatusServiceUnavailable) })
	cfg := testCfg(t)
	cfg.AgentPlacementWaitSec = 5

	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), layerContracts(1, "fast"), "auto", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pr := results[0]
	if pr.Result.Deferred || pr.Err != "" || pr.Node != "fast-node" || sum.Succeeded != 1 {
		t.Fatalf("result = node %q deferred=%v err=%q reason=%q placement=%q summary=%+v, want it to land on fast-node after its refusal",
			pr.Node, pr.Result.Deferred, pr.Err, pr.Result.Reason, pr.PlacementReason, sum)
	}
	if node.dispatches.Load() != 2 {
		t.Fatalf("dispatches = %d, want 2 (the refusal, then the ack)", node.dispatches.Load())
	}
}

// TestAVerificationRetryNeverLandsOnALocalBoxThatDoesNotDeclareTheLayer: the declaring node answers wrong, so
// acceptance fails and the engine looks for a different node to retry on. The local seat cannot run the
// layer, so there is none: the first attempt stands, with no retry recorded against a seat that would only
// have deferred it.
func TestAVerificationRetryNeverLandsOnALocalBoxThatDoesNotDeclareTheLayer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	node, url := acceptingNode(t, "fast-node", "wrong answer", func(f *fakeNode) { f.layers = oneCardRows(t) })

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), layerContracts(1, "fast"), "auto", []string{url})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pr := results[0]
	if len(pr.AcceptanceFailures) == 0 || pr.Node != "fast-node" || node.dispatches.Load() != 1 {
		t.Fatalf("result = node %q acceptance failures %v deferred=%v dispatches=%d, want the declaring node's wrong answer published",
			pr.Node, pr.AcceptanceFailures, pr.Result.Deferred, node.dispatches.Load())
	}
	if pr.RetriedOn != "" || sum.Retried != 0 || strings.Contains(pr.RetryNote, "declares no layers") {
		t.Fatalf("retried_on=%q retry_note=%q summary=%+v, want no retry on a seat that declares no layers", pr.RetriedOn, pr.RetryNote, sum)
	}
}

// TestAVerificationRetryGoesToAnotherNodeThatDeclaresTheLayer: with a second node that declares the layer, THAT
// is the different node the retry asks for. The local seat is never consulted.
func TestAVerificationRetryGoesToAnotherNodeThatDeclaresTheLayer(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	wrong, wrongURL := acceptingNode(t, "wrong-node", "wrong answer", func(f *fakeNode) { f.layers = oneCardRows(t) })
	right, rightURL := acceptingNode(t, "right-node", "digest from right-node", func(f *fakeNode) { f.layers = oneCardRows(t) })

	results, sum, err := Run(t.Context(), testCfg(t), neverLocal(t), layerContracts(1, "fast"), "auto", []string{wrongURL, rightURL})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pr := results[0]
	if pr.Node != "right-node" || pr.RetriedOn != "right-node" || len(pr.AcceptanceFailures) != 0 || pr.Result.Deferred {
		t.Fatalf("result = node %q retried_on %q failures %v deferred=%v note=%q, want the clean retry on right-node", pr.Node, pr.RetriedOn, pr.AcceptanceFailures, pr.Result.Deferred, pr.RetryNote)
	}
	if sum.Retried != 1 || sum.RetryRecovered != 1 || wrong.dispatches.Load() != 1 || right.dispatches.Load() != 1 {
		t.Fatalf("summary = %+v dispatches wrong=%d right=%d, want one recovered retry across the two declaring nodes", sum, wrong.dispatches.Load(), right.dispatches.Load())
	}
}

// TestALayerWaitThatEndsEmptyIsACapacityDeferEvenUnderALocalLease: the node that declares the layer refuses
// every dispatch while a text lease holds the local seat. The seat is not what the subtask waited for - it could
// never have run the layer - so the wait ends as the capacity defer it is, not as a defer naming the lease holder.
func TestALayerWaitThatEndsEmptyIsACapacityDeferEvenUnderALocalLease(t *testing.T) {
	compressPolls(t, 5*time.Millisecond, time.Second)
	compressWait(t, 50*time.Millisecond, 100*time.Millisecond)
	node, url := refusingNode(t, "fast-node", http.StatusServiceUnavailable, func(f *fakeNode) { f.layers = oneCardRows(t) })
	dir, _ := holdLease(t, gpulease.ClassText, "held for the layer test")
	cfg := testCfg(t)
	cfg.GPULockPath = dir
	cfg.AgentPlacementWaitSec = 1

	results, sum, err := RunWith(t.Context(), cfg, neverLocal(t), layerContracts(1, "fast"), "auto", []string{url}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := results[0].Result
	if sum.Deferred != 1 || r.DeferClass != core.DeferClassCapacity || !strings.Contains(r.Reason, "no node had room") || strings.Contains(r.Reason, "reserved") {
		t.Fatalf("summary = %+v result = class %q reason %q, want a capacity defer that does not blame the lease", sum, r.DeferClass, r.Reason)
	}
	if node.dispatches.Load() < 2 {
		t.Fatalf("the declaring node was asked %d time(s), want it re-asked during the wait", node.dispatches.Load())
	}
}

// TestALayerNoNodeDeclaresDefersByNameEvenUnderALocalLease: a text lease holds the local seat and nobody on the
// fleet declares the layer. Waiting for the lease would wait for a seat that could never run it, so the subtask
// defers naming the layer at once instead of a defer naming the lease holder.
func TestALayerNoNodeDeclaresDefersByNameEvenUnderALocalLease(t *testing.T) {
	for _, route := range []string{"spread", "auto"} {
		t.Run(route, func(t *testing.T) {
			plain, url := acceptingNode(t, "plain-node", "digest from plain-node", nil)
			dir, _ := holdLease(t, gpulease.ClassText, "held for the layer test")
			cfg := testCfg(t)
			cfg.GPULockPath = dir

			results, sum, err := Run(t.Context(), cfg, neverLocal(t), layerContracts(1, "fast"), route, []string{url})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			r := results[0].Result
			if sum.Deferred != 1 || plain.dispatches.Load() != 0 || r.DeferClass != core.DeferClassContract ||
				!strings.Contains(r.Reason, "fast") || strings.Contains(r.Reason, "reserved") {
				t.Fatalf("summary = %+v dispatches = %d result = class %q reason %q, want a contract defer naming the layer that does not blame the lease",
					sum, plain.dispatches.Load(), r.DeferClass, r.Reason)
			}
		})
	}
}

// ---- the deals, white box ------------------------------------------------------------------------

// fastRemoteView is a snapshot row for a node that declares the `fast` layer.
func fastRemoteView(t *testing.T) NodeView {
	t.Helper()
	v := fitBigRemote
	v.NodeID = "fast-node"
	v.Layers = oneCardRows(t)
	return v
}

// TestDealSpreadNeverGivesALocalSlotToALayerTheBoxDoesNotDeclare: the deal itself, over a fleet snapshot.
func TestDealSpreadNeverGivesALocalSlotToALayerTheBoxDoesNotDeclare(t *testing.T) {
	r := fitRunner(fastRemoteView(t))
	for i, sl := range r.dealSpread(layerContracts(4, "fast"), fitLocal()) {
		if sl.view.Local || sl.capacityWait || sl.view.NodeID != "fast-node" {
			t.Errorf("subtask %d dealt to %q (local=%v, capacity wait=%v, reason %q), want fast-node", i, sl.view.NodeID, sl.view.Local, sl.capacityWait, sl.reason)
		}
	}
}

// TestDealSpreadKeepsTheLocalSlotWhereTheBoxDeclaresTheLayer: declared in config (production) or carried as
// rows on the local view (Place's own convention) - either way the slot stays.
func TestDealSpreadKeepsTheLocalSlotWhereTheBoxDeclaresTheLayer(t *testing.T) {
	cases := map[string]func(t *testing.T) (*runner, NodeView){
		"declared in the delegator's config": func(t *testing.T) (*runner, NodeView) {
			r := fitRunner(fastRemoteView(t))
			r.cfg = oneCardConfig(t)
			return r, fitLocal()
		},
		"advertised as rows on the local view": func(t *testing.T) (*runner, NodeView) {
			local := fitLocal()
			local.Layers = oneCardRows(t)
			return fitRunner(fastRemoteView(t)), local
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			r, local := build(t)
			slots := r.dealSpread(layerContracts(4, "fast"), local)
			for i, sl := range slots {
				if want := i%2 == 0; sl.view.Local != want {
					t.Errorf("subtask %d: local=%v node=%q, want local=%v (rotation slots 1 and 3 are the local seat's)", i, sl.view.Local, sl.view.NodeID, want)
				}
			}
		})
	}
}

// TestDealSpreadSendsAFullDeclaringNodesOverflowToTheCapacityWait: the node that declares the layer is at its
// headroom. The overflow is a place in line for it (INV-4); it is neither dealt local nor deferred.
func TestDealSpreadSendsAFullDeclaringNodesOverflowToTheCapacityWait(t *testing.T) {
	remote := fastRemoteView(t)
	remote.MaxConcurrentJobs, remote.JobsRunning = 1, 0
	r := fitRunner(remote)
	book := r.dealSpread(layerContracts(3, "fast"), fitLocal())
	if book[0].view.NodeID != "fast-node" || book[0].capacityWait {
		t.Fatalf("subtask 0 = %+v, want it dealt to the node's one free slot", book[0].placement)
	}
	for i := 1; i < 3; i++ {
		if !book[i].capacityWait || !strings.Contains(book[i].reason, "fast") || !strings.Contains(book[i].reason, "headroom") {
			t.Errorf("subtask %d = %+v (capacity wait=%v), want a capacity wait whose reason names the layer and the headroom", i, book[i].placement, book[i].capacityWait)
		}
	}
}

// TestDealSpreadChargesNoLocalRunSlotForALayerTheSeatCannotTake: a subtask nobody can run and the seat only
// defers by name occupies no slot of the seat's run-cap line, so the unnamed subtask behind it still gets the
// one free slot the line has.
func TestDealSpreadChargesNoLocalRunSlotForALayerTheSeatCannotTake(t *testing.T) {
	r := fitRunner(fitMidRemote) // plain: eligible for an unnamed contract, never for one that names a layer
	r.cfg = testCfg(t)
	r.cfg.FleetMaxConcurrentJobs = 1 // the seat's run-cap line takes ONE run
	slots := r.dealSpread([]core.AgentContract{
		layerContract("digest page A", "fast"), // declared by no node: deferred by name, runs nothing
		layerContract("digest page B", ""),
		layerContract("digest page C", ""), // rotation slot 0 of 2: the seat's
	}, fitLocal())
	if !slots[2].view.Local || slots[2].capacityWait {
		t.Fatalf("subtask 2 was dealt %+v (capacity wait=%v, reason %q), want the local seat: the layer-naming subtask ran nothing there and must not have spent its slot",
			slots[2].view.NodeID, slots[2].capacityWait, slots[2].reason)
	}
}

// TestDealAutoRemoteKeepsAnIdleLayerlessBoxOutOfAContractThatNamesALayer: idle local wins only the work it
// can run.
func TestDealAutoRemoteKeepsAnIdleLayerlessBoxOutOfAContractThatNamesALayer(t *testing.T) {
	r := &runner{route: "auto"}
	views, bases := []NodeView{fastRemoteView(t)}, []string{"http://fast-node:18811"}
	slots := r.dealAutoRemote([]core.AgentContract{layerContract("digest page A", "fast"), layerContract("digest page B", "")}, fitLocal(), views, bases, false, nil)
	if slots[0].view.NodeID != "fast-node" || slots[0].view.Local || slots[0].noRemote || slots[0].capacityWait {
		t.Errorf("the contract naming fast was dealt %+v, want fast-node", slots[0])
	}
	if !slots[1].view.Local || slots[1].reason != "local idle" {
		t.Errorf("the unnamed contract was dealt %+v, want the idle local seat", slots[1])
	}
}

// TestNoEligibleRemoteNamesTheLayerNoRemoteDeclares: a remote that publishes no layer rows declares none, and
// the verdict says that instead of "placement and gate disagree".
func TestNoEligibleRemoteNamesTheLayerNoRemoteDeclares(t *testing.T) {
	r := &runner{remotes: []string{"http://plain-node:18811"}}
	st := schemaSubtask()
	st.Contract.Layer = "fast"
	why, class := r.noEligibleRemote(st, []NodeView{eligibleRemote()}, nil)
	if !strings.Contains(why, "fast") || strings.Contains(why, "please report") || class != core.DeferClassContract {
		t.Fatalf("verdict = %q (class %q), want the contract class and a sentence naming the layer", why, class)
	}
	// A composite remote that lacks it is named by its own rows' verdict: unchanged.
	composite := eligibleRemote()
	composite.Layers = pairOnlyRows(t)
	why, class = r.noEligibleRemote(st, []NodeView{composite}, nil)
	if !strings.Contains(why, "fast") || class != core.DeferClassContract {
		t.Fatalf("composite verdict = %q (class %q), want the table's refusal naming the layer", why, class)
	}
}
