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
	defer b.mu.Unlock()
	if status != 0 {
		b.lastStatus = status
	}
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
	}
	return false
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
