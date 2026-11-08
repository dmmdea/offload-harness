package pipeline

// The node's side of the agent loop's KV-slot retention (ADR 0056 Layer 2, ADR 0100): the agent.KVSlotStore the loop
// talks to, built over this node's llama-swap endpoint, the seat the run was admitted on, and the shared save/restore
// core (internal/kvslot) the fleet lane also runs. internal/agent must not import this package, so the loop sees only
// the interface; everything node-specific - the slot directory, the seat pin, the seen-set, the cap sweep, the warm
// path - is here.

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/kvslot"
)

const (
	// kvSeenMax and kvSeenTTL bound the process-level presence tracker: a key seen once and never again must not
	// accumulate for the life of the process.
	kvSeenMax = 256
	kvSeenTTL = 24 * time.Hour
	// kvSlotAttachTimeout bounds the work of deciding whether a run gets the retention (the slot-dir read and the pin).
	kvSlotAttachTimeout = 5 * time.Second
)

// kvSeen is shared by every run in this process: the second sighting of a key may be a different run entirely.
var kvSeen = newSeenSet(kvSeenMax, kvSeenTTL)

// seenSet is a bounded, expiring set of keys.
type seenSet struct {
	mu  sync.Mutex
	m   map[string]time.Time
	max int
	ttl time.Duration
	now func() time.Time
}

func newSeenSet(max int, ttl time.Duration) *seenSet {
	return &seenSet{m: map[string]time.Time{}, max: max, ttl: ttl, now: time.Now}
}

// Seen reports whether key was already in the set (and unexpired), and marks it seen now. At the bound the oldest
// entry goes first.
func (s *seenSet) Seen(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if t, ok := s.m[key]; ok && now.Sub(t) < s.ttl {
		return true
	}
	for k, t := range s.m {
		if now.Sub(t) >= s.ttl {
			delete(s.m, k)
		}
	}
	for len(s.m) >= s.max {
		oldest, ot := "", now
		for k, t := range s.m {
			if oldest == "" || t.Before(ot) {
				oldest, ot = k, t
			}
		}
		delete(s.m, oldest)
	}
	s.m[key] = now
	return false
}

// kvSlotStore implements agent.KVSlotStore for one run on one seat.
type kvSlotStore struct {
	endpoint, seat, dir string
	// pin is the seat's config pin taken at attach time; every call re-checks the live seat against it, so a file is
	// only ever saved from and restored into the same seat shape.
	pin  string
	base string
	// capBytes bounds the slot directory (kvslot_cap_gib, 0 = 8 GiB); rewarmBudget bounds a re-warm.
	capBytes     int64
	rewarmBudget time.Duration
	seen         *seenSet
}

// newKVSlotStore decides whether this run gets the retention and builds its store. nil when the config switches it
// off, the running seat's command names no slot directory or runs more than one slot, or the seat pin cannot be read:
// the run then proceeds exactly as it did before the feature existed.
func (p *Pipeline) newKVSlotStore(ctx context.Context, seat string) agent.KVSlotStore {
	if !p.cfg.AgentKVSlotRetentionOn() || strings.TrimSpace(p.cfg.Endpoint) == "" {
		return nil
	}
	actx, cancel := context.WithTimeout(ctx, kvSlotAttachTimeout)
	defer cancel()
	dir, ok, _ := kvslot.SlotDir(actx, p.cfg.Endpoint, seat)
	if !ok {
		return nil
	}
	pin, ok := agent.ProbeSeatPin(actx, p.cfg.Endpoint, seat)
	if !ok {
		return nil
	}
	capGiB := p.cfg.KVSlotCapGiB
	if capGiB <= 0 {
		capGiB = 8
	}
	return &kvSlotStore{
		endpoint: p.cfg.Endpoint, seat: seat, dir: dir, pin: pin.SHA256,
		base:     pin.SHA256 + "\x00" + seat,
		capBytes: int64(capGiB) << 30, rewarmBudget: admissionBudget(p.cfg.AgentAdmissionWaitSec),
		seen: kvSeen,
	}
}

func (s *kvSlotStore) Base() string { return s.base }

func (s *kvSlotStore) call(ctx context.Context, key, action string) agent.KVSlotResult {
	o := kvslot.Do(ctx, kvslot.Call{Endpoint: s.endpoint, Seat: s.seat, Key: key, Dir: s.dir, Action: action, PinSHA256: s.pin})
	if o.ErrMsg != "" {
		return agent.KVSlotResult{Reason: "unavailable"}
	}
	if !o.OK() {
		return agent.KVSlotResult{Reason: o.Resp.Status, Ms: o.Resp.Ms}
	}
	return agent.KVSlotResult{OK: true, Tokens: o.Resp.Tokens, Bytes: o.Resp.Bytes, Ms: o.Resp.Ms}
}

func (s *kvSlotStore) Save(ctx context.Context, key string) agent.KVSlotResult {
	r := s.call(ctx, key, "save")
	if r.OK {
		// The same LRU pass the fleet lane runs after a save, and never the file just written.
		_ = kvslot.SweepDir(s.dir, s.capBytes, kvslot.Path(s.dir, key))
	}
	return r
}

func (s *kvSlotStore) Restore(ctx context.Context, key string) agent.KVSlotResult {
	return s.call(ctx, key, "restore")
}

func (s *kvSlotStore) FileExists(key string) bool {
	st, err := os.Stat(kvslot.Path(s.dir, key))
	return err == nil && !st.IsDir()
}

func (s *kvSlotStore) Delete(key string) { _ = os.Remove(kvslot.Path(s.dir, key)) }

// Rewarm loads the seat through the normal warm path (warmSeat: admission-bounded, lease-fenced) when it is not loaded,
// and reports whether it had to. A seat that is already ready costs one /running read.
func (s *kvSlotStore) Rewarm(ctx context.Context) bool {
	_, _, attempted, loaded := warmSeatWith(ctx, s.endpoint, s.seat, s.rewarmBudget, nil)
	return attempted && loaded
}

func (s *kvSlotStore) Seen(key string) bool { return s.seen.Seen(key) }
