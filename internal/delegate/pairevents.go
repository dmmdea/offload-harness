package delegate

import (
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
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
// card identity on pr. node is the harness node name ("" = this box), aliases
// the other names the same node goes by (its fleet node id, the name it
// reported); seat is the seat the placement intends to run on; local says the
// seat is behind this box's own endpoint, so its alias can be resolved to an
// engine (a remote node's aliases live in its own roster).
func (r *runner) pairInflight(pr *PlacedResult, jobID, node string, aliases []string, seat, state string, local bool) {
	if r.pair == nil || !r.pair.Enabled() {
		return
	}
	now := time.Now().UnixMilli()
	if pr.pairModel == "" {
		pr.pairModel = seat
		if pr.pairModel == "" {
			pr.pairModel = "agent-seat"
		}
		if local {
			pr.pairEngine = r.pair.LocalEngine("agent_delegate", pr.pairModel)
		} else {
			pr.pairEngine = pairworkloads.EngineFor("agent_delegate", pr.pairModel)
		}
		pr.pairCreated = now
	}
	pr.pairNode, pr.pairAliases = node, aliases
	started := int64(0)
	if state == "running" {
		pr.pairStarted = now
		started = now
	}
	r.pair.Emit(pairworkloads.Event{
		JobID:       jobID,
		Model:       pr.pairModel,
		Engine:      pr.pairEngine,
		Node:        node,
		NodeAliases: aliases,
		State:       state,
		Requester:   pairworkloads.Requester(ledger.ProcessOrigin().Session),
		CreatedAt:   pr.pairCreated,
		StartedAt:   started,
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
	// The card stays on the node the in-flight frame put it on. A node names
	// itself on the wire by its fleet node id when one is configured, which
	// PAIR cannot resolve — before 0.131.2 that name re-pointed every remote
	// card at the delegator on completion — so the wire name is only an
	// alias behind the dispatch host, and a local placement keeps "" (this
	// box) or the endpoint host the in-flight frame named (C-58).
	node, aliases := pr.pairNode, append([]string(nil), pr.pairAliases...)
	if pr.ranBase != "" {
		if h := pairNodeName(pr.ranBase, ""); h != "" && h != node {
			aliases = append(aliases, h)
		}
		if pr.Node != "" && pr.Node != node {
			aliases = append(aliases, pr.Node)
		}
		if node == "" && len(aliases) > 0 {
			node, aliases = aliases[0], aliases[1:]
		}
	}
	r.pair.Emit(pairworkloads.Event{
		JobID:       jobID,
		Model:       pr.pairModel,
		Engine:      pr.pairEngine,
		Node:        node,
		NodeAliases: aliases,
		State:       state,
		Error:       errText,
		Requester:   pairworkloads.Requester(ledger.ProcessOrigin().Session),
		CreatedAt:   pr.pairCreated,
		StartedAt:   pr.pairStarted,
		CompletedAt: time.Now().UnixMilli(),
	})
}

// A delegation card reads "running" only once the seat is WORKING on it
// (0.140.5). Before, the frame went out at the node's ack (remote) or the
// hand-off to the runner (local), so a card said "Running on <node>"
// through a 2-minute seat load while the card sat at 0 % (2026-09-23). The
// card now stays "queued" through admission and cold load, and turns
// "running" on the first sign the seat is serving the request: a streamed
// token, a decode, a tool call or a re-pack. A prefill counts once it has
// lasted pairPrefillGrace without the seat probe (5 s cadence) calling it a
// load. A run that reports no progress at all (a node too old to publish it)
// turns running pairNoProgressGrace after it started, as before, give or take.
const (
	pairPrefillGrace    = 8 * time.Second
	pairNoProgressGrace = 30 * time.Second
)

// seatWorking reads one progress report. since is when the job was first
// seen started; now is the reading's time.
func seatWorking(p *core.LiveProgress, since, now time.Time) bool {
	if p == nil {
		return !since.IsZero() && now.Sub(since) >= pairNoProgressGrace
	}
	if p.TokensOut > 0 {
		return true
	}
	switch p.Phase {
	case "decoding", "tool", "repack":
		return true
	case "prefill":
		return p.LastProgressMs > 0 && now.Sub(time.UnixMilli(p.LastProgressMs)) >= pairPrefillGrace
	}
	return false
}

// pairStartGate flips a local placement's card to running once, from the
// run's own progress reports (core.WithProgressReport) or a timer, and never
// after the run returned.
type pairStartGate struct {
	mu      sync.Mutex
	fired   bool
	done    bool
	last    *core.LiveProgress
	started time.Time
	timer   *time.Timer
	fire    func()
}

func newPairStartGate(fire func()) *pairStartGate {
	g := &pairStartGate{fire: fire, started: time.Now()}
	g.mu.Lock()
	g.timer = time.AfterFunc(pairNoProgressGrace, g.recheck)
	g.mu.Unlock()
	return g
}

// observe is the progress sink.
func (g *pairStartGate) observe(p core.LiveProgress) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fired || g.done {
		return
	}
	g.last = &p
	if seatWorking(g.last, g.started, time.Now()) {
		g.fireLocked()
		return
	}
	if p.Phase == "prefill" && p.LastProgressMs > 0 {
		// Silent prefill sends no further report: re-read at the grace.
		wait := time.Until(time.UnixMilli(p.LastProgressMs).Add(pairPrefillGrace))
		g.timer.Reset(wait + 100*time.Millisecond)
	}
}

func (g *pairStartGate) recheck() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fired || g.done {
		return
	}
	var since time.Time
	if g.last == nil {
		since = g.started
	}
	if seatWorking(g.last, since, time.Now()) {
		g.fireLocked()
	}
}

func (g *pairStartGate) fireLocked() {
	g.fired = true
	g.timer.Stop()
	g.fire()
}

// stop ends the gate: nothing fires after it returns.
func (g *pairStartGate) stop() {
	g.mu.Lock()
	g.done = true
	g.timer.Stop()
	g.mu.Unlock()
}
