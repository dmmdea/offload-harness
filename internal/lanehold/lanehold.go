// Package lanehold is the placement half of the four single-shot lanes (sttremote, visionremote,
// textremote, forecastremote): the loop that keeps a call's place in line while a fleet node is full
// or leased and, on the auto route, while this box's own card is leased too (ADR 0105).
//
// A lane used to try its roster once. A node that answered 503 (queue full, leased, draining) sent
// the call away at once, and the auto route then queued it on this box's own leased card, where it
// gave up after the gpu wait. Nothing asked the fleet again. This package owns the one loop that
// does: pick a node and dispatch until a node accepts, the lane's own local trigger frees, or the
// bound ends, whichever comes first. The poll-and-run half of a call stays in each lane.
//
// The package knows no lane types. A lane hands it a Try (one pick plus one dispatch) and the error
// that Try returns says, through Holder, whether a place may free and which node just refused.
//
// Every wait is a select on the context, the bound and a timer; the timings are package variables
// so a test compresses them.
package lanehold

import (
	"context"
	"errors"
	"fmt"
	"log"
	mathrand "math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/delegate"
	"github.com/dmmdea/offload-harness/internal/gpulease"
	"github.com/dmmdea/offload-harness/internal/modelaffinity"
)

// The timings. PollInterval is how often a wait reads the roster again (the delegator's capacity
// wait uses the same figure); RefusalCooldown is how long a node that just refused is left alone
// whatever its health says, because an upload is megabytes and must never be re-sent into a node that
// just turned it away. CooldownJitter spreads that hold up to this fraction longer, drawn once per
// refusal, so nine callers refused by one node on the same tick do not re-upload on the same tick.
var (
	PollInterval    = 3 * time.Second
	RefusalCooldown = 10 * time.Second
	CooldownJitter  = 0.3
)

// ErrLocalFree is what Await returns, wrapped in a *LocalFreeError, when the lane's own local card
// freed while the call waited: the caller runs local then.
var ErrLocalFree = errors.New("this box's card freed")

// LocalFreeError is the end of a wait whose local side freed. Last is the fleet's last answer, which
// the caller quotes in its placement note.
type LocalFreeError struct {
	Last   error
	Waited time.Duration
}

func (e *LocalFreeError) Error() string {
	if e.Last == nil {
		return ErrLocalFree.Error()
	}
	return ErrLocalFree.Error() + "; the fleet's last answer: " + e.Last.Error()
}

func (e *LocalFreeError) Is(target error) bool { return target == ErrLocalFree }
func (e *LocalFreeError) Unwrap() error        { return e.Last }

// Hint is what a lane's placement error tells the loop.
type Hint struct {
	// Waitable: a place may free (a 503 or 429 at dispatch; no node eligible while at least one miss
	// was temporary: card leased, queue full, cooling). False for everything else, which the loop
	// returns at once exactly as a single try would.
	Waitable bool
	// Base and RetryAfter name the node that refused at dispatch and its Retry-After (0 = none sent).
	Base       string
	RetryAfter time.Duration
	// Short is the reason in a few words, for the one log line a change of reason earns. "" means
	// nothing new to say (a node merely cooling after a refusal already logged).
	Short string
}

// Holder is implemented by a lane's placement error.
type Holder interface{ Hold() Hint }

// HintOf reads the hint off err, or the zero Hint (not waitable) when err carries none.
func HintOf(err error) Hint {
	var h Holder
	if errors.As(err, &h) {
		return h.Hold()
	}
	return Hint{}
}

// Waitable reports whether err says a place may free.
func Waitable(err error) bool { return HintOf(err).Waitable }

// waitedError is the last error of a wait that reached its bound, its text extended with how long
// the call held its place. Unwrap keeps the lane's own error reachable (its defer class, its text).
type waitedError struct {
	err    error
	waited time.Duration
}

func (e *waitedError) Error() string { return e.err.Error() + "; " + WaitedNote(e.waited) }
func (e *waitedError) Unwrap() error { return e.err }

// Dur is a wait as the notes quote it: whole seconds ("41s"), or milliseconds under a second so a
// compressed timing still shows.
func Dur(d time.Duration) string {
	if d >= time.Second {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Millisecond).String()
}

