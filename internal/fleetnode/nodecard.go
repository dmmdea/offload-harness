package fleetnode

import (
	"net/http"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
	"github.com/dmmdea/offload-harness/internal/pairworkloads"
)

// Who asked a job, and who cards it (PAIR routing fixes, D7/D9/D11).
//
// A fleet node stamps work it runs FOR another box with the door "fleet", and the box that asked
// reports it (pairworkloads.FleetDoor). That fails when the asker reports nothing: a thin client, a
// scratch `install client` config, a box that is not a PAIR member. Such an asker says so on the
// request with core.PairCardHeader = "node" (it sends it ONLY when its own emitter is not enabled, so
// an older asker, which sends nothing, is never carded twice), and this node then emits the job's one
// card itself, from its own emitter, so an in-flight card of a killed fleet-serve still gets an
// orphan marker and is closed.
//
// Independently, the asker's name (core.AskerHeader) is recorded on the node's ledger row whenever the
// header is present, so the node's ledger says who each job was for.

// askerOf reads the attribution headers of a request that creates work: the asker's name (untrusted,
// so sanitized and bounded) and whether the asker asked this node to card the job.
func askerOf(r *http.Request) (asker string, nodeCards bool) {
	return core.SanitizeAsker(r.Header.Get(core.AskerHeader)), r.Header.Get(core.PairCardHeader) == core.PairCardNode
}

// originOf reads the origin headers of a request that creates work: the asker's session label and run class
// (ADR 0085), each cleaned by the ledger's own rule, so a label valid in the asker's own Record is never dropped
// here and one the ledger would refuse never lands on a row. A value over the cap is refused whole, not cut like
// the tenant is: a cut session id would name another session.
//
// trustClass says the request is one this node can authenticate (trustsWire). Only then is the class honoured: a
// labelled row is left out of every production view, so a class from a caller the node cannot identify would let
// any peer that reaches the port hide this node's rows. The same caller's session is kept as a plain label, but
// not one that carries a class of its own ("bake:run-7", the go-test label), because the ledger reads a class off
// such a label when the row names none.
func originOf(r *http.Request, trustClass bool) (session, class string) {
	session = ledger.CleanOrigin(r.Header.Get(core.OriginHeader))
	if !trustClass {
		if session != "" && ledger.ClassOfSession(session) != "" {
			session = ""
		}
		return session, ""
	}
	return session, ledger.CleanClass(r.Header.Get(core.RunClassHeader))
}

// trustsWire reports whether the request is one this node can authenticate, so the run class it carries may be
// believed. A token-gated lane has already passed authorizeGated (a bearer when a token is configured, else a
// loopback listener, the trust boundary of the local MCP surface); any other lane (the media task types and the
// legacy path-taking stt lane are open on purpose, so deployed tokenless clients keep working) is trusted only
// when a token is configured and the request presented it.
func (s *Server) trustsWire(r *http.Request, taskType string) bool {
	if tokenGated(s.opts.Cfg, taskType) {
		return true
	}
	return s.opts.Cfg.FleetAuthToken != "" && bearerOK(r, s.opts.Cfg.FleetAuthToken)
}

// nodeCard is the serving node's one PAIR card for a job its asker will not card. Every method is a
// no-op on a nil receiver, which is what newNodeCard returns whenever no card is due.
type nodeCard struct {
	e         *pairworkloads.Emitter
	jobID     string
	model     string
	engine    string
	requester string
	created   int64

	mu      sync.Mutex
	started int64
	done    bool
}

// newNodeCard returns the card of job jobID of harness task task (model: the seat or family it was
// admitted for, "" if unknown), or nil when none is due: the asker did not signal. A node whose
// emitter is not enabled (the key is off, or PAIR is not installed here) gets a card whose frames
// the emitter drops, so the one gate on enablement is the emitter's own.
func (s *Server) newNodeCard(task, model, jobID, asker string, signalled bool) *nodeCard {
	if !signalled || jobID == "" {
		return nil
	}
	if model == "" {
		model = task
	}
	who := "fleet"
	if asker != "" {
		who += ":" + asker
	}
	return &nodeCard{
		e: s.opts.Pair, jobID: jobID, model: model,
		engine:    pairworkloads.EngineFor(task, model),
		requester: pairworkloads.Requester(who),
		created:   time.Now().UnixMilli(),
	}
}

func (c *nodeCard) event(state string) pairworkloads.Event {
	return pairworkloads.Event{
		JobID: c.jobID, Model: c.model, Engine: c.engine, // Node empty: this box
		State: state, Requester: c.requester, CreatedAt: c.created, StartedAt: c.started,
	}
}

// queued opens the card when the job is admitted.
func (c *nodeCard) queued() {
	if c == nil {
		return
	}
	c.mu.Lock()
	ev := c.event("queued")
	c.mu.Unlock()
	c.e.Emit(ev)
}

// running turns the card running when the job starts, once.
func (c *nodeCard) running() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.done || c.started != 0 {
		c.mu.Unlock()
		return
	}
	c.started = time.Now().UnixMilli()
	ev := c.event("running")
	c.mu.Unlock()
	c.e.Emit(ev)
}

// finish closes the card with the job's outcome: failed (with the reason) for a result that is not
// OK or deferred, completed otherwise, except a job another job's hold on the card kept from running
// (core.CardHeld), which closes quiet like every other card of such a call (pairworkloads.CardOutcome);
// model, when the run reports one, replaces the admission-time guess. Once.
func (c *nodeCard) finish(res core.Result) {
	if c == nil {
		return
	}
	// started is false whatever running saw: this card turns running when the node's worker takes the
	// job (a media job is claimed to running at admission, before its lane waits for the card), so it
	// says nothing about whether the card was ever held; the class decides.
	state, errText := pairworkloads.CardOutcome(res, false)
	c.end(state, errText, res.Meta.Model)
}

// fail closes the card failed with reason (a panic, a drain that never started the job, a refused
// admission).
func (c *nodeCard) fail(reason string) {
	if c == nil {
		return
	}
	c.end("failed", reason, "")
}

func (c *nodeCard) end(state, errText, model string) {
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return
	}
	c.done = true
	if model != "" {
		c.model = model
	}
	ev := c.event(state)
	ev.Error = errText
	ev.CompletedAt = time.Now().UnixMilli()
	c.mu.Unlock()
	c.e.Emit(ev)
}

// discard ends the card without a frame: the admission was a duplicate of a job that already has its
// own card.
func (c *nodeCard) discard() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.done = true
	c.mu.Unlock()
}
