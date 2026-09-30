// progress.go reports a delegation call's progress to the MCP client (ADR 0065).
//
// A delegation is silent for minutes. The MCP spec lets a server send
// `notifications/progress` for a request whose caller supplied a progress token
// (`_meta.progressToken`), and a client that resets its own timeout on progress
// keeps waiting instead of aborting the call. Whether the reference client honours
// that is UNVERIFIED — it is why the whole-call deadline is a hard limit that does
// not depend on it — so this is strictly opt-in and strictly additive: no token, no
// session, or no callback means nothing is sent and nothing changes.
//
// What is sent: an opening notification, one per subtask state change (started,
// finished and how), and a heartbeat every progressHeartbeat while nothing changes.
// `progress` is a running counter (the spec asks for a strictly increasing value,
// and a heartbeat has no new work to count); everything a reader needs is in the
// message.

package mcpserver

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dmmdea/offload-harness/internal/delegate"
)

// progressHeartbeat is how often a delegation call that carries a progress token
// repeats itself while nothing changes (a var so tests compress it).
var progressHeartbeat = 30 * time.Second

const (
	// progressSendTimeout bounds one notification write: a client that stops
	// reading must not wedge the reporter, and through it the handler.
	progressSendTimeout = 5 * time.Second
	// progressStopWait bounds how long the handler waits for the reporter to wind
	// down before it returns its result.
	progressStopWait = 2 * time.Second
	// progressQueue is the event buffer between the engine's goroutines and the
	// reporter; an event that does not fit is dropped (the next heartbeat carries
	// the state).
	progressQueue = 64
)

// progressReporter sends one call's progress notifications.
type progressReporter struct {
	session  *mcp.ServerSession
	token    any
	total    int
	deadline time.Time
	begun    time.Time

	events chan delegate.ProgressEvent
	quit   chan struct{}
	done   chan struct{}
	closed atomic.Bool
	warn   sync.Once

	// seq and lastDone belong to the run loop alone.
	seq      float64
	lastDone int
}

// startProgress begins reporting a delegation call's progress and returns the
// function that ends it (always safe to call, and to defer). It hooks the engine
// through opts.OnProgress. total is how many subtasks the call owes; deadline is
// the call's deadline (zero = none).
//
// It does nothing — and returns a no-op — unless the request carried a progress
// token and has a live session: the spec forbids progress for a request that did
// not ask for it, and a unit test's bare request has no session at all.
func (s *Server) startProgress(ctx context.Context, req *mcp.CallToolRequest, opts *delegate.RunOptions, total int, deadline time.Time) (stop func()) {
	noop := func() {}
	if req == nil || req.Params == nil || req.Session == nil {
		return noop
	}
	token := req.Params.GetProgressToken()
	if token == nil {
		return noop
	}
	p := &progressReporter{
		session: req.Session, token: token, total: total, deadline: deadline, begun: time.Now(),
		events: make(chan delegate.ProgressEvent, progressQueue), quit: make(chan struct{}), done: make(chan struct{}),
	}
	opts.OnProgress = p.observe
	go p.run(ctx)
	return p.stop
}

// observe is the engine's callback. It runs on the engine's goroutines and never
// blocks them: an event that finds the queue full is dropped.
func (p *progressReporter) observe(ev delegate.ProgressEvent) {
	if p.closed.Load() {
		return
	}
	select {
	case p.events <- ev:
	default:
	}
}

// stop ends the reporter. It waits (bounded) for an in-flight notification to be
// written, so no notification is sent once the handler has returned its result.
func (p *progressReporter) stop() {
	if p.closed.Swap(true) {
		return
	}
	close(p.quit)
	select {
	case <-p.done:
	case <-time.After(progressStopWait):
	}
}

func (p *progressReporter) run(ctx context.Context) {
	defer close(p.done)
	tick := time.NewTicker(progressHeartbeat)
	defer tick.Stop()
	p.send(ctx, p.opening())
	for {
		select {
		case ev := <-p.events:
			p.lastDone = ev.Done
			p.send(ctx, p.describe(ev))
		case <-tick.C:
			p.send(ctx, p.heartbeat())
		case <-p.quit:
			return
		}
	}
}

// send writes one notification, bounded, and never fails the call: progress is
// courtesy, and a client that dropped its end is the client's business.
func (p *progressReporter) send(ctx context.Context, msg string) {
	if p.closed.Load() {
		return
	}
	p.seq++
	sctx, cancel := context.WithTimeout(ctx, progressSendTimeout)
	defer cancel()
	err := p.session.NotifyProgress(sctx, &mcp.ProgressNotificationParams{ProgressToken: p.token, Progress: p.seq, Message: msg})
	if err != nil {
		p.warn.Do(func() {
			log.Printf("mcpserver: a progress notification could not be sent (further failures are not logged): %v", err)
		})
	}
}

func (p *progressReporter) opening() string {
	return fmt.Sprintf("delegating %d subtask(s)%s", p.total, p.deadlineNote())
}

func (p *progressReporter) describe(ev delegate.ProgressEvent) string {
	if ev.Kind == "started" {
		return fmt.Sprintf("subtask %d of %d started", ev.Index+1, ev.Total)
	}
	where := ""
	if ev.Node != "" {
		where = " on " + ev.Node
	}
	return fmt.Sprintf("subtask %d of %d finished (%s)%s; %d of %d done", ev.Index+1, ev.Total, ev.Outcome, where, ev.Done, ev.Total)
}

func (p *progressReporter) heartbeat() string {
	return fmt.Sprintf("still working: %d of %d subtasks done after %s%s",
		p.lastDone, p.total, time.Since(p.begun).Round(time.Second), p.deadlineNote())
}

// deadlineNote says how long the call has left, or that it has no deadline.
func (p *progressReporter) deadlineNote() string {
	if p.deadline.IsZero() {
		return "; no call deadline"
	}
	left := time.Until(p.deadline).Round(time.Second)
	if left < 0 {
		left = 0
	}
	return fmt.Sprintf("; call deadline in %s", left)
}
