package askcache

import (
	"strconv"
	"sync"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
)

func doc(name, text string) core.ContextDoc { return core.ContextDoc{Name: name, Text: text} }

// TestKeyIsStableForTheSameCall: the whole cache rests on an identical call producing an
// identical key. If this ever wobbles the cache simply never hits and the feature is inert.
func TestKeyIsStableForTheSameCall(t *testing.T) {
	d := []core.ContextDoc{doc("cfg.go", "const Cap = 32\n"), doc("run.go", "func Run() {}\n")}
	a := Key("what is the cap", "/repo", d)
	b := Key("what is the cap", "/repo", d)
	if a != b {
		t.Fatalf("the same question over the same content must key identically:\n%s\n%s", a, b)
	}
	if a == "" {
		t.Fatal("empty key")
	}
}

// TestKeyChangesWhenFileCONTENTChangesUnderTheSameName is THE load-bearing property, and
// the reason this cache is safe to serve from at all: the key is derived from the file
// BYTES, never from the path. A file edited between two otherwise-identical calls is a
// different key, so a stale answer about the pre-edit content can never be served.
//
// Break the content half of the key (key on the name alone) and this is the test that goes
// red — mutation-proven, because a cache that keys on path would happily answer a question
// about a file it no longer describes.
func TestKeyChangesWhenFileCONTENTChangesUnderTheSameName(t *testing.T) {
	before := Key("what is the cap", "/repo", []core.ContextDoc{doc("cfg.go", "const Cap = 32\n")})
	after := Key("what is the cap", "/repo", []core.ContextDoc{doc("cfg.go", "const Cap = 64\n")})
	if before == after {
		t.Fatal("same path, different bytes must be a DIFFERENT key — keying on path serves a stale answer")
	}
}

// TestKeyChangesWithEveryOtherInput: the remaining key components. Each is checked
// separately so a regression names which half broke.
func TestKeyChangesWithEveryOtherInput(t *testing.T) {
	base := []core.ContextDoc{doc("cfg.go", "const Cap = 32\n")}
	k := Key("what is the cap", "/repo", base)

	if Key("what is the timeout", "/repo", base) == k {
		t.Fatal("a different question must be a different key")
	}
	if Key("what is the cap", "/other", base) == k {
		t.Fatal("a different read_root must be a different key")
	}
	if Key("what is the cap", "/repo", []core.ContextDoc{doc("renamed.go", "const Cap = 32\n")}) == k {
		t.Fatal("the same bytes under a different attached name must be a different key")
	}
	if Key("what is the cap", "/repo", append(append([]core.ContextDoc(nil), base...), doc("b.go", "x"))) == k {
		t.Fatal("attaching another file must be a different key")
	}
	// Field-boundary safety: this is what field()'s length-prefixing actually buys, and the
	// fixture has to put a NUL byte INSIDE a field to prove it. cache.Key joins parts with a
	// NUL separator on its own, so Key("ab","c",nil) vs Key("a","bc",nil) would already
	// differ ("ab\x00c" vs "a\x00bc") even without field() — that pair proves nothing about
	// length-prefixing. Without field(), a NUL embedded IN a value collides with the
	// separator itself: Key("a\x00b","c",nil) and Key("a","b\x00c",nil) would both raw-join
	// to the identical byte string "a\x00b\x00c". field()'s length prefix is what keeps them
	// apart. Strip field() from Key and this is the assertion that goes red.
	if Key("a\x00b", "c", nil) == Key("a", "b\x00c", nil) {
		t.Fatal("key fields must not run together")
	}
}

