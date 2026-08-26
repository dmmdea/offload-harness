// Package modelaffinity is the process-local admission gate that stops two text
// lanes from thrashing one llama-swap by asking it for different models at once.
//
// THE INCIDENT. The agent seat (qwen3.8-27b) degraded ~4x — 72s to 307s per
// call — because llama-swap was loading a DIFFERENT model into the same serving
// slot while it worked. Every text seat in the default config points at one
// endpoint (agent_model, model, reasoning_model, escalation_model, triage_model,
// vision_model, all on http://127.0.0.1:11436), and llama-swap serializes model
// residency: two lanes naming different models force an evict-and-reload. The
// harness had nothing that serialized by model — mediaSlot is media-only,
// gpulease's ClassMedia never covers interactive text, sttclient's inferMu
// guards a different process, pipeline's swapMu guards a timestamp map, and
// internal/mcpserver has no limiter at all.
//
// WHAT THIS DOES. It converts thrash into ordered batching:
//
//   - Requests naming the SAME model on the SAME base proceed concurrently.
//     llama-swap already queues those harmlessly (see pipeline.breakerFailure's
//     design note: "llama-swap QUEUES incoming requests while it loads a
//     model"), so serialising them would be a pure regression. The expensive
//     event is the SWITCH, not the overlap.
//   - A request naming a DIFFERENT model on a base that has in-flight requests
//     parks until they drain, then proceeds. N interleaved switches become one
//     switch per batch.
//
// KEYED ON THE RESOLVED BASE, NEVER ON THE MODEL ALONE. llamaclient's
// resolveEndpoint (internal/llamaclient/lanes.go) can return a different base
// per model — a seat_endpoints pin, a busy-aware cascade_remote_lane, or the
// default. Two models served by two llama-swap instances do not contend at all.
// Keying on the model would serialise lanes that never conflicted; keying on the
// base is what makes the gate match the physical resource. Callers pass the base
// resolveEndpoint already decided — this package never re-decides it.
//
// WHAT IT IS NOT. This is not a GPU lease. internal/gpulease deliberately
// excludes ordinary interactive text ("thousands per day at ~46ms, and leasing
// them is untenable... a known limit, not an oversight"), and that judgement
// stands: a filesystem lease per text request is off the table. This gate is
// two uncontended mutex acquisitions and no allocation on the fast path.
//
// CROSS-PROCESS LIMIT — STATED PLAINLY. The registry lives in this process. Two
// harness processes on one box (an MCP server and a CLI invocation, two MCP
// servers under two editors) still thrash each other exactly as before: neither
// can see the other's in-flight set. Making it machine-wide would require the
// same fenced, pid-recycle-safe, machine-wide state root gpulease uses — an
// acquire/release round trip through the filesystem on EVERY text call, which is
// the cost gpulease refused for text. Nothing here narrows that gap; it only
// fixes the common case, which is one harness process with several concurrent
// lanes inside it (delegate's fan-out, the MCP server's concurrent tool calls,
// the agent loop running beside a cascade call).
//
// RESIDUAL, NAMED. Two llama-swap routes outside this gate can still force a
// load: internal/agent's /upstream/{model}/props probes (window.go, props.go —
// ProbeSeatPin exists precisely to warm a seat, so gating it would fight its
// purpose) and internal/tokclient's /upstream/{model}/tokenize. Both are affine
// to their own caller's seat and neither is a burst source, but a tokenize is a
// separate admission from the generation that follows it, so gating it could not
// batch the two anyway — holding one admission across tokenize-then-generate is
// a different, larger change through internal/pipeline.
package modelaffinity

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// defaultBudget is the per-batch drain allowance used when a caller passes a
// non-positive budget (an http.Client with no Timeout). It matches the config
// default request_timeout_sec = 120 (internal/config.Defaults), so the fallback
// is the same order as the real thing rather than an invented number.
const defaultBudget = 120 * time.Second

// Ticket is the admission a caller holds while its request is in flight. The
// zero Ticket is a no-op, so `defer tk.Release()` is safe on the error path.
type Ticket struct{ g *gate }

// Release ends the caller's in-flight window and, when it was the last of its
// batch, hands the base to whoever is parked. It is safe on a zero Ticket and
// must be called exactly once per successful Admit.
func (t Ticket) Release() {
	if t.g != nil {
		t.g.release()
	}
}

