package mediacap

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/offload-harness/internal/config"
)

func counting(calls *atomic.Int32) func(config.Config) []Route {
	return func(cfg config.Config) []Route {
		calls.Add(1)
		return []Route{{Name: "run_graph", State: Configured, Detail: cfg.RunGraphScript}}
	}
}

// A verdict is reused inside the TTL and read again after it; another config has its own entry.
func TestCacheReusesAVerdictForTheTTLAndKeysByConfig(t *testing.T) {
	var calls atomic.Int32
	now := time.Now()
	c := NewCache(time.Minute, func() time.Time { return now }, counting(&calls))
	cfg := config.Config{RunGraphScript: "a.mjs"}
	for i := 0; i < 5; i++ {
		c.Routes(cfg)
	}
	if calls.Load() != 1 {
		t.Fatalf("%d derivations inside one window, want 1", calls.Load())
	}
	now = now.Add(59 * time.Second)
	c.Routes(cfg)
	if calls.Load() != 1 {
		t.Fatal("a read at 59 s must still hit")
	}
	now = now.Add(2 * time.Second)
	c.Routes(cfg)
	if calls.Load() != 2 {
		t.Fatal("a read past 60 s must derive again")
	}
	if got := c.Routes(config.Config{RunGraphScript: "b.mjs"}); calls.Load() != 3 || got[0].Detail != "b.mjs" {
		t.Fatalf("a different config must not share the entry (calls %d, %+v)", calls.Load(), got)
	}
	now = now.Add(-time.Hour) // a clock that went backwards is never trusted as "fresh"
	c.Routes(cfg)
	if calls.Load() != 4 {
		t.Fatal("an entry stamped in the future must be re-derived")
	}
	c.Reset()
	c.Routes(cfg)
	if calls.Load() != 5 {
		t.Fatal("Reset must drop the verdicts")
	}
}

// RoutesKeyed trusts the caller's key: a caller that holds one config computes the key once and the cache
// never encodes the config again.
func TestCacheKeyedLookupUsesTheGivenKey(t *testing.T) {
	var calls atomic.Int32
	c := NewCache(time.Minute, time.Now, counting(&calls))
	cfg := config.Config{RunGraphScript: "a.mjs"}
	k, ok := KeyOf(cfg)
	if !ok {
		t.Fatal("a plain config must be keyable")
	}
	c.RoutesKeyed(k, cfg)
	c.Routes(cfg) // the same key, computed by the cache
	if calls.Load() != 1 {
		t.Fatalf("the keyed and unkeyed lookups must share an entry, derivations %d", calls.Load())
	}
}

// A config json cannot encode has no key and is never cached: every read derives.
func TestCacheNeverCachesAConfigItCannotKey(t *testing.T) {
	var calls atomic.Int32
	c := NewCache(time.Minute, time.Now, counting(&calls))
	cfg := config.Config{ImageGenCFG: math.Inf(1)}
	if _, ok := KeyOf(cfg); ok {
		t.Fatal("test premise: +Inf cannot be encoded")
	}
	c.Routes(cfg)
	c.Routes(cfg)
	if calls.Load() != 2 {
		t.Fatalf("an unkeyable config must derive every time, got %d", calls.Load())
	}
}

// One slow derivation never holds up a read of a different config (the old cache held one global lock
// across the whole filesystem walk), and concurrent readers of the SAME config share one derivation.
func TestCacheDoesNotHoldOneLockAcrossEveryConfigsDerivation(t *testing.T) {
	slowStarted, slowRelease := make(chan struct{}), make(chan struct{})
	var slowCalls, fastCalls atomic.Int32
	c := NewCache(time.Minute, time.Now, func(cfg config.Config) []Route {
		if cfg.RunGraphScript == "slow.mjs" {
			if slowCalls.Add(1) == 1 {
				close(slowStarted)
			}
			<-slowRelease
		} else {
			fastCalls.Add(1)
		}
		return []Route{{Name: "run_graph", State: Configured}}
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Routes(config.Config{RunGraphScript: "slow.mjs"}) }()
	}
	<-slowStarted
	done := make(chan struct{})
	go func() { c.Routes(config.Config{RunGraphScript: "fast.mjs"}); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a read of another config waited on a slow derivation: one lock is held across the walk")
	}
	close(slowRelease)
	wg.Wait()
	if slowCalls.Load() != 1 {
		t.Fatalf("four concurrent readers of one config derived %d times, want 1", slowCalls.Load())
	}
	if fastCalls.Load() != 1 {
		t.Fatalf("fast derived %d times", fastCalls.Load())
	}
}

// The cache stays bounded: past its limit, stale entries go first, then everything.
func TestCacheStaysBounded(t *testing.T) {
	var calls atomic.Int32
	c := NewCache(time.Minute, time.Now, counting(&calls))
	for i := 0; i < cacheMaxEntries*3; i++ {
		c.Routes(config.Config{RunGraphScript: string(rune('a'+i%26)) + string(rune('A'+i/26))})
	}
	c.mu.Lock()
	n := len(c.m)
	c.mu.Unlock()
	if n > cacheMaxEntries {
		t.Fatalf("%d entries, bound is %d", n, cacheMaxEntries)
	}
}
