package rosterprobe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/delegate"
)

// The timings, in one place (docs/systems/fleet-node.md, "The single-shot lanes read the same roster"):
//
//	DefaultTimeout        5 s   how long ONE lane call waits for ONE node's /fleet/health. A single-shot call
//	                            pays it on its own critical path, so it is shorter than the 15 s the delegator
//	                            gives a node whose work lasts minutes (a node between the two is a candidate
//	                            there and a miss here, and the miss is named). The accelerator lane keeps its 2 s.
//	MemoTTL               2 s   a GOOD answer is reused this long, by every lane in the process, so a burst of
//	                            calls reads each node once. It is the delegator's own memo window.
//	NegativeTTL           30 s  a node that failed at the TRANSPORT is not dialled again for this long. It is
//	                            the delegator's window too: long enough that a call stops paying a probe bound for
//	                            a box that is down, short enough that a node that reboots is picked up again.
//	TransientNegativeTTL  5 s   the cap on that window for the failures a node shows while it is only BUSY or
//	                            restarting: it did not answer in time, or the dial was refused or unroutable. A box
//	                            that comes back inside half a minute is back in rotation within seconds, not held
//	                            out for the rest of NegativeTTL; a DNS failure or a TLS fault keeps the full window.
//
// Constants, not variables: nothing mutates a package-level window, so two tests (or a test and a
// running lane) can never see each other's. A cache carries its own copy (NewCache), which a test
// in this package sets on its own *Cache beside a fake clock.
const (
	DefaultTimeout       = 5 * time.Second
	MemoTTL              = 2 * time.Second
	NegativeTTL          = 30 * time.Second
	TransientNegativeTTL = 5 * time.Second
)

// unbounded marks a negative-cache entry that holds for a caller with any timeout: the node
// REFUSED or could not be reached at all, so waiting longer would not have helped.
const unbounded = time.Duration(1<<63 - 1)

// Where a Reading came from, for a caller that wants to say so.
const (
	SourceProbe    = ""         // read from the node just now (or by a concurrent caller's probe this one joined)
	SourceMemo     = "memo"     // a good answer from the last MemoTTL
	SourceNegative = "negative" // a transport failure from the last NegativeTTL, replayed without a dial
)

// Reading is what a lane learned about one roster member.
type Reading struct {
	Member
	// View is the node's health, valid only when Err is nil. Views may be shared between callers
	// (the memo hands out copies of one reading), so a lane reads them and never edits one.
	View delegate.NodeView
	// Err is why there is no View: the entry was refused (see Member.Refused), the node did not
	// answer, answered with a status, or answered something that is not health.
	Err error
	// Source says whether this was dialled now, served from the memo, or replayed from the
	// negative cache.
	Source string
}

// Miss words why a lane passed this member over, the same way for every cause: a refused entry
// says it was not dialled, anything else is the base and the error the probe returned. Both print
// the base redacted and the error scrubbed (Shown, Scrub), so no credential an entry was pasted
// with reaches a lane's "probed ..." line.
func (r Reading) Miss() string {
	if r.Refused != nil {
		return r.Member.Miss()
	}
	return r.Shown() + ": " + Scrub(r.Base, r.Err)
}

// Cache holds the memo, the negative cache and the probes in flight. One process-wide instance
// (Default) backs every lane, so what the vision lane learns about a dead node saves the text lane
// the same wait.
type Cache struct {
	mu      sync.Mutex
	memo    map[string]memoEntry
	dead    map[string]deadEntry
	flights map[string]*flight

	// The windows and the clock. NewCache sets them from the constants above and time.Now; a test in
	// this package sets them on ITS OWN cache (and a fake clock), never on a package variable.
	memoTTL      time.Duration
	negativeTTL  time.Duration
	transientTTL time.Duration
	now          func() time.Time
	// onJoin, when set, is called each time a caller finds a probe already in flight and waits for it.
	// It exists so a test can release a held probe only once every joiner is provably waiting; it is
	// nil in production.
	onJoin func()
}

type memoEntry struct {
	at   time.Time
	view delegate.NodeView
}

type deadEntry struct {
	at  time.Time
	why string
	// ttl is how long THIS verdict holds: negativeTTL, or transientTTL for a failure a node shows
	// while busy or restarting (see transient).
	ttl time.Duration
	// bound is the probe timeout the failure was observed under, when it was a TIMEOUT: a caller
	// willing to wait longer than that has not been shown to fail. unbounded for a refusal.
	bound time.Duration
}

