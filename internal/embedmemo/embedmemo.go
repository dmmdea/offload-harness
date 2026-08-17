// Package embedmemo memoizes embedding vectors by exact input bytes so a text
// that has already been embedded is never embedded again.
//
// WHY THIS EXISTS (two payoffs, the second larger than the first):
//
//  1. Embedding is a pure function of (model, text). The harness re-embeds the
//     same strings by construction. The two real sources, both wired:
//     the shadow-label drain (re-embeds the SAME stored inputs on every run, and
//     re-scores the same reference summaries across every item), and the kNN
//     pre-filter (embeds each request input; off unless knn_prefilter_enabled).
//     Recomputing a deterministic function is pure waste.
//
//     Deliberately NOT claimed: exemplar selection. internal/exemplars retrieves
//     lexically (tokenise + an inverted index) and contains no embedder at all,
//     so it is not a memo consumer. An earlier draft of this doc listed it; that
//     was wrong, and a false claim about which paths are covered is worse than a
//     shorter list.
//
//  2. The bigger win is not the compute, it is the SWAP. Every seat on this
//     fleet carries ttl=300, the embedder included, so the first embed after a
//     >5-minute idle gap pays a cold model load (~1-2 s) before it computes
//     anything. A memo hit skips the HTTP call entirely, which means it skips
//     the swap — on the request path, where that latency is most visible.
//
// # Exact bytes, deliberately — no normalization
//
// The obvious "improvement" here is to normalize the key (trim, collapse
// whitespace, casefold) for a higher hit rate. This package does NOT do that,
// and the omission is load-bearing: normalization makes two DIFFERENT texts
// share one key, so the memo would hand back a vector computed for a different
// string. For a cache of a semantic quantity that is a silent correctness bug —
// the retrieved neighbours would be wrong and nothing would report it.
//
// # Namespaces: how a wrong-model answer is made structurally impossible
//
// Every key, counter and prune-order entry is scoped to a NAMESPACE derived from
// (embedderID, epoch). Switching embedders therefore cannot serve the previous
// model's vectors — not because a check happens to run, but because the two
// models address disjoint keyspaces and disjoint counters. Epoch is the manual
// lever for the one case an id cannot see: a model re-quantized or re-trained and
// republished under an unchanged name.
//
// A namespace also records the DIMENSION of the first vector stored in it. A
// later vector of a different dimension proves the model changed behind a stable
// id, which is reported rather than silently mixed into a cosine routine.
//
// # Failure policy
//
// Every failure degrades to a plain pass-through call to the real embedder, and
// every failure is COUNTED and reportable. A memo that cannot open its file,
// cannot read, or reads something malformed must never turn into a failed embed —
// it was only ever an optimisation. But it must never look healthy either: the
// difference between "idle" and "broken" is exactly what the reporting surfaces
// exist to show. Errors from the underlying embedder are never stored (a cached
// error would be permanent).
package embedmemo

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bktVecs = []byte("vecs") // namespaced key -> record (header + float64 payload)
	bktSeq  = []byte("seq")  // ns || 8-byte BE counter -> vecs key (prune order, per namespace)
	bktMeta = []byte("meta") // per-namespace counters
)

const (
	// recVersion 2 changes the on-disk layout (namespaced seq keys + per-namespace
	// counters). A v1 record therefore fails decode, is dropped as corrupt, and is
	// recomputed once — the correct outcome for a store that never shipped.
	recVersion = byte(2)
	recHeadLen = 1 + 8 + 4 // version + seq + dim
	nsLen      = 16        // hex chars of the namespace digest
)

// Per-namespace meta keys. Suffixed with the namespace so two embedders sharing
// one file can never read each other's totals — the defect that made a "distinct
// vectors" count keep growing across epoch bumps and stop describing the live memo.
const (
	mkCounter = "counter:" // next sequence number
	mkCount   = "count:"   // live entry count (transactional; avoids an O(n) scan per put)
	mkHits    = "hits:"
	mkMisses  = "misses:"
	mkStores  = "stores:"
	mkDim     = "dim:"
)

