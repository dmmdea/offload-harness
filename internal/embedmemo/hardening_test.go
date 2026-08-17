package embedmemo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// The persisted hit/miss totals are written ONLY by Flush, which only CloseShared
// calls. A process that exits without it leaves them at zero, and every reporting
// surface then states — in the one number the Phase 0.4 gate reads — that a memo
// which served thousands of hits was "never consulted".
func TestCountersSurviveTheProcessThatWroteThem(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memo.db")

	m, err := Shared(path, "e", "", 0)
	if err != nil {
		t.Fatalf("Shared: %v", err)
	}
	w := m.Wrap(func(string) ([]float64, error) { return []float64{1, 2}, nil })
	for i := 0; i < 4; i++ { // 1 miss + 3 hits
		if _, err := w("t"); err != nil {
			t.Fatal(err)
		}
	}
	// The shutdown path the binaries actually run.
	if err := CloseShared(); err != nil {
		t.Fatalf("CloseShared: %v", err)
	}

	m2, err := OpenReadOnly(path, "e", "")
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer m2.Close()
	st, err := m2.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.HitRate == nil {
		t.Fatal("HitRate is nil after a real shutdown — the loupe would print \"never consulted\"")
	}
	if st.LifetimeHits != 3 || st.LifetimeMisses != 1 {
		t.Fatalf("lifetime hits/misses = %d/%d, want 3/1", st.LifetimeHits, st.LifetimeMisses)
	}
}

// A corrupt record must be REPAIRED, not merely skipped. Leaving it in place made
// the damage permanent: put declined to overwrite an existing key, so every
// lookup of that text re-embedded forever and leaked an error count.
func TestCorruptRecordIsRepairedOnNextEmbed(t *testing.T) {
	m := openTemp(t, 0)
	var calls atomic.Int64
	embed := func(string) ([]float64, error) { calls.Add(1); return []float64{1, 2, 3}, nil }
	if _, err := m.Wrap(embed)("x"); err != nil {
		t.Fatal(err)
	}
	if err := m.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktVecs).Put([]byte(m.key("x")), []byte{9, 9, 9})
	}); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	for i := 0; i < 3; i++ {
		if _, err := m.Wrap(embed)("x"); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load() - before; got != 1 {
		t.Fatalf("embedder ran %d times after corruption, want exactly 1 — the record was never repaired", got)
	}
}

// A version bump is the documented reason recVersion exists. Every prior record
// must become recomputable, not permanently unreadable AND unwritable.
func TestRecordsFromAnOlderVersionAreRepairedNotDeadlocked(t *testing.T) {
	m := openTemp(t, 0)
	var calls atomic.Int64
	embed := func(string) ([]float64, error) { calls.Add(1); return []float64{4, 5}, nil }
	if _, err := m.Wrap(embed)("v"); err != nil {
		t.Fatal(err)
	}
	// Rewrite the record with a bogus version byte, keeping everything else valid.
	if err := m.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktVecs)
		rec := append([]byte(nil), b.Get([]byte(m.key("v")))...)
		rec[0] = 99
		return b.Put([]byte(m.key("v")), rec)
	}); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	for i := 0; i < 3; i++ {
		if _, err := m.Wrap(embed)("v"); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load() - before; got != 1 {
		t.Fatalf("embedder ran %d times, want 1 — an old-version record deadlocked read-vs-write", got)
	}
}

