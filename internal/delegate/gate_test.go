package delegate

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/gpulease"
)

// gateSchema is a minimal gbnf-compilable output schema — the gate only checks
// PRESENCE (compilability is Validate's job, checked before dispatch), but the
// fixture uses a real one so the tests read like real contracts.
var gateSchema = json.RawMessage(`{"properties":{"summary":{"type":"string"}}}`)

// schemaSubtask builds the reference eligible subtask: schema present, depth 0,
// EstTokens chosen so it fits the reference remote's 8192-token ceiling.
func schemaSubtask() Subtask {
	return Subtask{
		Contract: core.AgentContract{
			SchemaVersion: core.AgentWireSchemaVersion,
			Goal:          "summarize the docs",
			OutputSchema:  gateSchema,
			Depth:         0,
		},
		EstTokens: 1000,
	}
}

// eligibleRemote is the reference remote that passes every gate condition for
// schemaSubtask: enabled, resident, 8192-token ceiling, shallow queue.
func eligibleRemote() NodeView {
	return NodeView{
		NodeID:         "lenovo",
		AgentEnabled:   true,
		AgentSeat:      "offload-e4b",
		AgentResident:  true,
		AgentCtxTokens: 8192,
		QueueDepth:     1,
	}
}

func localNode() NodeView {
	return NodeView{NodeID: "qube", Local: true}
}

// TestPlace drives every gate condition flip through the §S3 hard gate as
// reshaped (roast deltas 3+5): remote eligible iff AgentEnabled &&
// AgentResident && EstTokens+specReserve <= AgentCtxTokens && OutputSchema
// present && Depth == 0; remote chosen ONLY when localBusy; lowest QueueDepth
// among eligible remotes; no eligible remote => local regardless.
//
// Every node here publishes NO capacity numbers, which is what keeps these
// cases pinning the ORIGINAL queue_depth rule after 0.101.0 added the two
// capacity keys above it: with max_queue_depth and max_concurrent_jobs unknown,
// neither new key can fire and depth decides, exactly as it always did. The
// capacity keys themselves are pinned in TestPlaceIsCapacityAware.
func TestPlace(t *testing.T) {
	fit := 8192 - specReserve // the exact EstTokens that fills the ceiling

	cases := []struct {
		name      string
		mutate    func(st *Subtask, remotes []NodeView) // fixture tweak; remotes aliases the slice passed to Place
		remotes   func() []NodeView
		localBusy bool
		wantNode  string
	}{
		{
			name:      "idle local always wins even with an eligible remote",
			remotes:   func() []NodeView { return []NodeView{eligibleRemote()} },
			localBusy: false,
			wantNode:  "qube",
		},
		{
			name:      "busy local with an eligible remote goes remote",
			remotes:   func() []NodeView { return []NodeView{eligibleRemote()} },
			localBusy: true,
			wantNode:  "lenovo",
		},
		{
			name: "agent lane disabled on the remote stays local",
			mutate: func(st *Subtask, remotes []NodeView) {
				remotes[0].AgentEnabled = false
			},
			remotes:   func() []NodeView { return []NodeView{eligibleRemote()} },
			localBusy: true,
			wantNode:  "qube",
		},
		{
			name: "seat not roster-resident on the remote stays local",
			mutate: func(st *Subtask, remotes []NodeView) {
				remotes[0].AgentResident = false
			},
			remotes:   func() []NodeView { return []NodeView{eligibleRemote()} },
			localBusy: true,
			wantNode:  "qube",
		},
		{
			name: "contract without an output schema stays local",
			mutate: func(st *Subtask, remotes []NodeView) {
				st.Contract.OutputSchema = nil
			},
			remotes:   func() []NodeView { return []NodeView{eligibleRemote()} },
			localBusy: true,
			wantNode:  "qube",
		},
		{
			name: "empty-but-non-nil schema counts as absent (presence means bytes)",
			mutate: func(st *Subtask, remotes []NodeView) {
				st.Contract.OutputSchema = json.RawMessage{}
			},
			remotes:   func() []NodeView { return []NodeView{eligibleRemote()} },
			localBusy: true,
			wantNode:  "qube",
		},
		{
			name: "depth>=1 requester never places remote (hop limit)",
			mutate: func(st *Subtask, remotes []NodeView) {
				st.Contract.Depth = 1
			},
			remotes:   func() []NodeView { return []NodeView{eligibleRemote()} },
			localBusy: true,
			wantNode:  "qube",
		},
		{
			name: "ctx arithmetic boundary: EstTokens+specReserve == ceiling passes",
			mutate: func(st *Subtask, remotes []NodeView) {
				st.EstTokens = fit
			},
			remotes:   func() []NodeView { return []NodeView{eligibleRemote()} },
			localBusy: true,
			wantNode:  "lenovo",
		},
		{
			name: "ctx arithmetic boundary: one token over the ceiling fails",
			mutate: func(st *Subtask, remotes []NodeView) {
				st.EstTokens = fit + 1
			},
			remotes:   func() []NodeView { return []NodeView{eligibleRemote()} },
			localBusy: true,
			wantNode:  "qube",
		},
		{
			name: "unadvertised ctx ceiling (0) never fits",
			mutate: func(st *Subtask, remotes []NodeView) {
				remotes[0].AgentCtxTokens = 0
			},
			remotes:   func() []NodeView { return []NodeView{eligibleRemote()} },
			localBusy: true,
			wantNode:  "qube",
		},
		{
			name:      "busy local with no remotes at all stays local",
			remotes:   func() []NodeView { return nil },
			localBusy: true,
			wantNode:  "qube",
		},
		{
			name: "two eligible remotes: lowest queue depth wins",
			remotes: func() []NodeView {
				deep := eligibleRemote()
				deep.NodeID, deep.QueueDepth = "deep", 3
				shallow := eligibleRemote()
				shallow.NodeID, shallow.QueueDepth = "shallow", 1
				return []NodeView{deep, shallow}
			},
			localBusy: true,
			wantNode:  "shallow",
		},
		{
			name: "queue-depth tiebreak is stable: first listed wins",
			remotes: func() []NodeView {
				a := eligibleRemote()
				a.NodeID, a.QueueDepth = "first", 2
				b := eligibleRemote()
				b.NodeID, b.QueueDepth = "second", 2
				return []NodeView{a, b}
			},
			localBusy: true,
			wantNode:  "first",
		},
		{
			name: "an ineligible shallow remote loses to an eligible deeper one",
			remotes: func() []NodeView {
				shallow := eligibleRemote()
				shallow.NodeID, shallow.QueueDepth, shallow.AgentResident = "shallow-dead", 0, false
				deep := eligibleRemote()
				deep.NodeID, deep.QueueDepth = "deep-live", 4
				return []NodeView{shallow, deep}
			},
			localBusy: true,
			wantNode:  "deep-live",
		},
		{
			name: "busy local with only ineligible remotes stays local",
			remotes: func() []NodeView {
				a := eligibleRemote()
				a.AgentEnabled = false
				b := eligibleRemote()
				b.NodeID, b.AgentResident = "other", false
				return []NodeView{a, b}
			},
			localBusy: true,
			wantNode:  "qube",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := schemaSubtask()
			remotes := tc.remotes()
			if tc.mutate != nil {
				tc.mutate(&st, remotes)
			}
			got := Place("seed", st, localNode(), remotes, tc.localBusy)
			if got.NodeID != tc.wantNode {
				t.Fatalf("Place -> %q, want %q", got.NodeID, tc.wantNode)
			}
		})
	}
}