// waiter is one parked caller. admitted is CLOSED by whoever promotes the
// waiter's batch, and the promoter increments inflight on the waiter's behalf
// before closing — so a waiter that wakes already holds its admission and can
// never be handed a slot that then evaporates.
type waiter struct {
	model    string
	admitted chan struct{}
}

// gate is one llama-swap's serving slot as this process sees it.
//
// INVARIANT (holds whenever mu is not held): inflight == 0 implies len(queue)
// == 0. It is maintained inductively — release() promotes whenever inflight
// reaches zero, and Admit only appends to queue when inflight > 0 (its first
// branch takes the idle case, and by the invariant a non-empty queue already
// implies inflight > 0). This is what makes "park behind the queue" safe: there
// is always someone in flight whose release will come back and promote.
type gate struct {
	mu       sync.Mutex
	current  string // the model this base's in-flight batch names
	inflight int
	queue    []*waiter
}

var (
	registryMu sync.RWMutex
	registry   = map[string]*gate{}
)

// gateFor returns the gate for base, creating it once. The map is keyed by the
// RESOLVED base URL and is bounded by the number of endpoints the process ever
// talks to (one per llama-swap), so it does not grow with traffic.
func gateFor(base string) *gate {
	registryMu.RLock()
	g, ok := registry[base]
	registryMu.RUnlock()
	if ok {
		return g
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if g, ok = registry[base]; ok {
		return g
	}
	g = &gate{}
	registry[base] = g
	return g
}

// Admit takes an admission for one generation request against base naming
// model, and blocks only when admitting it now would force llama-swap to swap
// models out from under work already in flight.
//
// budget is how long ONE request against this base is allowed to take
// end-to-end — in production the resolved http.Client's Timeout. It is not how
// long the caller waits; it is the unit the wait bound is built from (see
// waitBound). ctx bounds the wait too, and in production it is usually the
// tighter of the two.
//
// On success the caller MUST call Release on the returned Ticket exactly once.
// On failure the error is always a *WaitError.
func Admit(ctx context.Context, base, model string, budget time.Duration) (Ticket, error) {
	if budget <= 0 {
		budget = defaultBudget
	}
	g := gateFor(base)

	g.mu.Lock()
	// Idle base: take it, name it, go. This is the overwhelmingly common path.
	if g.inflight == 0 {
		g.current = model
		g.inflight = 1
		g.mu.Unlock()
		return Ticket{g: g}, nil
	}
	// Running batch of the same model with nobody parked: join it. llama-swap
	// queues these harmlessly and no switch is involved.
	//
	// The `len(g.queue) == 0` half is the anti-starvation barrier and is
	// load-bearing: without it a steady stream of the resident model would
	// keep inflight above zero forever and the parked switch would never run.
	// It also CLOSES the batch at the instant someone parks, which is what
	// makes waitBound provable.
	if g.current == model && len(g.queue) == 0 {
		g.inflight++
		g.mu.Unlock()
		return Ticket{g: g}, nil
	}
	// Park. Snapshot what we are waiting behind for the error message, and fix
	// the bound now — it is wall-clock from this instant and is never extended
	// by later progress.
	ahead := g.batchesAheadLocked()
	bound := waitBound(ahead, budget)
	held, live := g.current, g.inflight
	w := &waiter{model: model, admitted: make(chan struct{})}
	g.queue = append(g.queue, w)
	g.mu.Unlock()

	start := time.Now()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	var cause error
	select {
	case <-w.admitted:
		return Ticket{g: g}, nil
	case <-timer.C:
		cause = context.DeadlineExceeded
	case <-ctx.Done():
		cause = ctx.Err()
	}

	// Withdraw. The re-check under the lock is not decoration: a promotion can
	// close admitted between our wake-up and our acquiring mu, and that
	// promotion already incremented inflight for us. Dropping it there would
	// leak a slot that nobody ever releases and wedge the base permanently.
	g.mu.Lock()
	select {
	case <-w.admitted:
		g.mu.Unlock()
		return Ticket{g: g}, nil
	default:
	}
	for i, x := range g.queue {
		if x == w {
			g.queue = append(g.queue[:i], g.queue[i+1:]...)
			g.queue = g.queue[:len(g.queue):len(g.queue)]
			break
		}
	}
	g.mu.Unlock()
	return Ticket{}, &WaitError{
		Base:         base,
		Want:         model,
		Held:         held,
		InFlight:     live,
		BatchesAhead: ahead,
		Waited:       time.Since(start),
		Bound:        bound,
		cause:        cause,
	}
}

// batchesAheadLocked counts the model switches that must happen before this
// caller can run: the batch in flight, plus one per DISTINCT model already
// parked. Because the barrier in Admit stops anyone joining or jumping the
// queue once a waiter exists, this count can only shrink after it is taken —
// which is what lets waitBound be a fixed wall-clock figure rather than one
// that resets on progress.
func (g *gate) batchesAheadLocked() int {
	n := 0
	if g.inflight > 0 {
		n++
	}
	seen := make(map[string]struct{}, len(g.queue))
	for _, w := range g.queue {
		if _, dup := seen[w.model]; dup {
			continue
		}
		seen[w.model] = struct{}{}
		n++
	}
	return n
}

// waitBound is the wall-clock ceiling on one park, fixed at park time.
//
// WHY THIS IS SUFFICIENT, not a guess. Each batch ahead drains within one
// budget of the moment it starts: every member of a batch is a single HTTP
// request bounded by that same budget (http.Client.Timeout covers the body read
// too), and the barrier means a batch takes no new members after a waiter
// parks. So batchesAhead x budget covers every drain this caller must wait
// through, and the extra budget is slack for the hand-off itself. If the wait
// still exhausts, no amount of further waiting helps: a release was lost, which
// is a defect to report, not congestion to sit through.
func waitBound(batchesAhead int, budget time.Duration) time.Duration {
	if batchesAhead < 0 {
		batchesAhead = 0
	}
	return time.Duration(batchesAhead+1) * budget
}

// release ends one admission and, when the batch is empty, promotes the next.
func (g *gate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight > 0 {
		g.inflight--
	}
	if g.inflight == 0 {
		g.promoteLocked()
	}
}

// promoteLocked hands an idle base to the oldest parked caller and to every
// other parked caller naming the SAME model — one switch, then as much
// concurrency as that model has waiting.
//
// Fairness comes from taking the model from the HEAD of the queue: the oldest
// waiter is always in the promoted batch, so every waiter reaches the head in
// bounded time and no lane can be starved by a busier one. Sweeping the whole
// queue for that model (rather than only a contiguous run) maximises batching
// without touching that guarantee, because it never changes WHICH model goes
// next.
func (g *gate) promoteLocked() {
	if len(g.queue) == 0 {
		return
	}
	next := g.queue[0].model
	g.current = next
	kept := g.queue[:0]
	for _, w := range g.queue {
		if w.model == next {
			g.inflight++
			close(w.admitted)
			continue
		}
		kept = append(kept, w)
	}
	for i := len(kept); i < len(g.queue); i++ {
		g.queue[i] = nil // drop references so promoted waiters can be collected
	}
	g.queue = kept
}

// WaitError is the outcome of an exhausted admission wait. It is a distinct
// type, with the base, the model wanted, and the model that held the slot,
// because "timed out" alone sends the reader looking at the model instead of at
// the contention that actually happened.
type WaitError struct {
	Base         string // the resolved llama-swap base the wait was on
	Want         string // the model this request named
	Held         string // the model whose batch held the base when we parked
	InFlight     int    // how many requests that batch had, when we parked
	BatchesAhead int    // model switches that had to happen before ours
	Waited       time.Duration
	Bound        time.Duration
	cause        error // context.DeadlineExceeded (our bound) or the caller's ctx.Err()
}

// Error names the contention. It deliberately contains the word "timeout":
// internal/pipeline.classifyErr buckets infra errors by substring and files
// anything unrecognised as "other", and an exhausted affinity wait is
// congestion, which that classifier spells "timeout". The wording is pinned by
// test so a reword cannot silently reclassify it in the ledger.
func (e *WaitError) Error() string {
	return fmt.Sprintf(
		"model-affinity timeout after %s (bound %s) on %s: wanted model %q, but %q held the serving slot with %d request(s) in flight and %d model switch(es) queued ahead",
		e.Waited.Round(time.Millisecond), e.Bound, e.Base, e.Want, e.Held, e.InFlight, e.BatchesAhead)
}

// Unwrap exposes the cause so errors.Is(err, context.DeadlineExceeded) and
// errors.Is(err, context.Canceled) keep working for callers that branch on them.
func (e *WaitError) Unwrap() error { return e.cause }

// queueDepth and inFlight are test observation points: they let a test prove a
// waiter has REACHED the gate before advancing, instead of sleeping and hoping.
// Not part of the production surface.
func queueDepth(base string) int {
	g := gateFor(base)
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.queue)
}

func inFlight(base string) int {
	g := gateFor(base)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflight
}
