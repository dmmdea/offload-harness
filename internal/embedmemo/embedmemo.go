// Package embedmemo memoizes embedding vectors by exact input bytes so a text
// that has already been embedded is never embedded again.
//
// WHY THIS EXISTS (two payoffs, the second larger than the first):
//
//  1. Embedding is a pure function of (model, text), so recomputing it is pure
//     waste. Where that recomputation actually happens, stated narrowly because
//     two earlier drafts of this list were wrong:
//
//     - The kNN pre-filter embeds every request input. Repeat inputs are real
//     (the ledger's duplicate-input rate is what T2-A's identity fields exist
//     to measure), so this is the genuine source — but it is OFF unless
//     knn_prefilter_enabled, which is false by default.
//     - The shadow-label drain calls Similar per item, so identical summary text
//     recurring WITHIN a run hits. Its Embed path is also gated on
//     knn_prefilter_enabled.
//
//     Deliberately NOT claimed, both corrected after review:
//     - Exemplar selection does not embed at all. internal/exemplars retrieves
//     lexically (tokenise + an inverted index) and contains no embedder.
//     - The drain does NOT "re-embed the same stored inputs on every run".
//     shadow.Drain is destructive — it renames the queue to .draining, reads
//     it, and removes the claim — so each run consumes a FRESH item set. Only a
//     crash-recovered claim replays. Nor does it re-score a shared reference
//     set: Similar compares each item's own entry and escalation summaries.
//
//     Being accurate about this matters more than the feature looking good: a
//     false claim about which paths are covered is worse than a shorter list.
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
	"sync"
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
	// Fault counters are PERSISTED, not just held in memory.
	//
	// Holding them only as process-local atomics made every reporting surface
	// that opens its own handle — which is what the loupe does — read zeros by
	// construction. The counters were incremented in one process and rendered in
	// another, so the whole fault display was unfalsifiable: a memo failing 100%
	// of its writes printed no fault line at all. That is the same
	// "instrument reports the opposite of the truth" defect this package was
	// already fixed for once.
	mkErrDecode = "err_decode:"
	mkErrRead   = "err_read:"
	mkErrWrite  = "err_write:"
	mkDimMiss   = "err_dim:"
	// mkUnderflow counts IMPOSSIBLE bookkeeping states: a counter asked to go below
	// zero, a live count read back negative, or a count claiming more entries than
	// the prune order can reach. Deliberately NOT counted: a count merely above the
	// current cap, which is what lowering embed_memo_max_entries legitimately
	// produces — counting that fabricated nine permanent faults from one config
	// edit. Silently normalizing a genuinely impossible state, though, erases the
	// evidence of the bug that produced it.
	mkUnderflow = "err_underflow:"
	// mkLastFault stores the most recent human-readable fault, so the one message
	// that tells an operator what to DO (e.g. "bump embed_memo_epoch") survives
	// the process that produced it.
	mkLastFault = "last_fault:"
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

	// Session counters.
	//
	// INVARIANT: these hold only what is NOT YET on disk. Flush drains them into
	// the persisted mk* keys, and Stats reports (persisted + session). A fault
	// recorded inside a successful transaction is persisted there and must NOT
	// also bump an atomic, or it is counted twice — which is how the first version
	// of the dimension counter reported 8 mismatches for 4 events.
	//
	// `stores` is the deliberate exception: mkStores is bumped transactionally in
	// put, so it is already authoritative. Flush only RESETS the session view of
	// it, and Stats correspondingly does not fold SessionStores into
	// LifetimeStores the way it does hits and misses.
	//
	// The atomics exist because the faults that matter most cannot be written at
	// the moment they happen: a failed read or a failed write transaction is
	// precisely when the store will not accept a counter update.
	hits       atomic.Int64
	misses     atomic.Int64
	stores     atomic.Int64
	errsDecode atomic.Int64 // records that failed to decode (corrupt / wrong version)
	errsRead   atomic.Int64 // View transactions that failed outright
	errsWrite  atomic.Int64 // Update transactions that failed

	// statMu serialises Stats against Flush. Without it, Stats loads the session
	// atomics, Flush swaps them to zero and commits, and Stats then adds the
	// already-persisted values again — reporting double the real hit count. The
	// reverse interleaving loses a whole session instead. Either way the Phase
	// 0.4 gate reads a fabricated number, which is worse than a stale one.
	statMu sync.Mutex
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
		if vb == nil {
			return nil
		}
		rec := vb.Get([]byte(k))
		if rec == nil {
			return nil // already gone; do not decrement a count we did not remove
		}
		// The record's own header carries its sequence number, so the seq entry is
		// addressable DIRECTLY. The previous version scanned the namespace to find
		// it — an O(bucket) cursor walk inside an exclusive write transaction, on
		// the request path, executed once per corrupt record. Its documented
		// trigger is a version bump, where EVERY record is corrupt: that made the
		// first run after an upgrade a full-bucket scan plus an fsync on every
		// single lookup, for the whole migration window, with no progress signal.
		seq := seqOfRecord(rec)
		if err := vb.Delete([]byte(k)); err != nil {
			return err
		}
		if mb != nil {
			if err := m.bumpNS(mb, mkCount, -1); err != nil {
				return err
			}
		}
		// A seq entry that outlives its vector would let the prune walk delete a
		// key since legitimately re-stored.
		//
		// VERIFY BEFORE DELETING. `seq` came out of a record decode() has already
		// REJECTED, so the header is exactly the part of it that is not
		// trustworthy — addressing the seq bucket with it can point at a
		// DIFFERENT, LIVE record's entry. Deleting that strands the victim: it
		// stays counted in mkCount but becomes unreachable by pruneTx (which can
		// only walk vectors through the seq bucket), so it owns a cap slot
		// permanently while every counter reports the store healthy and at cap.
		// Confirmed reproducible; the documented trigger — a recVersion bump,
		// where "EVERY record is corrupt" — would strand the whole namespace.
		//
		// If the entry does not point back at this key, leave it: pruneTx counts
		// only vectors it actually removed, so a dangling entry costs one victim
		// slot and nothing else.
		if sb != nil && seq != 0 {
			if string(sb.Get(m.seqKey(seq))) == k {
				return sb.Delete(m.seqKey(seq))
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
			// A dimension change is NOT a write fault, and conflating them sent the
			// operator to diagnose a failing disk when the real cause is a
			// re-quantized embedder. Record it as its own class, persist the
			// remediation message (it is the only text in this package that tells
			// an operator what to DO), and return nil so the caller is not also
			// charged an errsWrite.
			//
			// Persisted ONLY — deliberately no session-atomic bump. The invariant
			// is that the atomics hold faults NOT YET on disk (Flush drains them),
			// and Stats adds the two. Doing both here double-counted.
			_ = m.bumpNS(mb, mkDimMiss, 1)
			_ = mb.Put([]byte(mkLastFault+m.ns), []byte(fmt.Sprintf(
				"vector dimension %d does not match this namespace's dimension %d — the embedder changed behind a stable id; bump embed_memo_epoch to start a clean namespace",
				len(vec), d)))
			return nil
		}
		// An overwrite of a LIVE key must not consume a new sequence slot, or the
		// seq bucket accumulates entries pointing at one key and the prune walk
		// deletes a live vector while believing it evicted an old one. A CORRUPT
		// record, however, must be replaced in place — reusing its slot.
		if existing := vb.Get([]byte(k)); existing != nil {
			if _, ok := decode(existing); ok {
				return nil // live and readable: nothing to do
			}
			// Same untrusted-header problem as dropCorrupt: reuse the slot ONLY if
			// the seq bucket agrees it belongs to this key. Otherwise allocate a
			// fresh slot — a second entry may then point at this key, which
			// pruneTx tolerates (it counts vectors actually removed), whereas
			// re-encoding a wrong slot number propagates the corruption into a
			// record that is now decodable and therefore trusted.
			seq := seqOfRecord(existing)
			if seq == 0 || string(sb.Get(m.seqKey(seq))) != k {
				seq = readCounter(mb, mkCounter+m.ns) + 1
				if err := writeCounter(mb, mkCounter+m.ns, seq); err != nil {
					return err
				}
				if err := sb.Put(m.seqKey(seq), []byte(k)); err != nil {
					return err
				}
			}
			if err := vb.Put([]byte(k), encode(seq, vec)); err != nil {
				return err
			}
			// A repair IS a write. Omitting this bump made session_stores and
			// lifetime_stores disagree with no explanation available to a reader —
			// and the loupe's "counters unflushed" detector keys on the persisted
			// total, so a store whose only writes were repairs took the wrong branch.
			if err := m.bumpNS(mb, mkStores, 1); err != nil {
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
		if err := m.bumpNS(mb, mkCount, 1); err != nil {
			return err
		}
		if err := m.bumpNS(mb, mkStores, 1); err != nil {
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
	// Read as int64 and validate BEFORE narrowing. A count stored as all-ones
	// reads back negative, `n <= maxEntries` returns immediately, and prune never
	// runs again — the store then grows past its cap forever with no signal at
	// all. A negative count is unambiguously impossible (bumpNS clamps every write
	// at 0), so flagging it carries no phantom-fault risk.
	raw := readCounter(mb, mkCount+m.ns)
	if raw < 0 {
		_ = m.bumpNS(mb, mkUnderflow, 1)
		_ = mb.Put([]byte(mkLastFault+m.ns), []byte(fmt.Sprintf(
			"live-entry counter read %d — negative counts are impossible; the count is corrupt, delete the store to rebuild it", raw)))
		return nil
	}
	if raw <= int64(m.maxEntries) {
		return nil
	}
	n := int(raw)
	// n comes off disk with no bound. A corrupt counter would otherwise reach the
	// make() below with a multi-exabyte capacity and panic inside db.Update,
	// which propagates out through Wrap and kills the process — breaking this
	// package's own contract that every failure degrades to a live call.
	//
	// The clamp itself is silent for the ordinary case, but a count that no
	// configuration could produce is still counted.
	//
	// The history matters. First version counted every n > 2*maxEntries as a
	// fault — but that is exactly what LOWERING embed_memo_max_entries produces,
	// so one legitimate config edit fabricated nine permanent "underflow" faults.
	// Making it fully silent then removed the ONLY signal for a counter corrupted
	// HIGH: with mkCount set to 1<<40 the namespace is wiped on the next put
	// (insert, then prune evicts everything), the memo goes permanently 100% miss,
	// and every fault counter reads zero.
	//
	// A magnitude threshold was tried and is not enough on its own: a count
	// corrupted to 1<<20 wipes the namespace just as thoroughly as 1<<40 while
	// sitting far below any plausible floor. The reliable discriminator is
	// STRUCTURAL and needs no threshold — see the victim-walk check below, which
	// compares what the counter claims against what the prune order can actually
	// reach. The clamp here only bounds the allocation.
	if lim := m.maxEntries * 2; n > lim {
		n = lim
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
	// Count the vectors ACTUALLY removed, not the seq entries walked. bbolt's
	// Delete on a missing key is a silent no-op, so a dangling seq entry (left by
	// a header-less corrupt record, or by a layout change) would otherwise
	// decrement the live count for a vector that was never there — drifting the
	// counter low until prune stops firing and the store grows past its cap
	// forever, silently.
	removed := 0
	for _, v := range victims {
		if vb.Get(v.vecKey) != nil {
			if err := vb.Delete(v.vecKey); err != nil {
				return err
			}
			removed++
		}
		if err := sb.Delete(v.seqKey); err != nil {
			return err
		}
	}
	// STRUCTURAL corruption check, config-independent and threshold-free. The walk
	// asked for n-target victims; if the prune order could not supply them, the
	// counter claims more live vectors than the seq bucket can reach. That is
	// impossible in a consistent store and is exactly the state a corrupt count
	// produces — at ANY magnitude, which is why this replaces the magnitude floor
	// that missed everything below it.
	if want := n - target; len(victims) < want {
		_ = m.bumpNS(mb, mkUnderflow, 1)
		_ = mb.Put([]byte(mkLastFault+m.ns), []byte(fmt.Sprintf(
			"live-entry counter claims %d entries but the prune order holds only %d reachable — the count is corrupt; delete the store to rebuild it",
			n, len(victims)+target)))
	}
	if removed > 0 {
		return m.bumpNS(mb, mkCount, int64(-removed))
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
	// CountUnderflows records an impossible bookkeeping state: a counter asked to
	// go below zero, or a live-entry count so large no configuration could produce
	// it. Clamping without counting would erase the evidence of the bug that
	// produced it. Deliberately NOT counted: a count merely above the current cap,
	// which is what lowering embed_memo_max_entries legitimately produces.
	CountUnderflows int64 `json:"count_underflows"`
	// LastFault is the most recent human-readable fault with its remediation.
	LastFault string `json:"last_fault,omitempty"`
	// ForeignNamespaces/ForeignVectors describe entries this handle can never
	// reach (a prior embedder id or epoch). They are not deleted, so without this
	// a store at its size cap reports "0 vectors stored".
	ForeignNamespaces int `json:"foreign_namespaces,omitempty"`
	ForeignVectors    int `json:"foreign_vectors,omitempty"`
	// FileBytes is the store's on-disk size. bbolt never shrinks after a prune,
	// so this is a high-water mark and the only honest capacity signal.
	FileBytes int64 `json:"file_bytes,omitempty"`
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
	if m == nil || m.db == nil {
		return Stats{}, nil
	}
	// Held across BOTH the atomic loads and the read transaction, so a concurrent
	// Flush cannot swap the session counters to zero and commit them between the
	// two halves — which would report double the real hit count (or lose a whole
	// session, depending on the interleaving).
	m.statMu.Lock()
	defer m.statMu.Unlock()

	s := Stats{
		EmbedderID:    m.embedderID,
		Epoch:         m.epoch,
		SessionHits:   m.hits.Load(),
		SessionMisses: m.misses.Load(),
		SessionStores: m.stores.Load(),
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
		// Faults come from DISK, so a surface that opened its own handle (the
		// loupe does) reports what actually happened rather than the zeros its own
		// untouched atomics would give it.
		s.ErrorsDecode = readCounter(mb, mkErrDecode+m.ns)
		s.ErrorsRead = readCounter(mb, mkErrRead+m.ns)
		s.ErrorsWrite = readCounter(mb, mkErrWrite+m.ns)
		s.DimMismatches = readCounter(mb, mkDimMiss+m.ns)
		s.CountUnderflows = readCounter(mb, mkUnderflow+m.ns)
		s.LastFault = string(mb.Get([]byte(mkLastFault + m.ns)))
		// Namespaces other than the live one are orphaned-but-not-deleted (an
		// epoch bump or a model switch). Without this, a 640 MB store full of
		// unreachable vectors reports "0 vectors stored" and the operator is told
		// the file is empty while it sits at its size cap.
		if c := mb.Cursor(); c != nil {
			pfx := []byte(mkCount)
			live := mkCount + m.ns
			for k, v := c.Seek(pfx); k != nil && hasPrefix(k, pfx); k, v = c.Next() {
				if string(k) != live && len(v) == 8 && int64(binary.BigEndian.Uint64(v)) > 0 {
					s.ForeignNamespaces++
					s.ForeignVectors += int(binary.BigEndian.Uint64(v))
				}
			}
		}
		s.FileBytes = tx.Size()
		return nil
	})
	// Fold the unflushed session counters in, keeping every Lifetime* field on one
	// time base. Session faults are added the same way: they are persisted
	// opportunistically, but the ones since the last successful write are only in
	// memory.
	s.ErrorsDecode += m.errsDecode.Load()
	s.ErrorsRead += m.errsRead.Load()
	s.ErrorsWrite += m.errsWrite.Load()
	if err != nil {
		return s, fmt.Errorf("embedmemo: read stats: %w", err)
	}
	if missing {
		return s, errors.New("embedmemo: store has no meta bucket — the file is not a memo store (foreign or truncated)")
	}
	s.LifetimeHits += s.SessionHits
	s.LifetimeMisses += s.SessionMisses
	if s.LifetimeHits+s.LifetimeMisses > 0 {
		r := float64(s.LifetimeHits) / float64(s.LifetimeHits+s.LifetimeMisses)
		s.HitRate = &r
	}
	return s, nil
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
	m.statMu.Lock()
	defer m.statMu.Unlock()
	h, mi := m.hits.Swap(0), m.misses.Swap(0)
	ed, er, ew := m.errsDecode.Swap(0), m.errsRead.Swap(0), m.errsWrite.Swap(0)
	// stores is bumped transactionally inside put, so mkStores is already
	// authoritative — this only RESETS the session view. Without it, a periodic
	// flush left session_hits meaning "since the last tick" while session_stores
	// still meant "since process start", and both were published side by side.
	//
	// Swap, not Store: it is restored with the others on failure. Zeroing it
	// unconditionally reinstated the very defect this line fixes, one path over —
	// session_hits would mean "since the last SUCCESSFUL flush" while
	// session_stores meant "since the last ATTEMPTED one", and the MCP server's
	// periodic flush logs-and-continues on failure, so every later status read
	// stayed skewed until the next success.
	st := m.stores.Swap(0)
	if h == 0 && mi == 0 && ed == 0 && er == 0 && ew == 0 {
		return nil
	}
	err := m.db.Update(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bktMeta)
		if mb == nil {
			return errors.New("embedmemo: missing meta bucket")
		}
		for _, p := range []struct {
			key string
			n   int64
		}{
			{mkHits, h}, {mkMisses, mi},
			{mkErrDecode, ed}, {mkErrRead, er}, {mkErrWrite, ew},
		} {
			if p.n == 0 {
				continue
			}
			if e := m.bumpNS(mb, p.key, p.n); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		// Restore everything so a retry (or a later periodic flush) can still
		// persist it. Zeroing before the write meant a failed flush silently
		// DELETED the counters, making the next report show a rate lower than
		// reality — a fabricated measurement, not merely a missing one.
		m.hits.Add(h)
		m.misses.Add(mi)
		m.stores.Add(st)
		m.errsDecode.Add(ed)
		m.errsRead.Add(er)
		m.errsWrite.Add(ew)
		m.errsWrite.Add(1)
		return err
	}
	return nil
}

// bumpNS adds delta to a per-namespace counter, recording an underflow rather
// than silently normalizing one.
func (m *Memo) bumpNS(b *bolt.Bucket, key string, delta int64) error {
	name := key + m.ns
	n := readCounter(b, name) + delta
	if n < 0 {
		n = 0
		// Only count the underflow itself; recursing through bumpNS for the
		// underflow counter could loop.
		u := readCounter(b, mkUnderflow+m.ns) + 1
		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, uint64(u))
		if e := b.Put([]byte(mkUnderflow+m.ns), buf); e != nil {
			return e
		}
	}
	return writeCounter(b, name, n)
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

// seqOfRecord recovers the sequence slot recorded in a record's header.
//
// Two callers, both of which need the slot NUMBER and neither of which changes
// prune order by writing it:
//   - put's repair branch re-encodes the record with the same slot, so the
//     existing seq-bucket entry (which already points at this key) stays valid
//     and is neither orphaned nor duplicated;
//   - dropCorrupt uses it to address that seq entry DIRECTLY instead of scanning
//     the namespace for it.
//
// Prune order comes exclusively from the seq BUCKET's key ordering; the header
// copy is never sorted on. A record too short to carry a header yields 0, whose
// seq key will simply not exist — deleting a missing key is a no-op in bbolt, and
// pruneTx tolerates a dangling seq entry by counting only vectors it actually
// removed.
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