// TestEstimateTokensSumsEveryContractPart pins WHAT is counted: goal + every
// context doc (name AND text — both are materialized for the sub-agent) +
// the raw schema bytes + every acceptance string.
func TestEstimateTokensSumsEveryContractPart(t *testing.T) {
	c := core.AgentContract{
		Goal:         "abc",                                         // 3
		Context:      []core.ContextDoc{{Name: "doc", Text: "xyz"}}, // 3 + 3
		OutputSchema: json.RawMessage(`{"a":1}`),                    // 7
		Acceptance:   []string{"ab"},                                // 2
	}
	// total chars = 3 + 3 + 3 + 7 + 2 = 18 -> ceil(18/3) = 6
	if got := EstimateTokens(c); got != 6 {
		t.Fatalf("EstimateTokens = %d, want 6 (18 chars / 3)", got)
	}
}

// TestEstimateTokensRoundsUp pins the conservative direction of the division:
// a remainder must count as a whole token, never be truncated away.
func TestEstimateTokensRoundsUp(t *testing.T) {
	if got := EstimateTokens(core.AgentContract{Goal: "abcd"}); got != 2 {
		t.Fatalf("EstimateTokens(4 chars) = %d, want 2 (ceil, not floor)", got)
	}
	if got := EstimateTokens(core.AgentContract{Goal: "abcdef"}); got != 2 {
		t.Fatalf("EstimateTokens(6 chars) = %d, want exactly 2", got)
	}
}

