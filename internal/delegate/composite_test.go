package delegate

// The composite-tier runner (ADR 0039, 0.116.0): a LOCAL placement is decided
// once, over the placement table, and the decision drives the seat the local
// runner is handed, the `placed` block on the wire, the ledger's layer column,
// the pair-long capacity wait and the guard-named defers. Every test injects
// the decider's LIVE READERS (RunOptions.LocalDecider) and runs the REAL table
// over config.CompositeFixture() — a hand-written Decision would pin the
// runner against a decision the table never makes.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	placetable "github.com/dmmdea/offload-harness/internal/placement"
)

// compositeTestCfg is testCfg with the Qube's four layers declared: the
// reference composite box, rooted in a temp dir like every other test config.
func compositeTestCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := testCfg(t)
	fx := config.CompositeFixture()
	cfg.TierProfile, cfg.Tiers, cfg.Layers = fx.TierProfile, fx.Tiers, fx.Layers
	return cfg
}

// readings is what the injected decider's readers see: the pair agent seat's
// occupancy (scripted per DECISION, the last entry repeating), admitting
// display-card readings, and a presence answer.
type readings struct {
	pairAgent []placetable.SeatState
	presence  placetable.Presence
	decisions atomic.Int64
}

func (rd *readings) live() placetable.Live {
	n := int(rd.decisions.Add(1))
	state := placetable.SeatState{}
	if len(rd.pairAgent) > 0 {
		k := n - 1
		if k >= len(rd.pairAgent) {
			k = len(rd.pairAgent) - 1
		}
		state = rd.pairAgent[k]
	}
	pres := rd.presence
	return placetable.Live{
		Seat: func(layer, role string) placetable.SeatState {
			if layer == placetable.LayerPair && role == placetable.RoleAgent {
				return state
			}
			return placetable.SeatState{}
		},
		DeviceFree:  func(string) (float64, bool) { return 16, true },
		DeviceIndex: func(d string) (string, bool) { return d, true },
		HostFree:    func() (float64, bool) { return 80, true },
		Presence:    func() placetable.Presence { return pres },
	}
}

// tableDecider runs the production table over cfg's layers with rd's readers —
// exactly what the runner does with a Snapshot, minus nvidia-smi.
func tableDecider(cfg config.Config, rd *readings) func(context.Context, core.AgentContract, Subtask) placetable.Decision {
	return func(_ context.Context, c core.AgentContract, st Subtask) placetable.Decision {
		return placetable.Decide(placetable.RequestForContract(c, st.EstTokens, cfg.AgentMaxTokens), cfg.Layers, rd.live())
	}
}

// echoingLocal is a local seat that records the options it was handed and
// answers on the seat it was told to (the pipeline's own behaviour: the wire
// names the seat that ran and carries the placed block verbatim).
func echoingLocal(got *LocalOptions, calls *atomic.Int64) LocalRunner {
	return func(_ context.Context, c core.AgentContract, opts LocalOptions) (core.AgentWireResult, error) {
		calls.Add(1)
		*got = opts
		seat := opts.Seat
		if seat == "" {
			seat = "local-seat"
		}
		return core.AgentWireResult{SchemaVersion: core.AgentWireSchemaVersion, NodeID: "local", Seat: seat,
			Output: "qube answered locally", Structured: json.RawMessage(`{"answer":"qube"}`), StopReason: "done", Placed: opts.Placed}, nil
	}
}

// contractOfTokens is remoteContract carrying one context doc sized so the
// chars/3 estimate lands at ~tokens.
func contractOfTokens(tokens int) core.AgentContract {
	c := remoteContract()
	c.Context = []core.ContextDoc{{Name: "corpus.txt", Text: strings.Repeat("a", tokens*3)}}
	return c
}

func coldPair() placetable.SeatState { return placetable.SeatState{Known: true} }
func idlePair() placetable.SeatState { return placetable.SeatState{Known: true, Loaded: true} }
func busyPair(n int) placetable.SeatState {
	return placetable.SeatState{Known: true, Loaded: true, Inflight: n}
}

// wireOf renders one result the way both surfaces publish it.
func wireOf(t *testing.T, results []PlacedResult, sum Summary) map[string]any {
	t.Helper()
	b, err := json.Marshal(WireResponse(results, sum, nil))
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Results[0]
}