var (
	// ErrDisabled is returned by Open when the caller passed an empty path.
	ErrDisabled = errors.New("embedmemo: disabled (empty path)")
	// ErrNoStore reports that the memo file does not exist yet — nothing has been
	// embedded on this machine. Distinct from a lock failure, because "empty" and
	// "cannot look" are different answers.
	ErrNoStore = errors.New("embedmemo: no store file yet")
	// ErrIdentityMismatch is returned by Shared when a caller asks for a path that
	// is already open under a DIFFERENT embedder id or epoch. Returning the
	// existing handle would silently serve another model's vectors.
	ErrIdentityMismatch = errors.New("embedmemo: store already open under a different embedder id/epoch")
)

// Memo is a bbolt-backed embedding memo. A nil *Memo is valid and behaves as a
// pass-through, so callers never need a nil check before Wrap.
type Memo struct {
	db         *bolt.DB
	embedderID string
	epoch      string
	ns         string // namespace digest of (embedderID, epoch)
	maxEntries int
	readOnly   bool

	// Session counters. Reported alongside the persisted totals so a single run's
	// behaviour is legible without differencing two snapshots.
	hits       atomic.Int64
	misses     atomic.Int64
	stores     atomic.Int64
	errsDecode atomic.Int64 // records that failed to decode (corrupt / wrong version)
	errsRead   atomic.Int64 // View transactions that failed outright
	errsWrite  atomic.Int64 // Update transactions that failed
	dimMismatch atomic.Int64 // vectors whose dimension disagreed with the namespace
}

func namespaceOf(embedderID, epoch string) string {
	h := sha256.New()
	h.Write([]byte(embedderID))
	h.Write([]byte{0})
	h.Write([]byte(epoch))
	return hex.EncodeToString(h.Sum(nil))[:nsLen]
}

// Open opens (creating if needed) the memo db at path.
//
// A bolt open failure is a normal, expected condition (another process holds the
// file), so callers should treat the error as "run without a memo", not fatal.
func Open(path, embedderID, epoch string, maxEntries int) (*Memo, error) {
	if path == "" {
		return nil, ErrDisabled
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bktVecs, bktSeq, bktMeta} {
			if _, e := tx.CreateBucketIfNotExists(b); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Memo{db: db, embedderID: embedderID, epoch: epoch, ns: namespaceOf(embedderID, epoch), maxEntries: maxEntries}, nil
}

// OpenReadOnly opens an EXISTING memo for inspection only.
//
// For read-only reporting paths (the loupe), which document a hard constraint
// that they must never contend with a live MCP server. bbolt still takes a
// shared lock that an exclusive read-write handle blocks, so this CAN fail while
// the server runs — the point is that it fails FAST and legibly instead of
// stalling a full second and returning a lock error the caller has to interpret.
func OpenReadOnly(path, embedderID, epoch string) (*Memo, error) {
	if path == "" {
		return nil, ErrDisabled
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoStore
		}
		return nil, err
	}
	db, err := bolt.Open(path, 0o400, &bolt.Options{ReadOnly: true, Timeout: 200 * time.Millisecond})
	if err != nil {
		return nil, err
	}
	return &Memo{db: db, embedderID: embedderID, epoch: epoch, ns: namespaceOf(embedderID, epoch), readOnly: true}, nil
}

// Close releases the db. Safe on a nil Memo.
func (m *Memo) Close() error {
	if m == nil || m.db == nil {
		return nil
	}
	return m.db.Close()
}

// EmbedderID reports the model this handle is bound to (used by Shared to refuse
// a mismatched reuse rather than silently serving another model's namespace).
func (m *Memo) EmbedderID() string {
	if m == nil {
		return ""
	}
	return m.embedderID
}

// Epoch reports the invalidation token this handle is bound to.
func (m *Memo) Epoch() string {
	if m == nil {
		return ""
	}
	return m.epoch
}

// key derives the storage key. Fields are separated by a NUL that cannot occur
// in an id or epoch, so no combination of (id, epoch, text) can be ambiguous.
func (m *Memo) key(text string) string {
	h := sha256.New()
	h.Write([]byte(m.embedderID))
	h.Write([]byte{0})
	h.Write([]byte(m.epoch))
	h.Write([]byte{0})
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}