// TestLocalBusyReadsTheRealLease exercises LocalBusy against a REAL lease
// taken through gpulease's own write path in an isolated lease dir: free ->
// false, held -> true, released -> false. This proves LocalBusy rides the one
// read path (InspectDir) rather than a parallel reconstruction that would
// drift the moment gpulease moves a detail (the heartbeat file did exactly
// that once — see InspectDir's doc comment).
func TestLocalBusyReadsTheRealLease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lease")
	if LocalBusy(dir, "") {
		t.Fatal("LocalBusy = true on a lease dir nobody holds")
	}
	m, err := gpulease.OpenAt(dir, "")
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	lease, err := m.TryAcquire(gpulease.ClassMedia, gpulease.Options{Reason: "delegate-test", TTL: time.Minute})
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if !LocalBusy(dir, "") {
		t.Fatal("LocalBusy = false while a media lease is held")
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if LocalBusy(dir, "") {
		t.Fatal("LocalBusy = true after the lease was released")
	}
}

// TestLocalBusyUnresolvableLeaseDirReadsIdle pins the failure direction: a
// lease location gpulease refuses (a cloud-sync segment) must read as NOT
// busy — Place then keeps work local, which is always the safe placement.
func TestLocalBusyUnresolvableLeaseDirReadsIdle(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "My Drive", "lease") // refused by the sync-root guard
	if LocalBusy(bad, "") {
		t.Fatal("LocalBusy = true on an unresolvable lease dir; must fail toward idle/local")
	}
}

// TestRemoteEligible_ServedModelsWithoutSeatIsIneligible is the alias-blind
// gate regression: eligibleRemote's AgentSeat ("offload-e4b") is an ALIAS,
// the normal shape for an agent seat (agent-pool -> qwen3.8-27b-vllm,
// offload-e4b -> gemma-4-e4b — see swapclient.go:9's plannerUnserved
// lesson), never the canonical id the node's roster actually keys the model
// by ("gemma-4-e4b"). A served_models list carrying ONLY the canonical id —
// what an id-only publisher (the pre-fix swapRosterIDs) would have sent —
// must read as ineligible: the honest negative below documents that the
// node's job is to publish its aliases too (fleetnode's
// swapRosterServedModels now does, via swapclient.Roster.Names), not for
// this gate to special-case alias resolution on the delegator side.
func TestRemoteEligible_ServedModelsWithoutSeatIsIneligible(t *testing.T) {
	r := eligibleRemote()
	r.ServedModels = []string{"gemma-4-e4b"} // canonical id only — the alias seat is absent
	if remoteEligible(schemaSubtask(), r) {
		t.Fatal("a node whose served_models publishes only the canonical id, omitting the alias agent_seat, must not be placed on")
	}
	r.ServedModels = []string{"gemma-4-e4b", r.AgentSeat} // now the alias is published too
	if !remoteEligible(schemaSubtask(), r) {
		t.Fatal("seat present in served_models (as an alias, alongside the canonical id) must be eligible")
	}
	r.ServedModels = nil // pre-0.113.0 node: unknown, not a refusal
	if !remoteEligible(schemaSubtask(), r) {
		t.Fatal("absent served_models is UNKNOWN and must not gate")
	}
	r.ServedModels = []string{strings.ToUpper(r.AgentSeat)}
	if !remoteEligible(schemaSubtask(), r) {
		t.Fatal("seatServed must match case-insensitively, like swapclient.Roster.Serves")
	}
}

func TestBetterRemote_UtilizationBreaksQueueTies(t *testing.T) {
	st := schemaSubtask()
	a, b := eligibleRemote(), eligibleRemote()
	a.NodeID, b.NodeID = "a", "b"
	a.GpuUtilPct, a.GpuUtilKnown = 80, true
	b.GpuUtilPct, b.GpuUtilKnown = 10, true
	if !betterRemote("seed", &st, 0, b, a) || betterRemote("seed", &st, 0, a, b) {
		t.Fatal("lower known utilization must win an otherwise equal pair")
	}
	// queue depth still outranks utilization
	b.QueueDepth = a.QueueDepth + 1
	if betterRemote("seed", &st, 0, b, a) {
		t.Fatal("utilization must never override QueueDepth")
	}
	if !betterRemote("seed", &st, 0, a, b) {
		t.Fatal("a's lower QueueDepth must still win once QueueDepth is no longer tied")
	}
}

func TestBetterRemote_UnknownUtilizationNeverLoses(t *testing.T) {
	st := schemaSubtask()
	known, unknown := eligibleRemote(), eligibleRemote()
	known.GpuUtilPct, known.GpuUtilKnown = 5, true
	if betterRemote("seed", &st, 0, known, unknown) || betterRemote("seed", &st, 0, unknown, known) {
		t.Fatal("an unknown utilization is neither credited nor blamed — roster order keeps the tie")
	}
}

