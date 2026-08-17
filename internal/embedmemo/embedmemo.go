// Package embedmemo memoizes embedding vectors by exact input bytes so a text
// that has already been embedded is never embedded again.
//
// WHY THIS EXISTS (two payoffs, the second larger than the first):
//
//  1. Embedding is a pure function of (model, text). The harness re-embeds the
//     same strings constantly by construction — the kNN pre-filter embeds each
//     request input, the shadow drain re-embeds the SAME stored inputs on every
//     run, exemplar selection re-embeds a fixed pool. Recomputing a deterministic
//     function is pure waste.
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
// the retrieved neighbours would be wrong and nothing would report it. The
// measured workload is structural repeats (byte-identical strings re-embedded),
// which exact keying already captures, so normalization would buy little and
// risk much.
//
// # Invalidating on a model change
//
// The key includes the embedder id, so switching embedders can never serve the
// old model's vectors. It cannot, however, see a re-quantized or re-trained
// model published under the SAME id — nothing observable at this layer changes.
// Epoch is the manual lever for that case: bump it (config embed_memo_epoch) and
// every prior entry becomes unreachable. This mirrors the discipline the cache
// key already follows elsewhere in the harness: bind identity explicitly rather
// than hope a name is stable.
//
// # Failure policy
//
// Every failure degrades to a plain pass-through call to the real embedder. A
// memo that cannot open its file, cannot read, or reads something malformed must
// never turn into a failed embed — it was only ever an optimisation. Errors from
// the underlying embedder are never stored (a cached error would be permanent).
package embedmemo

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bktVecs = []byte("vecs") // key -> record (header + float64 payload)
	bktSeq  = []byte("seq")  // 8-byte BE insertion counter -> key (prune order)
	bktMeta = []byte("meta") // counters that must survive a restart
)

const (
	recVersion  = byte(1)
	recHeadLen  = 1 + 8 + 4 // version + seq + dim
	metaCounter = "counter"
	metaHits    = "hits"
	metaMisses  = "misses"
	metaStores  = "stores"
)

// ErrDisabled is returned by Open when the caller passed an empty path.
var ErrDisabled = errors.New("embedmemo: disabled (empty path)")

// Memo is a bbolt-backed embedding memo. The zero value is not usable; use Open.
// A nil *Memo is valid and behaves as a pass-through, so callers never need a
// nil check before Wrap.
type Memo struct {
	db         *bolt.DB
	embedderID string
	epoch      string
	maxEntries int

	// Session counters. They are reported alongside the persisted totals so a
	// single run's behaviour is legible without differencing two snapshots.
	hits   atomic.Int64
	misses atomic.Int64
	stores atomic.Int64
	errs   atomic.Int64
}

// Open opens (creating if needed) the memo db at path.
//
// embedderID must identify the serving model; epoch is an operator-controlled
// invalidation token (usually ""). maxEntries bounds the store — when it is
// exceeded, the oldest entries by insertion order are pruned back to 90% of the
// cap. maxEntries <= 0 means unbounded.
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
	return &Memo{db: db, embedderID: embedderID, epoch: epoch, maxEntries: maxEntries}, nil
}

// Close releases the db. Safe on a nil Memo.
func (m *Memo) Close() error {
	if m == nil || m.db == nil {
		return nil
	}
	return m.db.Close()
}

// key derives the storage key. Fields are separated by a NUL that cannot occur
// in an id or epoch, so no combination of (id, epoch, text) can be ambiguous —
// the same reason cache.Key joins on NUL.
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
// On a nil Memo it returns next unchanged, so a disabled memo costs exactly one
// nil check at construction and nothing at all per call.
func (m *Memo) Wrap(next func(string) ([]float64, error)) func(string) ([]float64, error) {
	if m == nil || m.db == nil || next == nil {
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
			// cause here (embedder cold/unreachable) is transient by nature.
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
	err := m.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktVecs)
		if b == nil {
			return nil
		}
		if v := b.Get([]byte(k)); v != nil {
			rec = append([]byte(nil), v...)
		}
		return nil
	})
	if err != nil || rec == nil {
		return nil, false
	}
	vec, ok := decode(rec)
	if !ok {
		// A malformed record is treated as a miss, never as an error and never as
		// an empty vector: handing []float64{} to a cosine routine would score 0
		// against everything and look like a legitimate "no similar neighbours"
		// answer. Distinguishing corrupt-from-absent is the whole point.
		m.errs.Add(1)
		return nil, false
	}
	return vec, true
}

func (m *Memo) put(k string, vec []float64) {
	err := m.db.Update(func(tx *bolt.Tx) error {
		vb, sb, mb := tx.Bucket(bktVecs), tx.Bucket(bktSeq), tx.Bucket(bktMeta)
		if vb == nil || sb == nil || mb == nil {
			return errors.New("embedmemo: missing bucket")
		}
		// An overwrite of an existing key must not consume a new sequence slot,
		// or the seq bucket accumulates entries pointing at one key and the
		// prune walk deletes a LIVE vector while believing it evicted an old one.
		if existing := vb.Get([]byte(k)); existing != nil {
			return nil
		}
		seq := readCounter(mb, metaCounter) + 1
		if err := writeCounter(mb, metaCounter, seq); err != nil {
			return err
		}
		if err := vb.Put([]byte(k), encode(seq, vec)); err != nil {
			return err
		}
		if err := sb.Put(seqKey(seq), []byte(k)); err != nil {
			return err
		}
		if err := bump(mb, metaStores, 1); err != nil {
			return err
		}
		return pruneTx(vb, sb, m.maxEntries)
	})
	if err != nil {
		m.errs.Add(1)
		return
	}
	m.stores.Add(1)
}

