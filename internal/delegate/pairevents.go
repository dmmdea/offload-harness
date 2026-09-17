package delegate

import (
	"net/url"
	"strings"
	"time"

	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// pairNodeName is the name a remote placement is reported under BEFORE the
// node has answered: the host of its dispatch URL (the tailnet name the fleet
// config lists, which is the hostname PAIR's members.json carries), lowercased
// and without the port. The fleet node id (`aorus-ampere8`-style) is not a
// PAIR member name and resolved to nothing, which is why 0.126.0's queued and
// running frames showed the delegator as the running node until the terminal
// frame, built from the node's reported name, corrected it. fallback is used
// when base has no usable host.
func pairNodeName(base, fallback string) string {
	u, err := url.Parse(strings.TrimSpace(base))
	if err == nil && u.Hostname() != "" {
		return strings.ToLower(u.Hostname())
	}
	return fallback
}

// PAIR workload frames for delegations (docs/systems/pair-workloads.md).
//
// A delegated subtask is one card in NVIDIA PAIR's Jobs list: queued when a
// remote node is asked, running when it acks (or a local placement starts),
// completed/failed when attempt()'s finish records it. PAIR's store keys a
// card by (origin, engine, runId, id), so the in-flight and terminal frames
// MUST carry the same model and engine — they are fixed at the first emission
// and carried on the PlacedResult, never recomputed from whatever seat the
// node later reported. A subtask that never placed (refused before placement,
// a capacity settle) emits nothing: it touched no card.
//
// Terminal frames for delegations come from HERE, not from the ledger
// observer (internal/pairworkloads.AttachLedger skips agent_delegate rows):
// the ledger row's ModelTier names the seat the node ended up on, which can
// differ from the seat the in-flight frame named, and two identities would be
// two cards, one stuck "running" until PAIR's staleness sweep failed it.

// pairInflight emits the queued / running frame for a subtask and pins the
// card identity on pr. node is the harness node name ("" = this box); seat is
// the seat the placement intends to run on.
func (r *runner) pairInflight(pr *PlacedResult, jobID, node, seat, state string) {
	if r.pair == nil || !r.pair.Enabled() {
		return
	}
	now := time.Now().UnixMilli()
	if pr.pairModel == "" {
		pr.pairModel = seat
		if pr.pairModel == "" {
			pr.pairModel = "agent-seat"
		}
		pr.pairEngine = pairworkloads.EngineFor("agent_delegate", pr.pairModel)
		pr.pairCreated = now
	}
	started := int64(0)
	if state == "running" {
		pr.pairStarted = now
		started = now
	}
	r.pair.Emit(pairworkloads.Event{
		JobID:     jobID,
		Model:     pr.pairModel,
		Engine:    pr.pairEngine,
		Node:      node,
		State:     state,
		Requester: pairworkloads.Requester(ledger.ProcessOrigin().Session),
		CreatedAt: pr.pairCreated,
		StartedAt: started,
	})
}

// pairTerminal emits the completed / failed frame for a subtask that had an
// in-flight frame. The verdict mirrors the ledger row's: a wire failure, a
// defer or a failed acceptance check is "failed", with the same reason text
// the ledger records.
func (r *runner) pairTerminal(jobID string, pr *PlacedResult) {
	if r.pair == nil || pr.pairModel == "" || !r.pair.Enabled() {
		return
	}
	state, errText := "completed", ""
	switch {
	case pr.Err != "":
		state, errText = "failed", pr.Err
	case pr.Result.Deferred:
		state, errText = "failed", pr.Result.Reason
		if errText == "" {
			errText = "deferred"
		}
	case len(pr.AcceptanceFailures) > 0:
		state, errText = "failed", "failed verification: "+pr.AcceptanceFailures[0]
	}
	r.pair.Emit(pairworkloads.Event{
		JobID:       jobID,
		Model:       pr.pairModel,
		Engine:      pr.pairEngine,
		Node:        pr.Node,
		State:       state,
		Error:       errText,
		Requester:   pairworkloads.Requester(ledger.ProcessOrigin().Session),
		CreatedAt:   pr.pairCreated,
		StartedAt:   pr.pairStarted,
		CompletedAt: time.Now().UnixMilli(),
	})
}