// flight is one probe in progress. A caller that finds one for the same base and bound waits for it
// instead of dialling the node again.
type flight struct {
	done chan struct{}
	view delegate.NodeView
	err  error
	// leaderGaveUp is true when the probing caller's own context ended: that is a fact about the
	// caller, not the node, so a waiter whose context is alive must not inherit it.
	leaderGaveUp bool
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{
		memo:         map[string]memoEntry{},
		dead:         map[string]deadEntry{},
		flights:      map[string]*flight{},
		memoTTL:      MemoTTL,
		negativeTTL:  NegativeTTL,
		transientTTL: TransientNegativeTTL,
		now:          time.Now,
	}
}

// Default is the cache every lane shares.
var Default = NewCache()

// Reset forgets everything remembered (the memo and the negative cache). Probes already in flight
// finish normally. Tests call it; so does anything that replaces the roster under a running process,
// since entries are keyed by base.
func (c *Cache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.memo = map[string]memoEntry{}
	c.dead = map[string]deadEntry{}
}

// Forget drops the negative verdict held against base. A lane calls it when a call to that node
// SUCCEEDED by a road other than a health probe (a dispatch that was accepted): the node is evidently
// up, and replaying "unreachable" to the next call for the rest of the window would pass over a node
// that is serving. A probe that succeeds does the same by itself. base is the entry as Members
// normalizes it (Member.Base); a trailing slash or surrounding space is tolerated.
func (c *Cache) Forget(base string) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.dead, base)
}

// Probe reads the roster's health through the shared cache. See (*Cache).Probe.
func Probe(ctx context.Context, remotes []string, token string, timeout time.Duration) []Reading {
	return Default.Probe(ctx, remotes, token, timeout)
}

// Probe reads every admitted member's /fleet/health CONCURRENTLY, each bounded by timeout (and
// by ctx, which wins when shorter), and returns one Reading per member in CONFIGURED order: the
// accelerator lane's "first listing node" and the compose lane's "config order breaks ties"
// depend on that order, and completion order is not one any caller can use. A refused member is
// returned flagged and never dialled. The wall time is the slowest member, not the sum.
//
// A good answer is memoised for MemoTTL. A TRANSPORT failure (dial refused, no route, DNS, a
// reset, the per-member timeout) is negative-cached and replayed without a dial: for NegativeTTL,
// capped at TransientNegativeTTL when it is the kind a busy or restarting node shows (timeout,
// refused or unroutable dial). A 401, 404 or 503, or a body that is not health, is a node that
// ANSWERED and is never cached. Nothing is cached once ctx is done: a cancelled call says nothing
// about a node.
//
// Callers that arrive while another is already probing the same base under the same bound wait for
// that probe's answer instead of dialling again, so a burst of lanes on a cold cache costs the node
// one request.
func (c *Cache) Probe(ctx context.Context, remotes []string, token string, timeout time.Duration) []Reading {
	members := Members(remotes)
	out := make([]Reading, len(members))
	var wg sync.WaitGroup
	for i, m := range members {
		out[i].Member = m
		if m.Refused != nil {
			out[i].Err = m.Refused
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i].View, out[i].Err, out[i].Source = c.read(ctx, out[i].Base, token, timeout)
		}(i)
	}
	wg.Wait()
	return out
}

// read answers one member from the memo, the negative cache, a probe already in flight, or a probe
// of its own, in that order. The first three are decided under ONE hold of the lock, so a probe that
// finishes between "is it cached" and "is anyone probing it" cannot be dialled a second time.
func (c *Cache) read(ctx context.Context, base, token string, timeout time.Duration) (delegate.NodeView, error, string) {
	key := base + "\x00" + timeout.String() // a 2 s caller's verdict is not a 5 s caller's
	for {
		c.mu.Lock()
		if v, ok := c.memoisedLocked(base); ok {
			c.mu.Unlock()
			return v, nil, SourceMemo
		}
		if why, ok := c.recentlyDeadLocked(base, timeout); ok {
			c.mu.Unlock()
			return delegate.NodeView{}, errors.New(why), SourceNegative
		}
		if f, ok := c.flights[key]; ok {
			c.mu.Unlock()
			if c.onJoin != nil {
				c.onJoin()
			}
			select {
			case <-f.done:
			case <-ctx.Done():
				return delegate.NodeView{}, ctx.Err(), SourceProbe
			}
			if f.leaderGaveUp && ctx.Err() == nil {
				continue // the prober's caller left; this one is still here, so ask again
			}
			return f.view, f.err, SourceProbe
		}
		f := &flight{done: make(chan struct{})}
		c.flights[key] = f
		c.mu.Unlock()

		f.view, f.err = c.dial(ctx, base, token, timeout)
		f.leaderGaveUp = ctx.Err() != nil
		c.mu.Lock()
		delete(c.flights, key)
		c.mu.Unlock()
		close(f.done)
		return f.view, f.err, SourceProbe
	}
}