// Wrap returns an embed function that consults the memo before calling next.
//
// On a nil or read-only Memo it returns next unchanged, so a disabled memo costs
// exactly one nil check at construction and nothing at all per call.
func (m *Memo) Wrap(next func(string) ([]float64, error)) func(string) ([]float64, error) {
	if m == nil || m.db == nil || next == nil || m.readOnly {
		return next
	}
	return func(text string) ([]float64, error) {
		k := m.key(text)
		if v, ok := m.get(k); ok {
			m.hits.Add(1)
			return v, nil
		}
		m.misses.Add(1)
		vec, err := next(text)
		if err != nil {
			// NEVER store an error. A cached failure is permanent, and the usual
			// cause here (embedder cold or unreachable) is transient by nature.
			return nil, err
		}
		if len(vec) > 0 {
			m.put(k, vec)
		}
		return vec, nil
	}
}

func (m *Memo) get(k string) ([]float64, bool) {
	var rec []byte
	var missingBucket bool
	err := m.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktVecs)
		if b == nil {
			// A read-write handle always creates this bucket in Open, so its
			// absence means the file is structurally not ours (foreign or
			// truncated). That is a fault, not an empty store — collapsing the two
			// would report a 100%-dead memo as a healthy idle one.
			missingBucket = true
			return nil
		}
		if v := b.Get([]byte(k)); v != nil {
			rec = append([]byte(nil), v...)
		}
		return nil
	})
	if err != nil {
		m.errsRead.Add(1)
		return nil, false
	}
	if missingBucket {
		m.errsRead.Add(1)
		return nil, false
	}
	if rec == nil {
		return nil, false
	}
	vec, ok := decode(rec)
	if !ok {
		// A malformed record is treated as a miss, never as an error and never as
		// an empty vector: handing []float64{} to a cosine routine would score 0
		// against everything and look like a legitimate "no similar neighbours"
		// answer.
		m.errsDecode.Add(1)
		// ...and it must be REPAIRABLE. Leaving the bad record in place made the
		// damage permanent: put() declined to overwrite an existing key, so the
		// miss -> live embed -> put cycle found the corrupt bytes still there and
		// returned without replacing them, forever. Every corrupt key then leaked a
		// hit and an error count on every single lookup.
		m.dropCorrupt(k)
		return nil, false
	}
	return vec, true
}

// dropCorrupt removes an undecodable record and its prune-order entry so the next
// embed can store a good one.
func (m *Memo) dropCorrupt(k string) {
	if m.readOnly {
		return
	}
	if err := m.db.Update(func(tx *bolt.Tx) error {
		vb, sb, mb := tx.Bucket(bktVecs), tx.Bucket(bktSeq), tx.Bucket(bktMeta)
		if vb == nil || vb.Get([]byte(k)) == nil {
			return nil // already gone; do not decrement a count we did not remove
		}
		if err := vb.Delete([]byte(k)); err != nil {
			return err
		}
		if mb != nil {
			if err := bump(mb, mkCount+m.ns, -1); err != nil {
				return err
			}
		}
		// The seq index must not outlive the vector it points at, or the prune
		// walk later deletes a key that has since been legitimately re-stored.
		if sb != nil {
			// Find first, delete after — never delete through the cursor mid-walk
			// (see pruneTx for why bbolt's Cursor.Delete + Next skips an entry).
			var found []byte
			c := sb.Cursor()
			pfx := []byte(m.ns)
			for sk, kv := c.Seek(pfx); sk != nil && hasPrefix(sk, pfx); sk, kv = c.Next() {
				if string(kv) == k {
					found = append([]byte(nil), sk...)
					break
				}
			}
			if found != nil {
				return sb.Delete(found)
			}
		}
		return nil
	}); err != nil {
		m.errsWrite.Add(1)
	}
}

