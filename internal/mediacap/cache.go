package mediacap

// A short-lived cache over Routes. The derivation reads the disk (script files, model files, custom-node
// directories), and two callers ask for it far more often than the disk changes: a fleet node's health is
// polled every few seconds by every delegator and its admission consults the verdict per job, and a thin
// client's media call asks whether this machine has a lane before it decides where to run. Each caller
// owns its Cache (a node stubs the derivation in tests; a client reads the real one), so the two never
// share a verdict they must not.

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

// Key identifies a config for the cache: the hash of its JSON form.
type Key [sha256.Size]byte

// KeyOf computes cfg's Key. ok is false for a config that cannot be encoded: such a config has no key and
// is never cached (what cannot be told apart is never shared). A caller that holds one config for its
// whole life computes the key once and uses RoutesKeyed, so a hot path never encodes the config again.
func KeyOf(cfg config.Config) (key Key, ok bool) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return Key{}, false
	}
	return sha256.Sum256(raw), true
}

// cacheMaxEntries bounds a Cache: a process serves one config, so more than a few entries only ever come
// from tests that build many.
const cacheMaxEntries = 32

type cacheEntry struct {
	// mu is held across the derivation on purpose: concurrent callers of the SAME config wait for one read
	// instead of each walking the disk, while callers of a different config never wait on it.
	mu     sync.Mutex
	at     time.Time
	routes []Route
	valid  bool
}

// Cache memoizes a route derivation per config for at most its TTL.
type Cache struct {
	ttl    time.Duration
	now    func() time.Time
	derive func(config.Config) []Route
	mu     sync.Mutex // guards entries only, never held across a derivation
	m      map[Key]*cacheEntry
}

// NewCache builds a Cache. now and derive are seams: production passes time.Now and Routes.
func NewCache(ttl time.Duration, now func() time.Time, derive func(config.Config) []Route) *Cache {
	return &Cache{ttl: ttl, now: now, derive: derive, m: map[Key]*cacheEntry{}}
}

// Routes returns cfg's routes, at most the TTL old.
func (c *Cache) Routes(cfg config.Config) []Route {
	k, ok := KeyOf(cfg)
	if !ok {
		return c.derive(cfg)
	}
	return c.RoutesKeyed(k, cfg)
}

// RoutesKeyed is Routes for a caller that already holds cfg's key (KeyOf(cfg)); the key is trusted.
func (c *Cache) RoutesKeyed(k Key, cfg config.Config) []Route {
	c.mu.Lock()
	e := c.m[k]
	if e == nil {
		if len(c.m) >= cacheMaxEntries {
			c.evictLocked()
		}
		e = &cacheEntry{}
		c.m[k] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	now := c.now()
	if e.valid && now.Sub(e.at) < c.ttl && now.Sub(e.at) >= 0 {
		return e.routes
	}
	e.routes, e.at, e.valid = c.derive(cfg), now, true
	return e.routes
}

// evictLocked drops stale entries and, when that frees nothing, every entry (callers mid-derivation keep
// the entry they hold). An entry that is being derived right now is neither stale nor waited on.
func (c *Cache) evictLocked() {
	now := c.now()
	for k, e := range c.m {
		if !e.mu.TryLock() {
			continue
		}
		stale := !e.valid || now.Sub(e.at) >= c.ttl || now.Sub(e.at) < 0
		e.mu.Unlock()
		if stale {
			delete(c.m, k)
		}
	}
	if len(c.m) >= cacheMaxEntries {
		c.m = map[Key]*cacheEntry{}
	}
}

// Reset drops every cached verdict, so the next read goes to the disk again.
func (c *Cache) Reset() {
	c.mu.Lock()
	c.m = map[Key]*cacheEntry{}
	c.mu.Unlock()
}
