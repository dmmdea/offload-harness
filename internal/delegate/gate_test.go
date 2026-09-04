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
			got := Place(st, localNode(), remotes, tc.localBusy)
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
		Goal:         "abc",                                       // 3
		Context:      []core.ContextDoc{{Name: "doc", Text: "xyz"}}, // 3 + 3
		OutputSchema: json.RawMessage(`{"a":1}`),                  // 7
		Acceptance:   []string{"ab"},                              // 2
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
	a, b := eligibleRemote(), eligibleRemote()
	a.NodeID, b.NodeID = "a", "b"
	a.GpuUtilPct, a.GpuUtilKnown = 80, true
	b.GpuUtilPct, b.GpuUtilKnown = 10, true
	if !betterRemote(b, a) || betterRemote(a, b) {
		t.Fatal("lower known utilization must win an otherwise equal pair")
	}
	// queue depth still outranks utilization
	b.QueueDepth = a.QueueDepth + 1
	if betterRemote(b, a) {
		t.Fatal("utilization must never override QueueDepth")
	}
	if !betterRemote(a, b) {
		t.Fatal("a's lower QueueDepth must still win once QueueDepth is no longer tied")
	}
}

func TestBetterRemote_UnknownUtilizationNeverLoses(t *testing.T) {
	known, unknown := eligibleRemote(), eligibleRemote()
	known.GpuUtilPct, known.GpuUtilKnown = 5, true
	if betterRemote(known, unknown) || betterRemote(unknown, known) {
		t.Fatal("an unknown utilization is neither credited nor blamed — roster order keeps the tie")
	}
}