// pruneTx evicts the oldest entries once the store exceeds maxEntries, walking
// the seq bucket in insertion order (a cursor scan over 8-byte keys) rather than
// scanning the vector bucket — the vectors are ~6 KB each, so ordering by a scan
// of THEM would read hundreds of megabytes to delete a handful of records.
func pruneTx(vb, sb *bolt.Bucket, maxEntries int) error {
	if maxEntries <= 0 {
		return nil
	}
	n := vb.Stats().KeyN
	if n <= maxEntries {
		return nil
	}
	target := maxEntries * 9 / 10
	if target < 1 {
		target = 1
	}
	c := sb.Cursor()
	for sk, kv := c.First(); sk != nil && n > target; sk, kv = c.Next() {
		if err := vb.Delete(kv); err != nil {
			return err
		}
		if err := c.Delete(); err != nil {
			return err
		}
		n--
	}
	return nil
}

// Stats reports this session's counters plus the persisted lifetime totals.
//
// Distinct is the number of stored vectors and Total the lifetime lookups, which
// together are the ratio Phase 0.4 asks for: the memo's own bookkeeping IS the
// distinct/total instrument, so no separate counter has to be bolted onto the
// embed closure and later removed.
type Stats struct {
	Distinct       int   `json:"distinct"`
	LifetimeHits   int64 `json:"lifetime_hits"`
	LifetimeMisses int64 `json:"lifetime_misses"`
	LifetimeStores int64 `json:"lifetime_stores"`
	SessionHits    int64 `json:"session_hits"`
	SessionMisses  int64 `json:"session_misses"`
	SessionStores  int64 `json:"session_stores"`
	SessionErrors  int64 `json:"session_errors"`
	// HitRate is nil when nothing has been looked up. It is deliberately a
	// pointer and not a bare 0.0: publishing "0% hit rate" for a memo that has
	// never been consulted reports a measured failure where there is no
	// measurement at all — the same silent-zero defect the ledger loupe fixed.
	HitRate *float64 `json:"hit_rate"`
}

func (m *Memo) Stats() Stats {
	s := Stats{
		SessionHits:   m.sessionHits(),
		SessionMisses: m.sessionMisses(),
		SessionStores: m.sessionStores(),
		SessionErrors: m.sessionErrs(),
	}
	if m == nil || m.db == nil {
		return s
	}
	_ = m.db.View(func(tx *bolt.Tx) error {
		if vb := tx.Bucket(bktVecs); vb != nil {
			s.Distinct = vb.Stats().KeyN
		}
		if mb := tx.Bucket(bktMeta); mb != nil {
			s.LifetimeHits = readCounter(mb, metaHits)
			s.LifetimeMisses = readCounter(mb, metaMisses)
			s.LifetimeStores = readCounter(mb, metaStores)
		}
		return nil
	})
	// Session counters have not been flushed yet, so add them for a live view.
	lh := s.LifetimeHits + s.SessionHits
	lm := s.LifetimeMisses + s.SessionMisses
	if lh+lm > 0 {
		r := float64(lh) / float64(lh+lm)
		s.HitRate = &r
	}
	return s
}

// Flush persists the session counters into the meta bucket. Called on Close by
// owners that want lifetime totals to survive the process.
func (m *Memo) Flush() error {
	if m == nil || m.db == nil {
		return nil
	}
	h, mi := m.hits.Swap(0), m.misses.Swap(0)
	if h == 0 && mi == 0 {
		return nil
	}
	return m.db.Update(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bktMeta)
		if mb == nil {
			return errors.New("embedmemo: missing meta bucket")
		}
		if err := bump(mb, metaHits, h); err != nil {
			return err
		}
		return bump(mb, metaMisses, mi)
	})
}

func (m *Memo) sessionHits() int64 {
	if m == nil {
		return 0
	}
	return m.hits.Load()
}
func (m *Memo) sessionMisses() int64 {
	if m == nil {
		return 0
	}
	return m.misses.Load()
}
func (m *Memo) sessionStores() int64 {
	if m == nil {
		return 0
	}
	return m.stores.Load()
}
func (m *Memo) sessionErrs() int64 {
	if m == nil {
		return 0
	}
	return m.errs.Load()
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
	if dim < 0 || len(rec) != recHeadLen+8*dim || dim == 0 {
		return nil, false
	}
	out := make([]float64, dim)
	for i := range out {
		out[i] = math.Float64frombits(binary.LittleEndian.Uint64(rec[recHeadLen+8*i:]))
	}
	return out, true
}

func seqKey(seq int64) []byte {
	k := make([]byte, 8)
	binary.BigEndian.PutUint64(k, uint64(seq))
	return k
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
	return writeCounter(b, name, readCounter(b, name)+delta)
}