// ledgerRows reads every JSON row the run's ledger holds.
func ledgerRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("ledger row %q: %v", line, err)
		}
		rows = append(rows, m)
	}
	return rows
}

// TestLocalRunUsesTheDecidedSeatAndPublishesPlaced: a ~205k-token contract
// overflows the pair's agent window; with the agent seat cold the table places
// it on the pair's long seat, the local runner is handed THAT seat, the wire
// carries `placed` beside the untouched `placement` string, and the ledger row
// names the layer.
func TestLocalRunUsesTheDecidedSeatAndPublishesPlaced(t *testing.T) {
	cfg := compositeTestCfg(t)
	rd := &readings{pairAgent: []placetable.SeatState{coldPair()}}
	var got LocalOptions
	var calls atomic.Int64
	results, sum, err := RunWith(context.Background(), cfg, echoingLocal(&got, &calls),
		[]core.AgentContract{contractOfTokens(205_000)}, "auto", nil, &RunOptions{LocalDecider: tableDecider(cfg, rd)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 || calls.Load() != 1 {
		t.Fatalf("summary = %+v calls = %d, want one local success", sum, calls.Load())
	}
	if got.Seat != "qwen3.8-27b-262k" || got.Placed == nil || got.Placed.Layer != "pair" || got.Placed.Role != "long" {
		t.Fatalf("local runner options = %+v, want the pair's long seat decided", got)
	}
	pr := results[0]
	if pr.Seat != "qwen3.8-27b-262k" || pr.Placed == nil || pr.Placed.Layer != "pair" || pr.Placed.Role != "long" || pr.Placed.Seat != "qwen3.8-27b-262k" {
		t.Fatalf("result seat = %q placed = %+v", pr.Seat, pr.Placed)
	}
	if pr.CapacityWaitSec != 0 || sum.Waited != 0 {
		t.Fatalf("a cold pair has nothing to drain: waited %.2fs summary %+v", pr.CapacityWaitSec, sum)
	}
	rw := wireOf(t, results, sum)
	placed, ok := rw["placed"].(map[string]any)
	if !ok || placed["layer"] != "pair" || placed["role"] != "long" || placed["seat"] != "qwen3.8-27b-262k" {
		t.Fatalf("wire placed = %v", rw["placed"])
	}
	if _, isString := rw["placement"].(string); !isString || rw["placement"] != "local idle" {
		t.Fatalf("placement must stay the untouched reason STRING (opencode + fleet_smoke read it as text), got %v", rw["placement"])
	}
	rows := ledgerRows(t, cfg.LedgerPath)
	if len(rows) != 1 || rows[0]["layer"] != "pair" {
		t.Fatalf("ledger rows = %v, want one row with layer pair", rows)
	}
}

// TestNonCompositeBoxPublishesNoPlacedKey pins the byte-identical constraint
// on the runner: no layers → zero LocalOptions, no placed block, no `placed`
// key on the wire, no `layer` on the ledger row.
func TestNonCompositeBoxPublishesNoPlacedKey(t *testing.T) {
	cfg := testCfg(t)
	var got LocalOptions
	var calls atomic.Int64
	results, sum, err := Run(context.Background(), cfg, echoingLocal(&got, &calls), []core.AgentContract{remoteContract()}, "auto", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 || got != (LocalOptions{}) || results[0].Placed != nil {
		t.Fatalf("summary=%+v options=%+v placed=%+v, want the pre-0.116 shape", sum, got, results[0].Placed)
	}
	rw := wireOf(t, results, sum)
	if _, has := rw["placed"]; has {
		t.Fatalf("a plain box must not publish a placed key: %v", rw)
	}
	rows := ledgerRows(t, cfg.LedgerPath)
	if _, has := rows[0]["layer"]; has {
		t.Fatalf("a plain box's ledger row must carry no layer: %v", rows[0])
	}
}

// TestGuardRefusalDefersNamingTheGuard: an explicit context_class long asks for
// the triple layer; with the operator at the desk (presence unset = present)
// the guard refuses, and the runner publishes a capacity defer whose placed
// block names the guard — nothing runs, nothing waits, nothing is trimmed.
func TestGuardRefusalDefersNamingTheGuard(t *testing.T) {
	cfg := compositeTestCfg(t)
	rd := &readings{pairAgent: []placetable.SeatState{coldPair()}, presence: placetable.Presence{Mode: "present", Known: true}}
	c := remoteContract()
	c.ContextClass = core.ContextClassLong
	results, sum, err := RunWith(context.Background(), cfg, neverLocal(t),
		[]core.AgentContract{c}, "auto", nil, &RunOptions{LocalDecider: tableDecider(cfg, rd)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Deferred != 1 || sum.Waited != 0 || sum.Failed != 0 {
		t.Fatalf("summary = %+v, want one defer", sum)
	}
	pr := results[0]
	if !pr.Result.Deferred || pr.Result.DeferClass != core.DeferClassCapacity {
		t.Fatalf("result = %+v, want a capacity defer", pr.Result)
	}
	if pr.Placed == nil || pr.Placed.Guard != "presence" || pr.Placed.Layer != "triple" {
		t.Fatalf("placed = %+v, want the presence guard on the triple layer named", pr.Placed)
	}
	if !strings.Contains(pr.Result.Reason, "presence") || pr.Seat != "qwen3.8-flash-next-262k" {
		t.Fatalf("reason = %q seat = %q, want the guard in the reason and the refused seat named", pr.Result.Reason, pr.Seat)
	}
	rw := wireOf(t, results, sum)
	if placed, _ := rw["placed"].(map[string]any); placed["guard"] != "presence" {
		t.Fatalf("wire placed = %v, want guard presence", rw["placed"])
	}
	if rows := ledgerRows(t, cfg.LedgerPath); len(rows) != 1 || rows[0]["layer"] != "triple" || rows[0]["deferred"] != true {
		t.Fatalf("ledger rows = %v, want one deferred row on layer triple", rows)
	}
}

// TestSaturatedPairIsRecordedNotRePlaced (council R2): the pair's agent seat
// reads 32/32 in flight and an eligible remote sits in the roster. The
// contract STILL runs local on agent-pool — saturation is written into
// placed.reason, no capacity wait runs, the remote is never dialled.
func TestSaturatedPairIsRecordedNotRePlaced(t *testing.T) {
	cfg := compositeTestCfg(t)
	cfg.AgentPlacementWaitSec = 5
	node, url := acceptingNode(t, "node-idle", "remote answered", func(f *fakeNode) {
		f.dispatchHook = func(int64) int {
			t.Error("the remote was dialled; a saturated pair is recorded, never re-placed")
			return http.StatusServiceUnavailable
		}
	})
	rd := &readings{pairAgent: []placetable.SeatState{busyPair(32)}}
	var got LocalOptions
	var calls atomic.Int64
	results, sum, err := RunWith(context.Background(), cfg, echoingLocal(&got, &calls),
		[]core.AgentContract{remoteContract()}, "auto", []string{url}, &RunOptions{LocalDecider: tableDecider(cfg, rd)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 || sum.Waited != 0 || calls.Load() != 1 || node.dispatches.Load() != 0 {
		t.Fatalf("summary=%+v local=%d dispatches=%d, want one local run and no dispatch", sum, calls.Load(), node.dispatches.Load())
	}
	pr := results[0]
	if got.Seat != "agent-pool" || pr.Seat != "agent-pool" || pr.CapacityWaitSec != 0 {
		t.Fatalf("seat = %q/%q waited = %.2f, want agent-pool with no wait", got.Seat, pr.Seat, pr.CapacityWaitSec)
	}
	if pr.Placed == nil || pr.Placed.Layer != "pair" || pr.Placed.Role != "agent" || !strings.Contains(pr.Placed.Reason, "queued in the seat") || !strings.Contains(pr.Placed.Reason, "32/32") {
		t.Fatalf("placed = %+v, want the saturation recorded in the reason", pr.Placed)
	}
}

// TestWindowOverflowWaitsForABusyPairThenRuns (council R1): the overflow
// contract's long seat would evict a busy agent-pool, so the runner holds it
// in the capacity wait; once the agent seat reads idle the contract runs on
// the long seat, the wait is credited, and placed.evicts names the seat it
// waited for.
func TestWindowOverflowWaitsForABusyPairThenRuns(t *testing.T) {
	compressWait(t, 20*time.Millisecond, 0)
	cfg := compositeTestCfg(t)
	cfg.AgentPlacementWaitSec = 5
	rd := &readings{pairAgent: []placetable.SeatState{busyPair(3), idlePair()}}
	var got LocalOptions
	var calls atomic.Int64
	results, sum, err := RunWith(context.Background(), cfg, echoingLocal(&got, &calls),
		[]core.AgentContract{contractOfTokens(205_000)}, "auto", nil, &RunOptions{LocalDecider: tableDecider(cfg, rd)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 || sum.Waited != 1 || calls.Load() != 1 {
		t.Fatalf("summary=%+v local=%d, want one success after a wait", sum, calls.Load())
	}
	pr := results[0]
	if pr.CapacityWaitSec <= 0 || !pr.waited {
		t.Fatalf("capacity_wait_sec = %.3f, want the drain wait credited", pr.CapacityWaitSec)
	}
	if got.Seat != "qwen3.8-27b-262k" || pr.Seat != "qwen3.8-27b-262k" {
		t.Fatalf("seat = %q/%q, want the pair's long seat", got.Seat, pr.Seat)
	}
	if pr.Placed == nil || pr.Placed.Evicts != "agent-pool" || pr.Placed.Layer != "pair" || pr.Placed.Role != "long" {
		t.Fatalf("placed = %+v, want evicts agent-pool on pair/long", pr.Placed)
	}
	if !strings.Contains(pr.PlacementReason, "capacity wait") || !strings.Contains(pr.PlacementReason, "agent-pool") {
		t.Fatalf("placement = %q, want the wait and the drained seat named", pr.PlacementReason)
	}
	if rd.decisions.Load() < 2 {
		t.Fatalf("decisions = %d, want the decider re-run on the tick", rd.decisions.Load())
	}
	if rows := ledgerRows(t, cfg.LedgerPath); len(rows) != 1 || rows[0]["layer"] != "pair" {
		t.Fatalf("ledger rows = %v, want exactly one row (the wait records nothing of its own)", rows)
	}
}

// TestWindowOverflowWaitExpiryRunsOnTheDecidedSeat: a pair that never drains.
// The wait is bounded by agent_placement_wait_sec and its expiry RUNS the
// contract on the decided seat with the expired wait in the reason — never a
// capacity defer (the seat exists and holds the contract) and never the
// lease-holder deferral (no lease is held).
func TestWindowOverflowWaitExpiryRunsOnTheDecidedSeat(t *testing.T) {
	compressWait(t, 50*time.Millisecond, 0)
	cfg := compositeTestCfg(t)
	cfg.AgentPlacementWaitSec = 1
	rd := &readings{pairAgent: []placetable.SeatState{busyPair(3)}}
	var got LocalOptions
	var calls atomic.Int64
	start := time.Now()
	results, sum, err := RunWith(context.Background(), cfg, echoingLocal(&got, &calls),
		[]core.AgentContract{contractOfTokens(205_000)}, "auto", nil, &RunOptions{LocalDecider: tableDecider(cfg, rd)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if waited := time.Since(start); waited < time.Second {
		t.Fatalf("Run returned after %s — it did not wait the configured second", waited)
	}
	if sum.Succeeded != 1 || sum.Waited != 1 || sum.Deferred != 0 || calls.Load() != 1 {
		t.Fatalf("summary=%+v local=%d, want the contract RUN after the wait expired", sum, calls.Load())
	}
	pr := results[0]
	if got.Seat != "qwen3.8-27b-262k" || pr.Placed == nil || pr.Placed.Evicts != "agent-pool" {
		t.Fatalf("seat = %q placed = %+v, want the decided long seat, evicting agent-pool", got.Seat, pr.Placed)
	}
	if !strings.Contains(pr.PlacementReason, "expired") || strings.Contains(pr.PlacementReason, "lease") {
		t.Fatalf("placement = %q, want the expired wait named and no lease text", pr.PlacementReason)
	}
	if pr.CapacityWaitSec < 0.4 {
		t.Fatalf("capacity_wait_sec = %.2f, want the wait credited", pr.CapacityWaitSec)
	}
}

// TestCompositeCapAdmitsA600KiBContract: the intake validates against the
// BOX's cap — 256 KiB on a plain box, the largest layer window × 3 on a
// composite one — so a contract sized for the long seats passes the door.
func TestCompositeCapAdmitsA600KiBContract(t *testing.T) {
	spec := SubtaskSpec{AgentContract: core.AgentContract{
		Goal:         "find the needle",
		Context:      []core.ContextDoc{{Name: "big.txt", Text: strings.Repeat("x", 600<<10)}},
		OutputSchema: json.RawMessage(`{"properties":{"answer":{"type":"string"}}}`),
	}}
	comp := config.CompositeFixture()
	if capBytes := comp.AgentContextCapBytes(); capBytes <= core.AgentContextMaxBytes {
		t.Fatalf("composite cap = %d, want above the plain %d", capBytes, core.AgentContextMaxBytes)
	}
	c, err := PrepareContractWithCap(spec, "", comp.AgentContextCapBytes())
	if err != nil {
		t.Fatalf("composite intake refused a 600 KiB contract: %v", err)
	}
	if len(c.Context) != 1 || c.Depth != 0 || c.SchemaVersion != core.AgentWireSchemaVersion {
		t.Fatalf("prepared contract = %+v", c)
	}
	if _, err := PrepareContractWithCap(spec, "", config.Default().AgentContextCapBytes()); err == nil {
		t.Fatal("a plain box's intake must refuse a 600 KiB contract at the 256 KiB cap")
	}
	if _, err := PrepareContract(spec, ""); err == nil {
		t.Fatal("PrepareContract keeps the plain transport cap")
	}
}

// TestLeaseClearedOntoABusyPairContinuesAsADecidedWait: the local seat is
// under a text lease (the lease wait), the lease clears mid-wait, and the
// composite decision for the freed seat says WAIT (the long seat would evict a
// busy agent-pool). The same wait continues as a decided one and runs the
// seat once the pair drains — the sentinel a forced local attempt hands back
// is never published as a result.
func TestLeaseClearedOntoABusyPairContinuesAsADecidedWait(t *testing.T) {
	compressWait(t, 20*time.Millisecond, 0)
	dir, lease := holdLease(t, gpulease.ClassText, "soak")
	cfg := compositeTestCfg(t)
	cfg.GPULockPath = dir
	cfg.AgentPlacementWaitSec = 5
	rd := &readings{pairAgent: []placetable.SeatState{busyPair(3), busyPair(2), idlePair()}}
	var got LocalOptions
	var calls atomic.Int64
	go func() {
		time.Sleep(120 * time.Millisecond)
		_ = lease.Release()
	}()
	results, sum, err := RunWith(context.Background(), cfg, echoingLocal(&got, &calls),
		[]core.AgentContract{contractOfTokens(205_000)}, "auto", nil, &RunOptions{LocalDecider: tableDecider(cfg, rd)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Succeeded != 1 || sum.Waited != 1 || sum.Infrastructure != 0 || calls.Load() != 1 {
		t.Fatalf("summary=%+v local=%d, want one success after the lease cleared and the pair drained", sum, calls.Load())
	}
	pr := results[0]
	if pr.waitCapacity || pr.Result.Output == "" || pr.Err != "" {
		t.Fatalf("result = %+v, want a real run (never the wait sentinel)", pr)
	}
	if got.Seat != "qwen3.8-27b-262k" || pr.Placed == nil || pr.Placed.Evicts != "agent-pool" {
		t.Fatalf("seat = %q placed = %+v, want the long seat after the drain", got.Seat, pr.Placed)
	}
	if pr.CapacityWaitSec <= 0 || !strings.Contains(pr.PlacementReason, "drained") {
		t.Fatalf("capacity_wait_sec = %.3f placement = %q", pr.CapacityWaitSec, pr.PlacementReason)
	}
	if rd.decisions.Load() < 2 {
		t.Fatalf("decisions = %d, want the decider re-run after the lease cleared", rd.decisions.Load())
	}
}
