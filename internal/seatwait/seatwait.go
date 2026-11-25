// Package seatwait is the CONTRACT-scoped wait for a seat that answered
// "busy".
//
// Verified against llama-swap v251's scheduler source (2026-09-02): HTTP 429
// means the model's RESERVED requests (queued + in-flight) reached its
// concurrencyLimit (default 10) — it is sent with `Retry-After: 1` and the
// body `code:"concurrency_limit"`. A process that is not up yet answers 503
// "process is not ready", and a health-check timeout answers 500 with
// src:"llama-swap". None of these means the seat is broken; they mean PEERS
// hold it — every parallel session's MCP process fans out against the same
// limit. Before this package the harness filed them as
// DeferClassInfrastructure and the CALLER re-routed the work to a weaker seat
// by hand (2026-09-01: several sessions, the operator's #1 priority).
//
// One Budget per RunAgentTask, carried in ctx and shared by every chat and
// re-pack call of that contract: a 10-step loop cannot burn ten budgets, and
// a server that always says Retry-After: 1 still runs out. The budget is the
// wait's whole authority — no attempt count, no per-call timer.
package seatwait

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBudget is the wait a contract may spend on a peer-held seat before
// the failure stands. Long enough for a fan-out's generations to drain (a
// slot frees only when a FULL generation finishes), short enough that a wedged
// seat is still noticed well inside one contract's wall.
const DefaultBudget = 90 * time.Second

// ladder is the sleep used when the server sends no usable Retry-After: it
// grows to a 15 s cap rather than forever, because a 429 clears in seconds
// once one peer's generation ends, and a long sleep just wastes the slot that
// freed up.
var ladder = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second}

// Budget is one contract's wait allowance. The zero Budget never waits.
type Budget struct {
	mu         sync.Mutex
	max        time.Duration
	spent      time.Duration
	attempts   int
	lastStatus int
	sleeping   bool // a reserved sleep is in progress (Sleep on this budget)
	gate       SeatGate
}

// SeatGate answers, before the budget waits out a busy answer other than 429,
// whether the seat is NOT SERVING right now (ADR 0066). It reads the seat, so it may
// take a moment and must be safe to call from any client of the contract.
type SeatGate func(status int) (down bool)

// WithSeatGate installs the gate: while it says the seat is not serving, NextFor
// refuses the wait. A 5xx from llama-swap on a seat whose engine died or cannot start
// is the seat's failure, not peers holding its slots — sleeping the contention budget
// on it (90 s by default) only delays the run's typed outcome. A 429 is contention by
// definition and is never asked. It is the seam every client of the budget shares,
// the structured re-pack's included, so none of them needs its own check. Returns b.
func (b *Budget) WithSeatGate(g SeatGate) *Budget {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	b.gate = g
	b.mu.Unlock()
	return b
}

// NewBudget builds a Budget from the config value in seconds: 0 = the
// DefaultBudget, negative = disabled (the pre-2026-09-02 behaviour: the first
// busy answer stands).
func NewBudget(cfgSec int) *Budget {
	switch {
	case cfgSec < 0:
		return &Budget{}
	case cfgSec == 0:
		return &Budget{max: DefaultBudget}
	}
	return &Budget{max: time.Duration(cfgSec) * time.Second}
}

// Next reserves the next sleep. A server Retry-After (whole seconds) wins over
// the ladder; BOTH count against the budget. ok=false means the caller must
// stop retrying and let the failure stand — the reservation is not made.
func (b *Budget) Next(retryAfter string) (time.Duration, bool) {
	return b.NextFor(0, retryAfter)
}

