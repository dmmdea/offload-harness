package delegate

import (
	"sync"
	"time"
)

// Quarantine is the process-scoped memory of fleet nodes whose answers failed
// the DOCUMENT FINGERPRINT check (research.DocFingerprint) — the shape of the
// Lenovo 4B's phantom "latest Go version" digests (2026-08-31 … 09-01): a
// result that passes as a result while sharing nothing with the page it was
// handed. Two strikes within TTL block the node for TTL; the block expires on
// its own, so a flaky node is re-tried later without operator action.
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
type Quarantine struct {
	mu      sync.Mutex
	ttl     time.Duration
	strikes map[string][]time.Time
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
	return &Quarantine{ttl: ttl, strikes: map[string][]time.Time{}, until: map[string]time.Time{}, now: time.Now}
}

// Strike records one fingerprint failure for base. It returns true exactly
// when this strike FLIPS the node into quarantine (the second strike inside
// TTL), so the caller can count flips without double-counting. A nil
// Quarantine records nothing.
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
	if len(recent) >= 2 {
		q.until[base] = now.Add(q.ttl)
		q.strikes[base] = nil
		return true
	}
	return false
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