// dial asks the node, bounded by timeout, and records what it learned.
func (c *Cache) dial(ctx context.Context, base, token string, timeout time.Duration) (delegate.NodeView, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	v, err := delegate.FetchNodeView(cctx, base, token)
	if err != nil {
		// Scrubbed BEFORE it is cached or returned: the dial error quotes the URL it dialled, and
		// the negative cache would otherwise replay a credential for the whole window.
		err = Scrubbed(base, err)
		c.noteDead(ctx, base, err, timeout)
		return delegate.NodeView{}, err
	}
	c.remember(base, v)
	return v, nil
}

func (c *Cache) memoisedLocked(base string) (delegate.NodeView, bool) {
	if c.memoTTL <= 0 {
		return delegate.NodeView{}, false
	}
	e, ok := c.memo[base]
	if !ok || c.now().Sub(e.at) >= c.memoTTL {
		return delegate.NodeView{}, false
	}
	return e.view, true
}

func (c *Cache) remember(base string, v delegate.NodeView) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.dead, base) // it answered: whatever was cached against it is out of date
	if c.memoTTL > 0 {
		c.memo[base] = memoEntry{at: c.now(), view: v}
	}
}

// recentlyDeadLocked answers the negative cache for a caller that would wait timeout. The reason the
// node failed is REPLAYED, with how stale the verdict is and when the node is dialled again, so
// the lane's "probed ..." line still names the node and says what happened to it.
func (c *Cache) recentlyDeadLocked(base string, timeout time.Duration) (string, bool) {
	d, ok := c.dead[base]
	if !ok {
		return "", false
	}
	elapsed := c.now().Sub(d.at)
	if elapsed >= d.ttl || timeout > d.bound {
		return "", false
	}
	return fmt.Sprintf("%s (cached %s ago, re-dial in %s)",
		d.why, elapsed.Round(time.Millisecond), (d.ttl - elapsed).Round(time.Millisecond)), true
}

// noteDead records a TRANSPORT failure against the base. Two guards, both load-bearing: only
// transport errors are cached (a node that answered with a status is not unreachable, and skipping
// it for half a minute would turn a momentary refusal into ineligibility), and nothing is cached once
// the CALLER's context is done, because a cancellation fails every probe at the same instant and
// that is a fact about the caller.
func (c *Cache) noteDead(ctx context.Context, base string, err error, timeout time.Duration) {
	if c.negativeTTL <= 0 || ctx.Err() != nil || !unreachable(err) {
		return
	}
	bound := unbounded
	if timedOut(err) {
		bound = timeout
	}
	ttl := c.negativeTTL
	if transient(err) && c.transientTTL < ttl {
		ttl = c.transientTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.memo, base)
	c.dead[base] = deadEntry{at: c.now(), why: err.Error(), ttl: ttl, bound: bound}
}

// unreachable reports whether err is a TRANSPORT failure: the health GET never received an HTTP
// answer at all. It is the delegator's own test (internal/delegate probeUnreachable, unexported),
// and a cancelled context is the caller giving up, never the node.
func unreachable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var uerr *url.Error
	return errors.As(err, &uerr)
}

// timedOut reports whether a transport failure was the probe running out of time, as opposed to
// the node refusing or being unroutable.
func timedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// transient reports whether a transport failure is the kind a node shows while it is busy or
// restarting: it did not answer in time, or the dial itself was refused or found no route. Those
// clear in seconds when the box comes back, so the verdict is held for TransientNegativeTTL at most.
// A name that does not resolve (*net.DNSError, which also surfaces as a "dial" error) or a fault
// above the dial (TLS, a reset after connecting) is not that kind and keeps the full window.
//
// It classifies by type, not by errno: Windows reports a refused connection as WSAECONNREFUSED,
// which errors.Is(err, syscall.ECONNREFUSED) does not match.
func transient(err error) bool {
	if timedOut(err) {
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return false
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}