// NextFor is Next with the busy STATUS that triggered it, remembered as
// LastStatus so a defer reason can name what the seat answered.
func (b *Budget) NextFor(status int, retryAfter string) (time.Duration, bool) {
	if b == nil {
		return 0, false
	}
	b.mu.Lock()
	if status != 0 {
		b.lastStatus = status
	}
	gate := b.gate
	b.mu.Unlock()
	// The seat is asked outside the lock (it reads the seat), and BEFORE the budget's
	// own bookkeeping: a budget that never waits still has to say why the failure stands.
	if gate != nil && status != 0 && status != 429 && gate(status) {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.max <= 0 {
		return 0, false
	}
	var d time.Duration
	if s, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && s > 0 {
		d = time.Duration(s) * time.Second
	} else if b.attempts < len(ladder) {
		d = ladder[b.attempts]
	} else {
		d = ladder[len(ladder)-1]
	}
	if b.spent+d > b.max {
		return 0, false
	}
	b.spent += d
	b.attempts++
	return d, true
}

// Spent is the wait this contract has reserved so far — the number the wire
// result reports as contention_wait_sec.
func (b *Budget) Spent() time.Duration {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// LastStatus is the most recent busy status waited out (0 = none).
func (b *Budget) LastStatus() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastStatus
}

// Attempts is how many busy answers were waited out.
func (b *Budget) Attempts() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

type ctxKey struct{}

// WithBudget attaches the contract's Budget to ctx for every seat call below.
func WithBudget(ctx context.Context, b *Budget) context.Context {
	return context.WithValue(ctx, ctxKey{}, b)
}

// FromContext returns the attached Budget, or a disabled one (never waits)
// when the caller attached nothing — so every seat client can call it
// unconditionally.
func FromContext(ctx context.Context) *Budget {
	if b, _ := ctx.Value(ctxKey{}).(*Budget); b != nil {
		return b
	}
	return &Budget{}
}

// Attached reports whether the caller attached a Budget: it runs under a contract, whose own rules decide what a lost
// answer means (ADR 0066). A call without one is on its own and may use a Resend.
func Attached(ctx context.Context) bool {
	b, _ := ctx.Value(ctxKey{}).(*Budget)
	return b != nil
}

// Retryable reports whether a non-200 is llama-swap saying "peers hold the
// seat" — the three answers verified against its source — keyed on the STATUS
// plus the smallest body discriminator that separates them from a real fault
// (a 500 from CUDA OOM is not a wait; a 502 is a generation that died
// mid-body and must not be silently re-sent).
func Retryable(status int, body string) bool {
	switch status {
	case 429:
		return true
	case 503:
		return strings.Contains(body, "process is not ready")
	case 500:
		return strings.Contains(body, `"src":"llama-swap"`) || strings.Contains(body, "health check timed out")
	case 502:
		// An EMPTY 502 is llama-swap's proxy failing to reach an upstream that is
		// between states (2026-09-04: the seat's engine had been stopped, the
		// swap still listed it ready for its stub's 30-s grace, and 8 contracts
		// deferred in 34 ms each as "lost_to_stack"). No generation happened, so
		// waiting on the same budget as a 429 is correct. A 502 WITH a body is
		// still a generation that died mid-stream — never retried.
		return strings.TrimSpace(body) == ""
	}
	return false
}

// DeathMarker is llama-swap's own wording for a start whose engine exited
// ("upstream command exited prematurely", process_command.go in upstream's
// source): the one body that says a start died.
const DeathMarker = "upstream command exited"

// startRefusedMarker is the wording of a 502 for a start that could not be
// completed ("unable to start process: ..."), as this repo's tests record it. It
// was not found in the current upstream source, so it is a recorded shape, not a
// quoted one.
const startRefusedMarker = "unable to start process"

// StartFailed reports whether a refusal's body says the seat's process did not
// start: the engine exited, or llama-swap was "unable to start process". Re-sending
// that request is a second failed start, whatever the status code says.
func StartFailed(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, DeathMarker) || strings.Contains(lower, startRefusedMarker)
}

// SwapMarker and ShutdownMarker are llama-swap's own words for a request it was still holding when the model was
// taken away: "<router>: model unloaded" and "<router> is shutting down" (a config reload closed the router; read in
// the v236 and v256 sources). The router is `matrix` or `group`, so both matches start after its name.
const (
	SwapMarker     = "model unloaded"
	ShutdownMarker = " is shutting down"
)