func (m *Memo) put(k string, vec []float64) {
	stored := false
	err := m.db.Update(func(tx *bolt.Tx) error {
		vb, sb, mb := tx.Bucket(bktVecs), tx.Bucket(bktSeq), tx.Bucket(bktMeta)
		if vb == nil || sb == nil || mb == nil {
			return errors.New("embedmemo: missing bucket")
		}
		// Record the namespace's vector dimension on first store; a later
		// disagreement proves the model changed behind a stable id.
		if d := readCounter(mb, mkDim+m.ns); d == 0 {
			if err := writeCounter(mb, mkDim+m.ns, int64(len(vec))); err != nil {
				return err
			}
		} else if int(d) != len(vec) {
			m.dimMismatch.Add(1)
			return fmt.Errorf("embedmemo: dimension %d does not match namespace dimension %d — the embedder changed behind a stable id; bump embed_memo_epoch", len(vec), d)
		}
		// An overwrite of a LIVE key must not consume a new sequence slot, or the
		// seq bucket accumulates entries pointing at one key and the prune walk
		// deletes a live vector while believing it evicted an old one. A CORRUPT
		// record, however, must be replaced in place — reusing its slot.
		if existing := vb.Get([]byte(k)); existing != nil {
			if _, ok := decode(existing); ok {
				return nil // live and readable: nothing to do
			}
			seq := seqOfRecord(existing)
			if err := vb.Put([]byte(k), encode(seq, vec)); err != nil {
				return err
			}
			stored = true
			return nil
		}
		seq := readCounter(mb, mkCounter+m.ns) + 1
		if err := writeCounter(mb, mkCounter+m.ns, seq); err != nil {
			return err
		}
		if err := vb.Put([]byte(k), encode(seq, vec)); err != nil {
			return err
		}
		if err := sb.Put(m.seqKey(seq), []byte(k)); err != nil {
			return err
		}
		if err := bump(mb, mkCount+m.ns, 1); err != nil {
			return err
		}
		if err := bump(mb, mkStores+m.ns, 1); err != nil {
			return err
		}
		stored = true
		return m.pruneTx(vb, sb, mb)
	})
	if err != nil {
		m.errsWrite.Add(1)
		return
	}
	// Only count a store that actually wrote. Counting the no-op path made
	// session_stores diverge from the persisted total for no reason a reader
	// could deduce.
	if stored {
		m.stores.Add(1)
	}
}

// pruneTx evicts the oldest entries in THIS namespace once it exceeds
// maxEntries, walking the seq bucket in insertion order.
//
// It reads the live count from a transactional counter rather than calling
// Bucket.Stats(), which is an O(bucket) page traversal — measured at ~76 µs per
// 1,000 entries, i.e. ~3.8 ms at the 50,000 default, paid inside the write
// transaction on EVERY put and blocking all other writers. That would have made
// the optimisation a pessimisation exactly as the store became useful.
func (m *Memo) pruneTx(vb, sb, mb *bolt.Bucket) error {
	if m.maxEntries <= 0 {
		return nil
	}
	n := int(readCounter(mb, mkCount+m.ns))
	if n <= m.maxEntries {
		return nil
	}
	target := m.maxEntries * 9 / 10
	if target < 1 {
		target = 1
	}
	// TWO PASSES, deliberately. Deleting through the cursor while iterating does
	// NOT evict oldest-first: bbolt's Cursor.Delete removes the inode from the
	// materialized node without moving the cursor, and the following Next() then
	// increments the index — skipping whatever shifted into the freed slot. The
	// node is always materialized here, because put() writes this same seq bucket
	// immediately before calling prune in the same transaction. Measured with a
	// 20-entry cap over 40 inserts: seq 20, 21 and 23 were evicted while the OLDER
	// seq 19 survived. That is the opposite of the documented contract, and it
	// evicts hot recent entries — on a feature whose entire payoff is hit rate.
	c := sb.Cursor()
	pfx := []byte(m.ns)
	type victim struct{ seqKey, vecKey []byte }
	victims := make([]victim, 0, n-target)
	for sk, kv := c.Seek(pfx); sk != nil && hasPrefix(sk, pfx) && len(victims) < n-target; sk, kv = c.Next() {
		victims = append(victims, victim{
			seqKey: append([]byte(nil), sk...),
			vecKey: append([]byte(nil), kv...),
		})
	}
	for _, v := range victims {
		if err := vb.Delete(v.vecKey); err != nil {
			return err
		}
		if err := sb.Delete(v.seqKey); err != nil {
			return err
		}
	}
	if len(victims) > 0 {
		return bump(mb, mkCount+m.ns, int64(-len(victims)))
	}
	return nil
}

