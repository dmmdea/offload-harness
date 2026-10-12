package delegate

import (
	"sync"
	"time"
)

// Quarantine is the process-scoped memory of fleet nodes whose answers failed
// the DOCUMENT FINGERPRINT check (research.DocFingerprint) — the shape of the
// <node-c> 4B's phantom "latest Go version" digests (2026-08-31 … 09-01): a
// result that passes as a result while sharing nothing with the page it was
// handed. Two strikes within TTL block the node for TTL, provided they outnumber
// the node's passes of the same window (Pass: a lexical tripwire misfires on an
// honest answer now and then, and a node that passed most of its work is not
// off-document); the block expires on its own, so a flaky node is re-tried later
// without operator action.
//
// Owned by the CALLER (the MCP server keeps one for its lifetime; a CLI run
// gets a fresh one) and passed through RunOptions, so it survives
// RunBatched's chunking — a per-Run map died every 8 contracts (roast, 2026-09-02).
// It is never persisted: the node bug it guards against needs a fix on the
// node, and a file that outlives the process would hide the fix's effect.
//
// Coverage: the default, spread and remote routes (fetchViews filters blocked
// nodes; runRemote strikes). The `queue` route hands placement to the pull
// holders (ADR 0030) before a runner exists, so it is NOT covered — a known
// bound, stated rather than hidden. The local seat is never quarantined.
//
// Only fingerprint failures strike. A user's over-strict `contains:` or a
// thin page's `min_items` is the CONTRACT's fault and must never quarantine
// the seat that answered honestly.
//
// A stated limit, on purpose: a node that answers about the wrong document less often than about the right one (4 misses
// beside 6 passes in the window) is not quarantined. The tripwire fails the RESULT, and a failed fingerprint keeps its
// cross-seat retry (skipsRetryAsResearchAcceptanceOnly), so such a node delivers no wrong digest: each miss costs one retry on
// another node. Idling a node on a lexical check costs the fleet a whole seat for the window, which is the larger loss, and
// the one rule that measured it (a node that passed 48 of 51 and was out for 30 minutes after two misses) is what the majority
// rule answers. An honest absence is neither a pass nor a strike (noteFingerprint), so it neither shields nor condemns.
type Quarantine struct {
	mu      sync.Mutex
	ttl     time.Duration
	strikes map[string][]time.Time
	passes  map[string][]time.Time
	until   map[string]time.Time
	now     func() time.Time
}

// DefaultQuarantineTTL: long enough to keep one bad node out of a working
// session's remaining runs, short enough that a node fixed mid-day comes back
// without a restart.
const DefaultQuarantineTTL = 30 * time.Minute

// NewQuarantine builds an empty Quarantine. ttl <= 0 uses DefaultQuarantineTTL.
func NewQuarantine(ttl time.Duration) *Quarantine {
	if ttl <= 0 {
		ttl = DefaultQuarantineTTL
	}
	return &Quarantine{ttl: ttl, strikes: map[string][]time.Time{}, passes: map[string][]time.Time{}, until: map[string]time.Time{}, now: time.Now}
}

// Strike records one fingerprint failure for base. It returns true exactly
// when this strike FLIPS the node into quarantine (at least two strikes inside
// TTL, and more strikes than passes in the same window: see Pass), so the caller
// can count flips without double-counting. A nil Quarantine records nothing.
func (q *Quarantine) Strike(base string) bool {
	if q == nil || base == "" {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	if until, ok := q.until[base]; ok && now.Before(until) {
		return false // already blocked; one flip per window
	}
	recent := q.strikes[base][:0]
	for _, t := range q.strikes[base] {
		if now.Sub(t) < q.ttl {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	q.strikes[base] = recent
	passed := 0
	for _, t := range q.passes[base] {
		if now.Sub(t) < q.ttl {
			passed++
		}
	}
	if len(recent) >= 2 && len(recent) > passed {
		q.until[base] = now.Add(q.ttl)
		q.strikes[base] = nil
		return true
	}
	return false
}

// Pass records one result of base that carried the fingerprint check and PASSED it. A node is quarantined for answering
// about the wrong document, and the check is lexical: it misfires on an honest answer now and then, so "two failures in
// the window" would quarantine a node that passed most of its work. A node that is truly off-document fails MORE often than
// it passes, which is what Strike now requires. Passes age out with the same window as strikes. A nil Quarantine records
// nothing.
func (q *Quarantine) Pass(base string) {
	if q == nil || base == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	recent := q.passes[base][:0]
	for _, t := range q.passes[base] {
		if now.Sub(t) < q.ttl {
			recent = append(recent, t)
		}
	}
	q.passes[base] = append(recent, now)
}

// Blocked reports whether base is currently quarantined. A nil Quarantine
// blocks nothing.
func (q *Quarantine) Blocked(base string) bool {
	if q == nil || base == "" {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	until, ok := q.until[base]
	if !ok {
		return false
	}
	if q.now().Before(until) {
		return true
	}
	delete(q.until, base)
	return false
}

// Until reports when base's quarantine ends (zero time when not blocked).
func (q *Quarantine) Until(base string) time.Time {
	if q == nil {
		return time.Time{}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if until, ok := q.until[base]; ok && q.now().Before(until) {
		return until
	}
	return time.Time{}
}
