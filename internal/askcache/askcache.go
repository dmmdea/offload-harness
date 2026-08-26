// Package askcache is offload_ask's in-process, content-addressed result cache: an
// identical repeat of a call returns the answer the seat already produced instead of
// spending the seat again.
//
// # What it actually buys, stated honestly
//
// The dominant cost of an ask is SEAT TIME — 46-75 s measured on the fleet — not the file
// read. The harness already reads and inlines the files itself (delegate.InlineContextPaths),
// and that read is microseconds; caching digests of it would save nothing worth the code.
// So this caches the finished RESULT, which is the only thing that removes seat time.
//
// It pays on an EXACT repeat and on nothing else. A DIFFERENT question over the same files
// still pays full seat time, because the seat has to reason about the new question. The only
// mechanism that would fix that is keeping a model context resident between calls, which
// needs llama-swap slot pinning — trading a seat's availability for cache warmth. That was
// explicitly declined. Nothing here should be read as a general speedup.
//
// # Why serving a cached answer is safe
//
// The key is derived from the file BYTES, never from the path. A file edited between two
// otherwise-identical calls hashes differently, so it is a different key and the seat runs
// again. There is therefore no window in which a stale answer can be served for changed
// content — that single property is the whole safety argument, and TestKey...ContentChanges
// (unit) plus TestAskEditedFileMissesTheCache (wired) exist to keep it true.
//
// # Scope: the process IS the session
//
// The MCP server is spawned per client over stdio, so one client connection is one process
// is one cache — it is born with the connection and dies with it. That equivalence is why
// there is NO session_id argument: adding a required input to offload_ask would undercut
// the one-call friction removal the tool exists for, and it would be a second, weaker
// spelling of a boundary the process already draws exactly.
package askcache

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync"

	"github.com/dmmdea/offload-harness/internal/cache"
	"github.com/dmmdea/offload-harness/internal/core"
)

// MaxEntries bounds one process's cached answers, oldest evicted first.
//
// Thirty-two because a session touching more distinct file sets than that is not reusing a
// working set — it is sweeping a corpus, where a repeat is unlikely and the cache has
// stopped helping. So it stops growing rather than holding the whole session's reading.
// Insertion order, not recency: "oldest out" is the property that bounds the memory a long
// connection can pin, and an LRU would let one hot entry keep 31 cold ones resident.
const MaxEntries = 32

// keySchema is mixed into every key so a future change to what the key covers cannot
// collide with a key minted under the old rules. It is inert today: every key minted within
// one build carries the same constant, and the cache never outlives the process, so there is
// nothing here for it to invalidate. It is kept as a hook for a possible future PERSISTENT
// cache — the one scenario where an old key could still be sitting around when the schema
// changes — not because it is doing any work now.
const keySchema = "askcache/v1"

// Cache is a bounded, content-addressed map from an ask's inputs to its finished result.
// The zero value is not usable; call New. Every method is safe for concurrent use, and safe
// on a nil receiver (a nil cache is an always-miss, never a panic — the optimization is lost
// but no answer is ever wrong).
type Cache struct {
	mu    sync.Mutex
	m     map[string]map[string]any
	order []string // insertion order, oldest first — the eviction queue
}

// New returns an empty cache.
func New() *Cache {
	return &Cache{m: make(map[string]map[string]any, MaxEntries)}
}

// Key derives the cache key for one ask from everything that could change its answer: the
// question, the resolved read_root, and the NAME AND CONTENT HASH of each resolved file, in
// the order the seat will see them.
//
// docs must be the contract's own resolved context (post read_root confinement, post
// de-dupe, post name de-collision), not the caller's raw path strings — one file can be
// named two ways ("/abs/cfg.go" and "cfg.go") and both must key identically, because both
// hand the seat the identical bytes.
//
// The CONTENT half is the load-bearing one: hashing the bytes rather than the path is what
// makes an edited file a different key, and therefore what makes serving a cached answer
// safe at all. Replace it with the path and a stale answer becomes servable.
//
// Every component is length-prefixed before hashing so two fields cannot slide into one
// another and collide (question "ab" + root "c" must not equal question "a" + root "bc").
func Key(question, readRoot string, docs []core.ContextDoc) string {
	parts := make([]string, 0, 4+2*len(docs))
	parts = append(parts, field(keySchema), field(question), field(readRoot), field(strconv.Itoa(len(docs))))
	for _, d := range docs {
		sum := sha256.Sum256([]byte(d.Text))
		parts = append(parts, field(d.Name), field(hex.EncodeToString(sum[:])))
	}
	return cache.Key(parts...)
}

func field(s string) string { return strconv.Itoa(len(s)) + ":" + s }

// Get returns a COPY of the stored result, and whether there was one.
//
// A copy because the caller stamps cache_hit onto what it returns; handing back the stored
// map would let that stamp — or any later edit — leak into the cache and into every
// subsequent hit. Shallow is sufficient and deliberate: the stored values are the response's
// own scalars and slices, which nothing mutates in place. GUARD: this holds only as long as
// that stays true. Any new value ever added to the response map that is itself a map, or a
// slice that something later appends to in place, needs clone (below) deepened to cover it —
// a shallow copy would let that mutation alias back through the stored entry into every
// future hit.
func (c *Cache) Get(key string) (map[string]any, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[key]
	if !ok {
		return nil, false
	}
	return clone(v), true
}

// Put stores a copy of result under key, evicting oldest-first past MaxEntries.
//
// Callers must only ever reach here with a SUCCESSFUL, non-deferred result. A defer, a
// refusal or a runner error is a statement about this minute, not about these files, and
// caching one would turn a transient seat failure into a lane that stays dead for the rest
// of the connection.
func (c *Cache) Put(key string, result map[string]any) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]map[string]any, MaxEntries)
	}
	if _, exists := c.m[key]; !exists {
		// Only a NEW key joins the eviction queue: re-putting a live key must not
		// consume a second slot, or one repeatedly-refreshed entry would evict the
		// rest of the working set behind it.
		c.order = append(c.order, key)
	}
	c.m[key] = clone(result)
	for len(c.order) > MaxEntries {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.m, oldest)
	}
}

// Len reports how many entries are held. Used by the bound test; the bound is the reason
// this cache cannot grow without limit on a long-lived connection.
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

func clone(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