// Prune must evict OLDEST-first. bbolt's Cursor.Delete does not advance the
// cursor and the following Next() then skips whatever shifted into the freed
// slot, so a delete-while-iterating walk evicts an arbitrary subset — measured
// previously as evicting seq 20/21/23 while the older seq 19 survived.
func TestPruneEvictsStrictlyOldestFirst(t *testing.T) {
	const max = 10
	m := openTemp(t, max)
	texts := make([]string, 40)
	for i := range texts {
		texts[i] = fmt.Sprintf("unique-text-%03d", i)
		vi := float64(i)
		if _, err := m.Wrap(func(string) ([]float64, error) { return []float64{vi}, nil })(texts[i]); err != nil {
			t.Fatal(err)
		}
	}
	// Collect the surviving sequence numbers straight from the seq bucket.
	var seqs []uint64
	if err := m.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bktSeq).Cursor()
		pfx := []byte(m.ns)
		for k, _ := c.Seek(pfx); k != nil && hasPrefix(k, pfx); k, _ = c.Next() {
			seqs = append(seqs, binary.BigEndian.Uint64(k[nsLen:]))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seqs) == 0 {
		t.Fatal("prune emptied the namespace")
	}
	if len(seqs) > max {
		t.Fatalf("%d entries survive, exceeding the cap %d", len(seqs), max)
	}
	// Survivors must be a CONTIGUOUS suffix of the insertion order: [n-k+1 .. n].
	// Any gap means an older entry outlived a newer one.
	last := seqs[len(seqs)-1]
	for i, s := range seqs {
		want := last - uint64(len(seqs)-1-i)
		if s != want {
			t.Fatalf("survivors are not the newest contiguous run: got %v (entry %d is seq %d, want %d)", seqs, i, s, want)
		}
	}
	if int(last) != len(texts) {
		t.Errorf("newest surviving seq = %d, want %d (the last insert)", last, len(texts))
	}
}

// Shared keyed on path alone silently handed a second caller the first caller's
// handle — and then served another model's vectors from it. Refusing is the only
// honest option, because bbolt allows one writer.
func TestSharedRefusesAConflictingEmbedderID(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { _ = CloseShared() })
	path := filepath.Join(dir, "memo.db")

	a, err := Shared(path, "embedder-A", "", 0)
	if err != nil {
		t.Fatalf("Shared A: %v", err)
	}
	if a.EmbedderID() != "embedder-A" {
		t.Fatalf("handle bound to %q", a.EmbedderID())
	}
	if _, err := Shared(path, "embedder-B", "", 0); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("second identity got err=%v, want ErrIdentityMismatch — a mismatched handle would serve another model's vectors", err)
	}
	if _, err := Shared(path, "embedder-A", "epoch-2", 0); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("a different epoch got err=%v, want ErrIdentityMismatch", err)
	}
	// The original identity must still be served the same handle.
	again, err := Shared(path, "embedder-A", "", 0)
	if err != nil || again != a {
		t.Fatalf("the original identity lost its handle: %v", err)
	}
}

