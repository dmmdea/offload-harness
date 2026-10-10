package pairworkloads

import (
	"fmt"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Attribution of a call the route sent to a fleet node (PAIR routing fixes, D5/D6).
//
// composeremote, visionremote, textremote, sttremote and mediaremote never call Pipeline.Run for a
// remote or auto-spilled call, so the asking box wrote no ledger row and showed no PAIR card for work
// it had spilled. A plain row would not have fixed it: ledger.Entry carries no node, so the observer
// would have carded the asker itself. RemoteCall is the one mechanism the lanes share: ONE card per
// dispatched call on the node that serves it (queued at dispatch, running when the node says the job
// started, terminal with the result), and ONE asker ledger row that tells the observer the card is
// already decided (Entry.CardByCaller).
//
// The card's identity is fixed when it opens: PAIR keys a card on (origin, engine, runId, id), so the
// engine never changes afterwards; the model may be refreshed on the terminal frame (the node says
// which seat it used), as a ledger-closed call card already does.

// RemoteCall implements core.RemoteAttribution.
type RemoteCall struct {
	e     *Emitter
	led   *ledger.Ledger
	req   core.Request
	route string
	start time.Time

	mu         sync.Mutex
	finished   bool
	dispatched bool
	base       string
	host       string
	nodeID     string
	fleetJob   string
	jobID      string
	engine     string
	model      string
	requester  string
	created    int64
	started    int64
}

// NewRemoteCall begins attributing a remote call of req. e and led may each be nil (no PAIR card,
// no ledger row). route is the normalized route the caller asked for.
func NewRemoteCall(e *Emitter, led *ledger.Ledger, req core.Request, route string) *RemoteCall {
	return &RemoteCall{e: e, led: led, req: req, route: route, start: time.Now()}
}

var _ core.RemoteAttribution = (*RemoteCall)(nil)

// remoteModel is the model a remote call's card names before the node has said which seat it used.
func remoteModel(task string) string {
	if EngineFor(task, "") == "hyperframes" {
		return "hyperframes"
	}
	return task
}

// Dispatched opens the call's card, queued, on the node named by the dispatch URL (Event.Node) and
// the fleet node id its health reported (an alias). Only the first call counts.
func (c *RemoteCall) Dispatched(base, nodeID, fleetJobID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.finished || c.dispatched {
		c.mu.Unlock()
		return
	}
	c.dispatched = true
	c.base, c.nodeID, c.fleetJob = base, nodeID, fleetJobID
	c.host = NodeName(base, nodeID)
	task := string(c.req.Task)
	c.engine = EngineFor(task, "")
	c.model = remoteModel(task)
	c.requester = Requester(ledger.ProcessOrigin().Session)
	c.created = time.Now().UnixMilli()
	c.jobID = fleetJobID
	if c.jobID == "" && c.e != nil {
		c.jobID = fmt.Sprintf("remote-%d-%d", c.created, c.e.seq.Add(1))
	}
	ev := c.eventLocked("queued")
	c.mu.Unlock()
	c.e.Emit(ev)
}

// Running turns the card running, once, when the node reports the job started.
func (c *RemoteCall) Running() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.finished || !c.dispatched || c.started != 0 {
		c.mu.Unlock()
		return
	}
	c.started = time.Now().UnixMilli()
	ev := c.eventLocked("running")
	c.mu.Unlock()
	c.e.Emit(ev)
}