// TestBetterRemote_TieBreakSkipsTheOperatorsDesktop pins the placement half of
// the 2026-09-20 defect. GpuUtilPct is the busiest card on the WHOLE box, so a
// node whose operator is gaming advertised that load and lost the tie to a node
// it should have beaten — on the Qube a game read 33% on the display card while
// every card the harness could use sat at 0%. WorkUtilPct skips a card reporting
// display_active, and placementUtil picks the figure per node.
func TestBetterRemote_TieBreakSkipsTheOperatorsDesktop(t *testing.T) {
	st := schemaSubtask()
	gaming, busy := eligibleRemote(), eligibleRemote()
	gaming.NodeID, busy.NodeID = "gaming", "busy"
	// The gaming node LOOKS busier on the whole box, but its harness cards are idle.
	gaming.GpuUtilPct, gaming.GpuUtilKnown = 33, true
	gaming.WorkUtilPct, gaming.WorkUtilKnown = 0, true
	// The other node has no desktop load but its harness card is genuinely working.
	busy.GpuUtilPct, busy.GpuUtilKnown = 20, true
	busy.WorkUtilPct, busy.WorkUtilKnown = 20, true
	if !betterRemote("seed", &st, 0, gaming, busy) {
		t.Fatal("the node whose HARNESS cards are idle must win, even though its desktop makes the whole box read busier")
	}
	if betterRemote("seed", &st, 0, busy, gaming) {
		t.Fatal("the node doing real harness work must not beat an idle one on a desktop-inflated figure")
	}
}

// TestBetterRemote_MixedFleetRanksOnOneFigurePerNode pins the defect the first
// cut of the work_util tie-break introduced. It chose the figure PER PAIR —
// work_util when both sides had it, gpu_util otherwise — so during a rollout,
// the only time a mixed fleet exists, three nodes formed a strict cycle:
//
//	gaming (work 0, gpu 80) beats light (work 5, gpu 5)   on work_util
//	light  (work 5, gpu 5)  beats older  (gpu 10)         on gpu_util
//	older  (gpu 10)         beats gaming (work 0, gpu 80) on gpu_util
//
// bestRemote folds this relation over a slice, so the winner was whichever node
// the slice started from. placementUtil picks the figure per NODE instead,
// which makes the key a total order at the cost of comparing an upgraded node's
// desktop-free number against an old node's desktop-inclusive one — a bias
// toward the node whose number is true.
func TestBetterRemote_MixedFleetRanksOnOneFigurePerNode(t *testing.T) {
	st := schemaSubtask()
	mk := func(id string, gpu, work int, workKnown bool) NodeView {
		v := eligibleRemote()
		v.NodeID = id
		v.GpuUtilPct, v.GpuUtilKnown = gpu, true
		v.WorkUtilPct, v.WorkUtilKnown = work, workKnown
		return v
	}
	gaming := mk("gaming", 80, 0, true) // upgraded, harness cards idle behind a game
	light := mk("light", 5, 5, true)    // upgraded, genuinely doing a little work
	older := mk("older", 10, 0, false)  // pre-0.132.2: publishes no work_util

	// Antisymmetry and transitivity over every pair and triple.
	all := []NodeView{gaming, light, older}
	for _, a := range all {
		for _, b := range all {
			if a.NodeID == b.NodeID {
				continue
			}
			if betterRemote("seed", &st, 0, a, b) && betterRemote("seed", &st, 0, b, a) {
				t.Fatalf("%s and %s each beat the other", a.NodeID, b.NodeID)
			}
		}
	}
	for _, a := range all {
		for _, b := range all {
			for _, c := range all {
				if betterRemote("seed", &st, 0, a, b) && betterRemote("seed", &st, 0, b, c) &&
					betterRemote("seed", &st, 0, c, a) {
					t.Fatalf("preference cycle: %s > %s > %s > %s", a.NodeID, b.NodeID, c.NodeID, a.NodeID)
				}
			}
		}
	}

	// And the fold the cycle actually broke: the winner cannot depend on order.
	fold := func(rs []NodeView) string {
		best, found := NodeView{}, false
		for _, r := range rs {
			if !found || betterRemote("seed", &st, 0, r, best) {
				best, found = r, true
			}
		}
		return best.NodeID
	}
	for _, order := range [][]NodeView{
		{gaming, light, older},
		{older, light, gaming},
		{light, gaming, older},
	} {
		if got := fold(order); got != "gaming" {
			t.Fatalf("fold(%s, %s, %s) = %s, want gaming — the node with the idlest HARNESS cards",
				order[0].NodeID, order[1].NodeID, order[2].NodeID, got)
		}
	}
}