// TakenAway reports whether a refusal means the model went away under the request, so that sending it again loads it
// back and answers it. There are two shapes, and which one a request gets depends on where it stood when the model went:
//
//   - llama-swap was still holding it: the markers above.
//   - the engine was working on it: a 5xx with no body, which is llama-swap's answer when its connection to the engine
//     drops. An unload gives the engine 10 s and then ends it, and every request on it is answered that way (measured on
//     v273 on 2026-11-25, a text and a speech model unloaded by hand: 502, no body, "proxy error: EOF" in its log). An
//     engine that crashed looks the same; the caller's bound is what keeps a crash from being asked for again and again.
//
// An answer that says a start died (StartFailed) is neither: asking again is a second failed start.
func TakenAway(status int, body string) bool {
	if status < 500 {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(body))
	if lower == "" {
		return true
	}
	return (strings.Contains(lower, SwapMarker) || strings.Contains(lower, ShutdownMarker)) && !StartFailed(body)
}

// maxResends is how many times one call may be sent again after its model was swapped out under it: the
// figure an agent run's seat recoveries use. Two swaps under one call are bad luck; a third is something
// that keeps taking the model away, and the caller is told.
const maxResends = 2

// ResendPause is the pause before a call is sent again, so a reloading llama-swap has its new router up. A var
// so a test compresses it.
var ResendPause = time.Second

// Resend is one call's allowance to be sent again after TakenAway. The call's own time starts when it is first
// sent: waiting for a card or a slot is bounded by the waits themselves.
type Resend struct {
	began time.Time
	limit time.Duration
	n     int
}

// NewResend is the allowance of one call that may last limit in all (0 = its context alone bounds it).
func NewResend(limit time.Duration) *Resend { return &Resend{limit: limit} }

// Again reports whether the call may be sent once more, and sleeps ResendPause when it may. sentAt is when the
// lost attempt went out. The call's own time must still hold what that attempt took: an answer lost at minute
// 25 of 30 is not asked for again in the 5 that are left.
func (r *Resend) Again(ctx context.Context, sentAt time.Time) bool {
	if r.began.IsZero() {
		r.began = sentAt
	}
	if r.n >= maxResends || ctx.Err() != nil {
		return false
	}
	now := time.Now()
	if r.limit > 0 && now.Add(now.Sub(sentAt)+ResendPause).After(r.began.Add(r.limit)) {
		return false
	}
	if Sleep(ctx, ResendPause) != nil {
		return false
	}
	r.n++
	return true
}

// Bound is ctx cut at the end of the call's own time, for a re-send: an http.Client's timeout starts over with
// every request, so without it a call could last its timeout once per attempt.
func (r *Resend) Bound(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.n == 0 || r.limit <= 0 {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, r.began.Add(r.limit))
}

// Note says on a final error how often the call was sent again ("" when it was not).
func (r *Resend) Note() string {
	if r.n == 0 {
		return ""
	}
	return fmt.Sprintf(" (sent again %d time(s))", r.n)
}

// Err is err with Note, and err itself for a call that was sent once.
func (r *Resend) Err(err error) error {
	if err == nil || r.n == 0 {
		return err
	}
	return fmt.Errorf("%w%s", err, r.Note())
}

// Sleeping reports whether a reserved sleep is in progress right now — the
// fact a wall-timeout classifier needs: "did the wall expire WHILE waiting
// on peers?" is a different question from "was there ever a busy answer?".
func (b *Budget) Sleeping() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sleeping
}

// Sleep is Sleep with the in-flight flag kept on this budget for the duration.
func (b *Budget) Sleep(ctx context.Context, d time.Duration) error {
	if b != nil {
		b.mu.Lock()
		b.sleeping = true
		b.mu.Unlock()
		defer func() {
			b.mu.Lock()
			b.sleeping = false
			b.mu.Unlock()
		}()
	}
	return Sleep(ctx, d)
}

// CausedTimeout reports whether contention, not the model's own work, is what
// spent a wall of length `wall`: a sleep was in flight when it expired, or the
// reserved waits account for at least half of it. A single early 429 that
// resolved in a second must NOT relabel a genuine budget timeout 15 minutes
// later as "seat contended" (review finding, 2026-09-02).
func (b *Budget) CausedTimeout(wall time.Duration) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastStatus == 0 {
		return false
	}
	return b.sleeping || (wall > 0 && b.spent*2 >= wall)
}

// Sleep waits d, or until ctx ends, whichever comes first.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