// Stats reports this session's counters plus the persisted totals for THIS
// namespace.
//
// Distinct is the live entry count and Total the lifetime lookups, which together
// are the ratio Phase 0.4 asks for: the memo's own bookkeeping IS the
// distinct/total instrument.
type Stats struct {
	EmbedderID string `json:"embedder_id"`
	Epoch      string `json:"epoch,omitempty"`
	Dim        int    `json:"dim,omitempty"`
	// Distinct counts only vectors in the LIVE namespace. Counting the whole file
	// would keep growing across epoch bumps (which orphan without deleting) and
	// stop describing the memo actually in use.
	Distinct int `json:"distinct"`
	// Lifetime* INCLUDE the current session — see Flush. Making the two halves
	// agree on a time base is deliberate: a consumer adding Lifetime+Session
	// would otherwise double-count some fields and not others.
	LifetimeHits   int64 `json:"lifetime_hits"`
	LifetimeMisses int64 `json:"lifetime_misses"`
	LifetimeStores int64 `json:"lifetime_stores"`
	SessionHits    int64 `json:"session_hits"`
	SessionMisses  int64 `json:"session_misses"`
	SessionStores  int64 `json:"session_stores"`
	// Fault counters, split so "the store is failing every write" is
	// distinguishable from "a few records were corrupt". Published by every
	// reporting surface: an unpublished fault counter is not a fault signal.
	ErrorsDecode  int64 `json:"errors_decode"`
	ErrorsRead    int64 `json:"errors_read"`
	ErrorsWrite   int64 `json:"errors_write"`
	DimMismatches int64 `json:"dim_mismatches"`
	// HitRate is nil when nothing has been looked up. It is deliberately a
	// pointer and not a bare 0.0: publishing "0% hit rate" for a memo that has
	// never been consulted reports a measured failure where there is no
	// measurement at all.
	HitRate *float64 `json:"hit_rate"`
}

// Stats returns the counters, or an error if the store could not be read. The
// error is returned rather than swallowed so a reporting surface cannot publish
// "available, 0 vectors, never consulted" for a store it failed to read — which
// would turn a read fault into a confident claim of emptiness.
func (m *Memo) Stats() (Stats, error) {
	s := Stats{
		EmbedderID:    m.EmbedderID(),
		Epoch:         m.Epoch(),
		SessionHits:   loadAtomic(m, func(x *Memo) int64 { return x.hits.Load() }),
		SessionMisses: loadAtomic(m, func(x *Memo) int64 { return x.misses.Load() }),
		SessionStores: loadAtomic(m, func(x *Memo) int64 { return x.stores.Load() }),
		ErrorsDecode:  loadAtomic(m, func(x *Memo) int64 { return x.errsDecode.Load() }),
		ErrorsRead:    loadAtomic(m, func(x *Memo) int64 { return x.errsRead.Load() }),
		ErrorsWrite:   loadAtomic(m, func(x *Memo) int64 { return x.errsWrite.Load() }),
		DimMismatches: loadAtomic(m, func(x *Memo) int64 { return x.dimMismatch.Load() }),
	}
	if m == nil || m.db == nil {
		return s, nil
	}
	var missing bool
	err := m.db.View(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bktMeta)
		if mb == nil {
			missing = true
			return nil
		}
		s.Distinct = int(readCounter(mb, mkCount+m.ns))
		s.Dim = int(readCounter(mb, mkDim+m.ns))
		s.LifetimeHits = readCounter(mb, mkHits+m.ns)
		s.LifetimeMisses = readCounter(mb, mkMisses+m.ns)
		s.LifetimeStores = readCounter(mb, mkStores+m.ns)
		return nil
	})
	if err != nil {
		return s, fmt.Errorf("embedmemo: read stats: %w", err)
	}
	if missing {
		return s, errors.New("embedmemo: store has no meta bucket — the file is not a memo store (foreign or truncated)")
	}
	// Session counters have not been flushed yet, so fold them in for a live view
	// and keep every Lifetime* field on the same time base.
	s.LifetimeHits += s.SessionHits
	s.LifetimeMisses += s.SessionMisses
	if s.LifetimeHits+s.LifetimeMisses > 0 {
		r := float64(s.LifetimeHits) / float64(s.LifetimeHits+s.LifetimeMisses)
		s.HitRate = &r
	}
	return s, nil
}