func TestRoundTripsTheStoredResult(t *testing.T) {
	c := New()
	k := Key("q", "/repo", []core.ContextDoc{doc("a.go", "x")})
	if _, ok := c.Get(k); ok {
		t.Fatal("a fresh cache must miss")
	}
	c.Put(k, map[string]any{"answer": "42", "verified": true})
	got, ok := c.Get(k)
	if !ok {
		t.Fatal("miss on the key just written")
	}
	if got["answer"] != "42" || got["verified"] != true {
		t.Fatalf("stored result came back altered: %v", got)
	}
	// A hit must hand back a COPY: the handler stamps cache_hit onto what it returns,
	// and that stamp must not leak into the stored entry (or into another hit).
	got["cache_hit"] = true
	again, _ := c.Get(k)
	if _, polluted := again["cache_hit"]; polluted {
		t.Fatal("Get handed out the stored map itself — a caller's edit polluted the cache")
	}
}

// TestBoundedAtMaxEntries pins the bound. Oldest-out: a session touching more distinct file
// sets than this is not reusing a working set, so the cache stops growing rather than
// holding a process's whole reading history.
//
// Break the eviction condition and this is the test that goes red — mutation-proven.
func TestBoundedAtMaxEntries(t *testing.T) {
	c := New()
	keys := make([]string, 0, MaxEntries+10)
	for i := 0; i < MaxEntries+10; i++ {
		k := Key("q"+strconv.Itoa(i), "/repo", []core.ContextDoc{doc("a.go", strconv.Itoa(i))})
		keys = append(keys, k)
		c.Put(k, map[string]any{"answer": strconv.Itoa(i)})
	}
	if n := c.Len(); n != MaxEntries {
		t.Fatalf("cache grew unbounded: %d entries, want %d", n, MaxEntries)
	}
	if _, ok := c.Get(keys[0]); ok {
		t.Fatal("the OLDEST entry must be the one evicted")
	}
	if _, ok := c.Get(keys[len(keys)-1]); !ok {
		t.Fatal("the newest entry must survive")
	}
}

// TestOverwriteDoesNotDoubleCountOrder: re-putting a live key must not consume a second
// eviction slot, or a repeatedly-refreshed entry would evict the rest of the working set.
//
// Len() alone cannot catch a double-count here: len(c.m) reads the MAP, which stays 1 either
// way because both Puts share the same key — the bug this guards against is `order` growing
// to [k,k], which is invisible until the eviction loop actually fires. So after the repeated
// Put this fills the cache with MaxEntries-1 MORE distinct keys and asserts Len() lands
// exactly on MaxEntries: if the duplicate went uncaught, `order` carries one extra ghost entry
// and Len() reads MaxEntries-1 once the real eviction loop runs. Removing the exists-guard in
// Put (the `if _, exists := c.m[key]; !exists` check) makes this go red.
func TestOverwriteDoesNotDoubleCountOrder(t *testing.T) {
	c := New()
	k := Key("q", "/repo", nil)
	c.Put(k, map[string]any{"answer": "first"})
	c.Put(k, map[string]any{"answer": "second"})
	got, _ := c.Get(k)
	if got["answer"] != "second" {
		t.Fatalf("overwrite did not replace: %v", got)
	}
	for i := 0; i < MaxEntries-1; i++ {
		c.Put(Key("other"+strconv.Itoa(i), "/repo", nil), map[string]any{"answer": i})
	}
	if n := c.Len(); n != MaxEntries {
		t.Fatalf("cache holds %d entries after filling to the bound, want %d — a double-counted "+
			"overwrite left a ghost slot in the eviction order", n, MaxEntries)
	}
}

// TestConcurrentUseIsSafe is the -race target: the MCP server can serve concurrent calls,
// so every map access sits under the mutex.
func TestConcurrentUseIsSafe(t *testing.T) {
	c := New()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				k := Key("q"+strconv.Itoa(j), "/repo", []core.ContextDoc{doc("a.go", strconv.Itoa(i))})
				c.Put(k, map[string]any{"answer": j})
				c.Get(k)
				c.Len()
			}
		}(i)
	}
	wg.Wait()
	if n := c.Len(); n > MaxEntries {
		t.Fatalf("bound broken under concurrency: %d", n)
	}
}