// WaitedNote is "waited 41s in line", or "" for no wait.
func WaitedNote(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return "waited " + Dur(d) + " in line"
}

// Cooldowns is one call's memory of the nodes that refused it. The zero value is ready to use.
type Cooldowns struct {
	mu sync.Mutex
	m  map[string]cooldown
}

type cooldown struct{ cool, retry time.Time }

// NewCooldowns returns an empty set (the zero value works too).
func NewCooldowns() *Cooldowns { return &Cooldowns{} }

func (c *Cooldowns) now() time.Time { return time.Now() }

// Refused records that base refused the call, with the Retry-After it sent (0 = none). The hold is
// jittered once, here: RefusalCooldown, and the Retry-After when it is longer, both stretched by the
// same factor between 1 and 1+CooldownJitter. A second refusal extends the hold, never shortens it.
func (c *Cooldowns) Refused(base string, retryAfter time.Duration) {
	if c == nil || base == "" {
		return
	}
	f := 1 + CooldownJitter*mathrand.Float64()
	now := c.now()
	cool := now.Add(time.Duration(float64(RefusalCooldown) * f))
	retry := cool
	if retryAfter > 0 {
		if r := now.Add(time.Duration(float64(retryAfter) * f)); r.After(retry) {
			retry = r
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]cooldown{}
	}
	cur := c.m[base]
	if cool.After(cur.cool) {
		cur.cool = cool
	}
	if retry.After(cur.retry) {
		cur.retry = retry
	}
	c.m[base] = cur
}

// Skip says whether the node at base is to be left alone this tick, and why in a few words. After a
// refusal the base is skipped for the cooldown whatever its health says. Past that, a node that
// publishes its room (a queue ceiling, or a saturation verdict) is tried as soon as its fresh health
// is not saturated and skipped while it is; a node that publishes neither waits out its Retry-After.
// A node that never refused the call is skipped only while its health says it is full.
func (c *Cooldowns) Skip(base string, v delegate.NodeView) (bool, string) {
	now := time.Now()
	var cd cooldown
	var refused bool
	if c != nil {
		c.mu.Lock()
		cd, refused = c.m[base]
		c.mu.Unlock()
	}
	if refused && now.Before(cd.cool) {
		return true, CoolingReason
	}
	if why, full := Full(v); full {
		return true, why
	}
	if refused && !PublishesRoom(v) && now.Before(cd.retry) {
		return true, CoolingReason
	}
	return false, ""
}

// CoolingReason is the miss a node that just refused gets: its health is not consulted.
const CoolingReason = "cooling after a refusal"

// next is the earliest cooldown end still in the future, or the zero time.
func (c *Cooldowns) next() time.Time {
	now := time.Now()
	var best time.Time
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cd := range c.m {
		for _, t := range []time.Time{cd.cool, cd.retry} {
			if t.After(now) && (best.IsZero() || t.Before(best)) {
				best = t
			}
		}
	}
	return best
}

// PublishesRoom: the node's health says how full it is (a queue ceiling, or a saturation verdict).
func PublishesRoom(v delegate.NodeView) bool { return v.MaxQueueDepth > 0 || v.SaturationKnown }

// Full says whether the node's health reads saturated (a new dispatch would be refused by the node's
// own arithmetic), with the numbers it published.
func Full(v delegate.NodeView) (string, bool) {
	if !delegate.Saturated(v) {
		return "", false
	}
	if v.MaxQueueDepth > 0 {
		return fmt.Sprintf("queue full (%d of %d jobs)", v.QueueDepth, v.MaxQueueDepth), true
	}
	return "queue full", true
}

// Place is one call's wait for a place in line.
type Place struct {
	// Lane names the call in the log lines ("transcribe", "vision", "text", "forecast").
	Lane string
	// Bound is the longest the call holds its place; <= 0 is one try and no wait.
	Bound time.Duration
	// LocalFree, when set, says this box's own side is free again: the wait ends with a
	// *LocalFreeError and the caller runs local. nil = the call has nowhere else to go.
	LocalFree func(context.Context) bool
	// LocalWhy says in a few words why the local side is not an option ("this box's card is leased").
	LocalWhy string
	// Try is one pick plus one dispatch against a fresh read of the roster. The Cooldowns are the
	// call's: Try passes them to its node pick so a node that just refused is not re-sent the upload.
	Try func(context.Context, *Cooldowns) error
	// Register, when set, is called once before the first wait: it returns the heartbeat that keeps
	// the call's place in this box's own line and the func that gives it up when the loop ends.
	Register func() (refresh, unregister func())
}