// Finish ends the call: one terminal frame when a node was chosen, and always one asker ledger row.
func (c *RemoteCall) Finish(res core.Result) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return
	}
	c.finished = true
	failed := res.Deferred || !res.OK
	// A node that answered ran the job, whatever the polls happened to see: a completed card with no
	// start would read "never started". Except a node that answered that another job held its card
	// (core.CardHeld): that job never ran, and "never started" is the true reading of its card.
	if c.started == 0 && c.dispatched && res.Meta.Node != "" && !(failed && core.CardHeld(res.Meta.ErrClass)) {
		c.started = c.created
	}
	var ev Event
	card := c.dispatched
	if card {
		// started is false whatever Running saw: a remote card turns running when the node ADMITS the
		// job (a media job is claimed to running at admission, before its lane waits for the card), so
		// it says nothing about whether the card was ever held. The class decides.
		state, errText := cardOutcome(failed, res.Meta.ErrClass, res.Reason, false)
		if res.Meta.Model != "" {
			c.model = res.Meta.Model
		}
		ev = c.eventLocked(state)
		ev.Error = errText
		ev.CompletedAt = time.Now().UnixMilli()
	}
	row := ledger.Entry{
		Task:         string(c.req.Task),
		TokensIn:     res.Meta.TokensIn,
		TokensOut:    res.Meta.TokensOut,
		LatencyMs:    time.Since(c.start).Milliseconds(),
		CacheHit:     res.Meta.CacheHit,
		Deferred:     failed,
		ModelTier:    res.Meta.Model,
		ErrClass:     res.Meta.ErrClass,
		InputChars:   len(c.req.Input),
		Door:         c.req.Door,
		Route:        c.route,
		Placement:    res.Meta.Placement,
		Node:         c.host,
		NodeID:       c.nodeID,
		FleetJobID:   c.fleetJob,
		CardByCaller: true,
	}
	if failed {
		row.Reason = res.Reason
		if row.Reason == "" {
			row.Reason = "deferred"
		}
	}
	c.mu.Unlock()
	// The row first: it is local file I/O, and the post below can take its whole bound (a PAIR on a loaded
	// box answers late, or not at all), during which the door may be killed by a client that gave up on
	// the call; the call's audit and savings row must not be lost to that wait. CardByCaller makes the
	// ledger observer skip it, so recording it posts nothing.
	if c.led != nil {
		_ = c.led.Record(row)
	}
	if card {
		// Inline: the lane returns this result to its door, which answers at once and may be killed
		// right after (see Begin), and a close on a background goroutine dies with the process.
		c.e.EmitSync(ev)
	}
}

// Bounce closes the card of an attempt a node took and handed back (it answered "another job holds my card", or that
// it could not take its lease) and arms the handle for the next node. The card closes quiet (completed, never
// started, the reason in `error`) WHATEVER the answer's err_class, because what decides is that nothing ran on that
// node, not why: a class that is not core.CardHeld (gpu_lease_unavailable) bounces the same way, and a call that a
// second node then serves must not show the first node's card red. It carries no ledger row: a call is one row,
// written by its Finish with the node that served it. The next Dispatched opens a new card (PAIR keys a card on its
// job id, and the next attempt carries a fresh one). Before a dispatch, or once the call has ended, it does nothing.
func (c *RemoteCall) Bounce(res core.Result) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.finished || !c.dispatched {
		c.mu.Unlock()
		return
	}
	// Quiet by the bounce, not by the class: a remote card turns running when the node ADMITS the job, which says
	// nothing about whether the job ran, and the class of a handed-back answer need not be a held one.
	state, errText := quietCardOutcome(res.Reason)
	ev := c.eventLocked(state)
	ev.StartedAt = 0
	ev.Error = errText
	ev.CompletedAt = time.Now().UnixMilli()
	// Armed again: the next Dispatched names the next node and opens its card.
	c.dispatched = false
	c.base, c.host, c.nodeID, c.fleetJob, c.jobID = "", "", "", "", ""
	c.started, c.created = 0, 0
	c.mu.Unlock()
	c.e.EmitSync(ev)
}

// Discard ends an attempt that is not the final answer (the auto route fell back to a local run). An
// attempt that never reached a node writes nothing; one that did is finished as a deferred call.
func (c *RemoteCall) Discard(reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	reached := c.dispatched && !c.finished
	if !reached {
		c.finished = true
	}
	c.mu.Unlock()
	if reached {
		res := core.Deferf(reason, "", core.Meta{})
		res.DeferClass = core.DeferClassInfrastructure
		c.Finish(res)
	}
}

// eventLocked is the card's frame in state; caller holds c.mu and the call was dispatched.
func (c *RemoteCall) eventLocked(state string) Event {
	return Event{
		JobID:       c.jobID,
		Model:       c.model,
		Engine:      c.engine,
		Node:        c.host,
		NodeAliases: remoteAliases(c.nodeID, c.host),
		State:       state,
		Requester:   c.requester,
		CreatedAt:   c.created,
		StartedAt:   c.started,
	}
}

func remoteAliases(nodeID, host string) []string {
	if nodeID == "" || nodeID == host {
		return nil
	}
	return []string{nodeID}
}