// Distinct must describe the LIVE namespace. Epoch bumps orphan without
// deleting, so a whole-file count grows monotonically across every
// re-quantization and stops describing the memo actually in use.
func TestDistinctCountsOnlyTheLiveNamespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memo.db")
	m1, err := Open(path, "e", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := m1.Wrap(func(string) ([]float64, error) { return []float64{1}, nil })(fmt.Sprintf("t%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	st1, err := m1.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st1.Distinct != 5 {
		t.Fatalf("distinct = %d, want 5", st1.Distinct)
	}
	m1.Close()

	m2, err := Open(path, "e", "epoch-2", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	st2, err := m2.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st2.Distinct != 0 {
		t.Fatalf("distinct after an epoch bump = %d, want 0 — the count is not namespace-scoped", st2.Distinct)
	}
	if _, err := m2.Wrap(func(string) ([]float64, error) { return []float64{2}, nil })("t0"); err != nil {
		t.Fatal(err)
	}
	st3, _ := m2.Stats()
	if st3.Distinct != 1 {
		t.Fatalf("distinct = %d, want 1 in the new namespace", st3.Distinct)
	}
}

// The cross-restart hit is the feature's entire value proposition for short-lived
// CLI processes, and was asserted nowhere.
func TestStoredVectorIsServedAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memo.db")
	want := []float64{0.5, -0.25, 1e-5}

	m1, err := Open(path, "e", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m1.Wrap(func(string) ([]float64, error) { return want, nil })("persist me"); err != nil {
		t.Fatal(err)
	}
	if err := m1.Flush(); err != nil {
		t.Fatal(err)
	}
	m1.Close()

	m2, err := Open(path, "e", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	called := false
	got, err := m2.Wrap(func(string) ([]float64, error) { called = true; return nil, errors.New("must not be called") })("persist me")
	if err != nil {
		t.Fatalf("reopened lookup failed: %v", err)
	}
	if called {
		t.Fatal("the embedder was called after a reopen — the vector did not survive the process")
	}
	if len(got) != len(want) {
		t.Fatalf("len %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("component %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// A dimension change behind a stable model id is provable — the namespace records
// its dimension on first store — and must be reported rather than mixed into a
// cosine routine that would then silently skip every index row.
func TestDimensionChangeBehindAStableIDIsReportedNotStored(t *testing.T) {
	m := openTemp(t, 0)
	if _, err := m.Wrap(func(string) ([]float64, error) { return []float64{1, 2, 3}, nil })("a"); err != nil {
		t.Fatal(err)
	}
	// A different dimension from the same "model".
	if _, err := m.Wrap(func(string) ([]float64, error) { return []float64{1, 2}, nil })("b"); err != nil {
		t.Fatalf("the caller must still receive its vector: %v", err)
	}
	st, err := m.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.DimMismatches == 0 {
		t.Fatal("a dimension change was not counted")
	}
	if st.Dim != 3 {
		t.Fatalf("namespace dim = %d, want the first-seen 3", st.Dim)
	}
	if st.Distinct != 1 {
		t.Fatalf("distinct = %d, want 1 — the mismatched vector must not be stored", st.Distinct)
	}
}

// Wrap runs on the request path and the MCP server handles concurrent tool calls.
func TestConcurrentWrapIsRaceFreeAndCountsCorrectly(t *testing.T) {
	m := openTemp(t, 0)
	const goroutines, perG = 8, 25
	var embedCalls atomic.Int64
	w := m.Wrap(func(s string) ([]float64, error) {
		embedCalls.Add(1)
		return []float64{float64(len(s))}, nil
	})
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				// Half overlapping keys, half distinct, so both the hit and the
				// store path are exercised concurrently.
				var key string
				if i%2 == 0 {
					key = fmt.Sprintf("shared-%d", i)
				} else {
					key = fmt.Sprintf("g%d-i%d", g, i)
				}
				if _, err := w(key); err != nil {
					t.Errorf("goroutine %d: %v", g, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	st, err := m.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.SessionHits+st.SessionMisses != int64(goroutines*perG) {
		t.Fatalf("hits+misses = %d, want %d — a lookup was lost", st.SessionHits+st.SessionMisses, goroutines*perG)
	}
	if st.ErrorsWrite != 0 || st.ErrorsRead != 0 || st.ErrorsDecode != 0 {
		t.Errorf("faults under concurrency: write=%d read=%d decode=%d", st.ErrorsWrite, st.ErrorsRead, st.ErrorsDecode)
	}
	if st.Distinct != int(st.SessionStores) {
		t.Errorf("distinct=%d but session stores=%d — the transactional count drifted", st.Distinct, st.SessionStores)
	}
}

func TestConcurrentSharedReturnsOneHandle(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { _ = CloseShared() })
	path := filepath.Join(dir, "memo.db")
	var mu sync.Mutex
	seen := map[*Memo]int{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := Shared(path, "e", "", 0)
			if err != nil {
				t.Errorf("Shared: %v", err)
				return
			}
			mu.Lock()
			seen[m]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 1 {
		t.Fatalf("%d distinct handles were returned for one identity", len(seen))
	}
}

// SessionStores must count writes that actually happened. Counting the no-op
// path made it diverge from the persisted total for no reason a reader could
// deduce from the two similarly-named fields.
func TestSessionStoresCountsOnlyRealWrites(t *testing.T) {
	m := openTemp(t, 0)
	w := m.Wrap(func(string) ([]float64, error) { return []float64{1}, nil })
	for i := 0; i < 5; i++ {
		if _, err := w("same"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		m.put(m.key("same"), []float64{1})
	}
	st, err := m.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.SessionStores != 1 {
		t.Fatalf("session stores = %d, want 1 — no-op puts were counted", st.SessionStores)
	}
	if st.LifetimeStores != 1 {
		t.Fatalf("lifetime stores = %d, want 1", st.LifetimeStores)
	}
}

// A read-only handle must never attempt writes; Wrap on one is a pass-through.
func TestReadOnlyHandleDoesNotMemoize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memo.db")
	m, err := Open(path, "e", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wrap(func(string) ([]float64, error) { return []float64{1}, nil })("seed"); err != nil {
		t.Fatal(err)
	}
	m.Close()

	ro, err := OpenReadOnly(path, "e", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	calls := 0
	fn := ro.Wrap(func(string) ([]float64, error) { calls++; return []float64{2}, nil })
	for i := 0; i < 2; i++ {
		if _, err := fn("seed"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("read-only Wrap memoized (calls=%d, want 2)", calls)
	}
	if _, err := ro.Stats(); err != nil {
		t.Fatalf("read-only Stats: %v", err)
	}
}

// THE DEAD-INSTRUMENT TEST. Fault counters held only as process-local atomics
// were rendered by surfaces that open their OWN handle (the loupe does), whose
// atomics are zero by construction — so the fault display could never be
// non-zero, and a memo failing every write read as healthy. They must survive
// the process that recorded them.
func TestFaultCountersSurviveTheProcessThatRecordedThem(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memo.db")

	m, err := Open(path, "e", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wrap(func(string) ([]float64, error) { return []float64{1, 2, 3}, nil })("x"); err != nil {
		t.Fatal(err)
	}
	// Corrupt it, then force a decode fault by looking it up.
	if err := m.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktVecs).Put([]byte(m.key("x")), []byte{9, 9, 9})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wrap(func(string) ([]float64, error) { return []float64{1, 2, 3}, nil })("x"); err != nil {
		t.Fatal(err)
	}
	live, err := m.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if live.ErrorsDecode == 0 {
		t.Fatal("the live handle did not record a decode fault")
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	m.Close()

	// A DIFFERENT handle — the loupe's situation exactly.
	ro, err := OpenReadOnly(path, "e", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	st, err := ro.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.ErrorsDecode == 0 {
		t.Fatal("a fresh handle reports zero faults — the fault display is dead code and a broken memo reads as healthy")
	}
}

// A dimension change is not a disk failure. Conflating them sent the operator to
// diagnose a dying store when the real cause was a re-quantized embedder.
func TestDimMismatchIsNotCountedAsAWriteFaultAndKeepsItsRemediation(t *testing.T) {
	m := openTemp(t, 0)
	if _, err := m.Wrap(func(string) ([]float64, error) { return []float64{1, 2, 3}, nil })("a"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := m.Wrap(func(string) ([]float64, error) { return []float64{1, 2}, nil })(fmt.Sprintf("b%d", i)); err != nil {
			t.Fatalf("the caller must still get its vector: %v", err)
		}
	}
	st, err := m.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.DimMismatches != 4 {
		t.Fatalf("dim mismatches = %d, want 4", st.DimMismatches)
	}
	if st.ErrorsWrite != 0 {
		t.Fatalf("errors_write = %d, want 0 — a dimension change is not a write fault", st.ErrorsWrite)
	}
	if st.LastFault == "" {
		t.Fatal("the remediation message was discarded; it is the only text telling an operator what to do")
	}
	if !strings.Contains(st.LastFault, "embed_memo_epoch") {
		t.Errorf("last fault does not name the remedy: %q", st.LastFault)
	}
}

// An orphaned namespace is unreachable by the live one AND not reclaimed, so a
// store can be simultaneously "empty" and at its size cap. That must be visible.
func TestOrphanedNamespacesAreReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memo.db")
	m1, err := Open(path, "e", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := m1.Wrap(func(string) ([]float64, error) { return []float64{1}, nil })(fmt.Sprintf("t%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	m1.Close()

	m2, err := Open(path, "e", "epoch-2", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	st, err := m2.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Distinct != 0 {
		t.Fatalf("distinct = %d, want 0 in the fresh namespace", st.Distinct)
	}
	if st.ForeignNamespaces != 1 {
		t.Fatalf("foreign namespaces = %d, want 1 — a store reporting 0 vectors while holding 3 is not honest", st.ForeignNamespaces)
	}
	if st.ForeignVectors != 3 {
		t.Fatalf("foreign vectors = %d, want 3", st.ForeignVectors)
	}
	if st.FileBytes <= 0 {
		t.Error("file size not reported; it is the only honest capacity signal since bbolt never shrinks")
	}
}

// A corrupt record's HEADER is not trustworthy — it is part of what decode()
// rejected. Addressing the seq bucket with the seq number stored in it can point
// at a DIFFERENT, LIVE record's entry; deleting that strands the victim, which
// stays counted in mkCount but becomes unreachable by pruneTx (which can only
// walk vectors through the seq bucket). It then owns a cap slot forever while
// every counter reports the store healthy and exactly at cap.
func TestCorruptHeaderDoesNotStrandAnotherRecord(t *testing.T) {
	const max = 8
	m := openTemp(t, max)
	embed := func(v float64) func(string) ([]float64, error) {
		return func(string) ([]float64, error) { return []float64{v}, nil }
	}
	// victim gets seq 1, saboteur gets seq 2.
	if _, err := m.Wrap(embed(1))("victim"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wrap(embed(2))("saboteur"); err != nil {
		t.Fatal(err)
	}
	// Corrupt the saboteur's record so its header points at the VICTIM's slot.
	if err := m.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bktVecs)
		rec := append([]byte(nil), b.Get([]byte(m.key("saboteur")))...)
		binary.BigEndian.PutUint64(rec[1:9], 1) // claim the victim's seq
		rec[9] = 0xFF                           // and make it undecodable
		return b.Put([]byte(m.key("saboteur")), rec)
	}); err != nil {
		t.Fatal(err)
	}
	// Touch the saboteur: get -> decode fails -> dropCorrupt runs.
	if _, err := m.Wrap(embed(3))("saboteur"); err != nil {
		t.Fatal(err)
	}
	// The victim's prune-order entry must survive.
	var victimSeqIntact bool
	if err := m.db.View(func(tx *bolt.Tx) error {
		victimSeqIntact = string(tx.Bucket(bktSeq).Get(m.seqKey(1))) == m.key("victim")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !victimSeqIntact {
		t.Fatal("a corrupt record's untrusted header deleted a LIVE record's seq entry — that record is now permanently unevictable while still counted")
	}
	// And the victim must still be evictable: push well past the cap and confirm
	// it does not survive as a stranded, uncountable resident.
	for i := 0; i < max*6; i++ {
		if _, err := m.Wrap(embed(float64(i)))(fmt.Sprintf("filler-%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := m.get(m.key("victim")); ok {
		t.Fatal("the victim survived far past the cap — it was stranded outside prune's reach")
	}
	st, err := m.Stats()
	if err != nil {
		t.Fatal(err)
	}
	// Assert the count MATCHES what is on disk, not merely that it is under the
	// cap. A stranded record does not push the count above cap — the defining
	// symptom is that everything reports healthy and exactly AT cap — so a
	// `Distinct > max` check reads as coverage while being unable to fire.
	onDisk := 0
	if err := m.db.View(func(tx *bolt.Tx) error {
		onDisk = tx.Bucket(bktVecs).Stats().KeyN
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if st.Distinct != onDisk {
		t.Fatalf("Distinct=%d but %d vectors on disk — the live count drifted from reality", st.Distinct, onDisk)
	}
	if onDisk > max {
		t.Fatalf("%d vectors on disk exceeds cap %d", onDisk, max)
	}
}

func TestOpenReadOnlyDistinguishesMissingFromDisabled(t *testing.T) {
	if _, err := OpenReadOnly("", "e", ""); !errors.Is(err, ErrDisabled) {
		t.Errorf("empty path: got %v, want ErrDisabled", err)
	}
	missing := filepath.Join(t.TempDir(), "nope.db")
	if _, err := OpenReadOnly(missing, "e", ""); !errors.Is(err, ErrNoStore) {
		t.Errorf("missing file: got %v, want ErrNoStore", err)
	}
}