// Await runs the loop. It returns how long the call held its place (0 when the first try placed
// it) and nil when a node accepted. Otherwise the error is the last Try error: unchanged when it was
// not waitable, extended with "; waited <d> in line" at the bound, joined with the context's error
// when the context ended, or a *LocalFreeError when the local side freed.
func (p Place) Await(ctx context.Context) (time.Duration, error) {
	cd := NewCooldowns()
	start := time.Now()
	err := p.Try(ctx, cd)
	if err == nil {
		return 0, nil
	}
	if p.Bound <= 0 {
		return 0, err
	}
	var (
		refresh, unregister func()
		registered          bool
		reason              string
		logged              bool
	)
	defer func() {
		if registered && unregister != nil {
			unregister()
		}
	}()
	for {
		h := HintOf(err)
		if !h.Waitable {
			return time.Since(start), err
		}
		if h.Base != "" {
			cd.Refused(h.Base, h.RetryAfter)
		}
		if h.Short != "" || !logged {
			now := h.Short
			if now == "" {
				now = "no node has room yet"
			}
			if p.LocalWhy != "" {
				now += "; " + p.LocalWhy
			}
			if now != reason {
				reason = now
				logged = true
				log.Printf("%s: waiting in line: %s", p.Lane, reason)
			}
		}
		if !registered && p.Register != nil {
			registered = true
			refresh, unregister = p.Register()
		}
		remaining := p.Bound - time.Since(start)
		if remaining <= 0 {
			waited := time.Since(start)
			log.Printf("%s: gave up waiting in line after %s", p.Lane, secs(waited))
			return waited, &waitedError{err: err, waited: waited}
		}
		d := min(PollInterval, remaining)
		if t := cd.next(); !t.IsZero() {
			d = min(d, max(time.Until(t), time.Millisecond))
		}
		if refresh != nil {
			refresh()
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return time.Since(start), errors.Join(err, ctx.Err())
		case <-timer.C:
		}
		if p.LocalFree != nil && p.LocalFree(ctx) {
			waited := time.Since(start)
			log.Printf("%s: this box's card freed after %s in line", p.Lane, secs(waited))
			return waited, &LocalFreeError{Last: err, Waited: waited}
		}
		next := p.Try(ctx, cd)
		if next == nil {
			return time.Since(start), nil
		}
		err = next
	}
}

// LogPlaced writes the one line that ends a wait a node answered: nothing for a call that did not wait.
func LogPlaced(lane, node string, waited time.Duration) {
	if waited <= 0 {
		return
	}
	log.Printf("%s: placed on %s after %s in line", lane, node, secs(waited))
}

func secs(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%d s", int(d.Round(time.Second)/time.Second))
	}
	return d.Round(time.Millisecond).String()
}

// Bound is how long a lane's call may hold its place: want, the lane's own figure, cut to at most
// half of what the caller's deadline has left, and 0 (no wait) when the operator switched waiting off
// (agent_placement_wait_sec < 0).
func Bound(ctx context.Context, cfg config.Config, want time.Duration) time.Duration {
	if cfg.AgentPlacementWaitSec < 0 || want <= 0 {
		return 0
	}
	if dl, ok := ctx.Deadline(); ok {
		if half := time.Until(dl) / 2; half < want {
			want = half
		}
	}
	if want < 0 {
		return 0
	}
	return want
}

// SeatWaiter is the Register of a call holding for this box's own seat: a place in the lease queue on
// the seat's cards, so a chained media acquire cannot jump the call (gpulease.RegisterSeatWaiter, the
// same place a blocked admission takes). nil when the lease gate is not armed or there is no model.
func SeatWaiter(model, why string) func() (refresh, unregister func()) {
	dir := modelaffinity.GPULeaseDir()
	model = strings.TrimSpace(model)
	if dir == "" || model == "" {
		return nil
	}
	return func() (func(), func()) {
		return gpulease.RegisterSeatWaiter(dir, "hold "+model+" for "+why, modelaffinity.SeatCards(model))
	}
}