func loadAtomic(m *Memo, f func(*Memo) int64) int64 {
	if m == nil {
		return 0
	}
	return f(m)
}

// Flush persists the session hit/miss counters into this namespace's totals.
//
// On failure the counters are RESTORED rather than lost. Swapping them to zero
// before the write meant a failed flush (disk full at shutdown) silently deleted
// a month of hits and made the next report show a hit rate LOWER than reality —
// a fabricated measurement, not merely a missing one.
func (m *Memo) Flush() error {
	if m == nil || m.db == nil || m.readOnly {
		return nil
	}
	h, mi := m.hits.Swap(0), m.misses.Swap(0)
	if h == 0 && mi == 0 {
		return nil
	}
	err := m.db.Update(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bktMeta)
		if mb == nil {
			return errors.New("embedmemo: missing meta bucket")
		}
		if e := bump(mb, mkHits+m.ns, h); e != nil {
			return e
		}
		return bump(mb, mkMisses+m.ns, mi)
	})
	if err != nil {
		m.hits.Add(h)
		m.misses.Add(mi)
		m.errsWrite.Add(1)
		return err
	}
	return nil
}

// ---- record encoding -------------------------------------------------------
//
// float64 is stored verbatim (IEEE-754 bits, little-endian) rather than
// narrowed to float32. Halving the file would cost ~7 decimal digits of
// mantissa on every component, which perturbs cosine scores near a decision
// threshold — a memo must return the SAME vector the embedder returned, or it
// is not a memo, it is a lossy approximation nobody asked for.

func encode(seq int64, vec []float64) []byte {
	out := make([]byte, recHeadLen+8*len(vec))
	out[0] = recVersion
	binary.BigEndian.PutUint64(out[1:9], uint64(seq))
	binary.BigEndian.PutUint32(out[9:13], uint32(len(vec)))
	for i, f := range vec {
		binary.LittleEndian.PutUint64(out[recHeadLen+8*i:], math.Float64bits(f))
	}
	return out
}

func decode(rec []byte) ([]float64, bool) {
	if len(rec) < recHeadLen || rec[0] != recVersion {
		return nil, false
	}
	dim := int(binary.BigEndian.Uint32(rec[9:13]))
	if dim == 0 || len(rec) != recHeadLen+8*dim {
		return nil, false
	}
	out := make([]float64, dim)
	for i := range out {
		out[i] = math.Float64frombits(binary.LittleEndian.Uint64(rec[recHeadLen+8*i:]))
	}
	return out, true
}

// seqOfRecord recovers the sequence slot of an existing (possibly corrupt)
// record so a repair can reuse it instead of orphaning the seq entry. A record
// too short to carry a header gets 0, which sorts first and is pruned first —
// the correct fate for something unreadable.
func seqOfRecord(rec []byte) int64 {
	if len(rec) < recHeadLen {
		return 0
	}
	return int64(binary.BigEndian.Uint64(rec[1:9]))
}

// seqKey namespaces the prune order so one namespace's eviction never walks (or
// deletes) another's entries.
func (m *Memo) seqKey(seq int64) []byte {
	k := make([]byte, nsLen+8)
	copy(k, m.ns)
	binary.BigEndian.PutUint64(k[nsLen:], uint64(seq))
	return k
}

func hasPrefix(b, pfx []byte) bool {
	if len(b) < len(pfx) {
		return false
	}
	for i := range pfx {
		if b[i] != pfx[i] {
			return false
		}
	}
	return true
}

func readCounter(b *bolt.Bucket, name string) int64 {
	v := b.Get([]byte(name))
	if len(v) != 8 {
		return 0
	}
	return int64(binary.BigEndian.Uint64(v))
}

func writeCounter(b *bolt.Bucket, name string, v int64) error {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(v))
	return b.Put([]byte(name), buf)
}

func bump(b *bolt.Bucket, name string, delta int64) error {
	n := readCounter(b, name) + delta
	if n < 0 {
		n = 0 // a count can never be negative; clamping beats persisting nonsense
	}
	return writeCounter(b, name, n)
}
