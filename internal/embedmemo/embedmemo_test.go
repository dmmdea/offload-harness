package embedmemo

import (
	"errors"
	"math"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func openTemp(t *testing.T, max int) *Memo {
	t.Helper()
	m, err := Open(filepath.Join(t.TempDir(), "memo.db"), "embedder-A", "", max)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

// A memo must return the SAME vector the embedder produced, bit for bit. This is
// the whole contract: an approximate answer is not a cache, it is a silent
// quality regression on every consumer that scores cosine near a threshold.
func TestHitReturnsBitIdenticalVector(t *testing.T) {
	m := openTemp(t, 0)
	// Values chosen to break a float32 round-trip: tiny, huge, and a mantissa
	// that does not survive narrowing.
	want := []float64{0.1234567890123456, -1e-300, 3.141592653589793, 1e300, 0}
	calls := 0
	wrapped := m.Wrap(func(string) ([]float64, error) { calls++; return want, nil })

	if _, err := wrapped("hello"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	got, err := wrapped("hello")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if calls != 1 {
		t.Fatalf("embedder called %d times; the second call must be served from the memo", calls)
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if math.Float64bits(got[i]) != math.Float64bits(want[i]) {
			t.Errorf("component %d = %v (bits %x), want %v (bits %x)", i, got[i], math.Float64bits(got[i]), want[i], math.Float64bits(want[i]))
		}
	}
}

// The embedder id is in the key, so switching models can never serve the previous
// model's vectors. This is the difference between a cache and a correctness bug.
func TestDifferentEmbedderIDNeverSharesEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "memo.db")

	mA, err := Open(path, "embedder-A", "", 0)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	if _, err := mA.Wrap(func(string) ([]float64, error) { return []float64{1, 0}, nil })("same text"); err != nil {
		t.Fatal(err)
	}
	mA.Close()

	mB, err := Open(path, "embedder-B", "", 0)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	defer mB.Close()
	calledB := false
	got, err := mB.Wrap(func(string) ([]float64, error) { calledB = true; return []float64{0, 1}, nil })("same text")
	if err != nil {
		t.Fatal(err)
	}
	if !calledB {
		t.Fatal("embedder B was NOT called — model A's vector was served for model B")
	}
	if got[0] != 0 || got[1] != 1 {
		t.Fatalf("got %v, want embedder B's own vector", got)
	}
}

// Epoch is the lever for the case the id cannot see: a model republished under an
// unchanged name. Bumping it must orphan every prior entry.
func TestEpochBumpOrphansPriorEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memo.db")
	m1, _ := Open(path, "e", "", 0)
	if _, err := m1.Wrap(func(string) ([]float64, error) { return []float64{1}, nil })("t"); err != nil {
		t.Fatal(err)
	}
	m1.Close()

	m2, _ := Open(path, "e", "2026-08-17-requant", 0)
	defer m2.Close()
	called := false
	if _, err := m2.Wrap(func(string) ([]float64, error) { called = true; return []float64{2}, nil })("t"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("epoch bump did not orphan the prior entry")
	}
}

// A cached error is permanent, and the usual cause here (embedder cold or
// unreachable) is transient by nature — exactly the wrong thing to make sticky.
func TestErrorsAreNeverStored(t *testing.T) {
	m := openTemp(t, 0)
	boom := errors.New("embedder down")
	calls := 0
	wrapped := m.Wrap(func(string) ([]float64, error) {
		calls++
		if calls == 1 {
			return nil, boom
		}
		return []float64{7}, nil
	})
	if _, err := wrapped("x"); !errors.Is(err, boom) {
		t.Fatalf("want the embedder error through, got %v", err)
	}
	got, err := wrapped("x")
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if len(got) != 1 || got[0] != 7 {
		t.Fatalf("retry did not reach the embedder, got %v", got)
	}
}

// A corrupt record must read as a MISS, never as an empty vector. An empty
// []float64 handed to a cosine routine scores 0 against everything, which is
// indistinguishable from a legitimate "nothing similar" answer — the failure
// would be invisible at exactly the layer that makes routing decisions.
func TestCorruptRecordIsAMissNotAnEmptyVector(t *testing.T) {
	m := openTemp(t, 0)
	wrapped := m.Wrap(func(string) ([]float64, error) { return []float64{1, 2, 3}, nil })
	if _, err := wrapped("x"); err != nil {
		t.Fatal(err)
	}
	// Corrupt the stored record in place.
	k := m.key("x")
	if err := m.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bktVecs).Put([]byte(k), []byte{9, 9, 9})
	}); err != nil {
		t.Fatal(err)
	}
	if v, ok := m.get(k); ok || v != nil {
		t.Fatalf("corrupt record must not be served: ok=%v v=%v", ok, v)
	}
	recomputed := false
	got, err := m.Wrap(func(string) ([]float64, error) { recomputed = true; return []float64{4, 5, 6}, nil })("x")
	if err != nil {
		t.Fatal(err)
	}
	if !recomputed {
		t.Fatal("a corrupt entry must fall through to the embedder")
	}
	if len(got) != 3 || got[0] != 4 {
		t.Fatalf("got %v, want the freshly computed vector", got)
	}
}

// The seq bucket drives prune order. If an overwrite consumed a new slot, the
// seq bucket would hold several entries pointing at one key and the prune walk
// would delete a LIVE vector while believing it had evicted an old one.
func TestOverwriteDoesNotConsumeASequenceSlot(t *testing.T) {
	m := openTemp(t, 0)
	wrapped := m.Wrap(func(string) ([]float64, error) { return []float64{1}, nil })
	for i := 0; i < 5; i++ {
		if _, err := wrapped("same"); err != nil {
			t.Fatal(err)
		}
	}
	// Force repeated put attempts for the identical key.
	for i := 0; i < 5; i++ {
		m.put(m.key("same"), []float64{1})
	}
	var vecs, seqs int
	_ = m.db.View(func(tx *bolt.Tx) error {
		vecs = tx.Bucket(bktVecs).Stats().KeyN
		seqs = tx.Bucket(bktSeq).Stats().KeyN
		return nil
	})
	if vecs != 1 || seqs != 1 {
		t.Fatalf("vecs=%d seqs=%d, want 1/1 — the seq index must stay 1:1 with stored vectors", vecs, seqs)
	}
}

func TestPruneEvictsOldestAndKeepsNewest(t *testing.T) {
	const max = 10
	m := openTemp(t, max)
	for i := 0; i < 40; i++ {
		text := string(rune('a'+i%26)) + string(rune('0'+i/26)) + "-unique"
		vi := float64(i)
		if _, err := m.Wrap(func(string) ([]float64, error) { return []float64{vi}, nil })(text); err != nil {
			t.Fatal(err)
		}
	}
	st, serr := m.Stats()
	if serr != nil {
		t.Fatal(serr)
	}
	if st.Distinct > max {
		t.Fatalf("distinct=%d exceeds cap %d", st.Distinct, max)
	}
	if st.Distinct == 0 {
		t.Fatal("prune emptied the store")
	}
	// The newest entry must still be present — prune walks oldest-first.
	last := string(rune('a'+39%26)) + string(rune('0'+39/26)) + "-unique"
	if _, ok := m.get(m.key(last)); !ok {
		t.Error("the most recently stored entry was evicted; prune is not oldest-first")
	}
	// And the seq index must not outlive the vectors it points at.
	var vecs, seqs int
	_ = m.db.View(func(tx *bolt.Tx) error {
		vecs = tx.Bucket(bktVecs).Stats().KeyN
		seqs = tx.Bucket(bktSeq).Stats().KeyN
		return nil
	})
	if vecs != seqs {
		t.Errorf("vecs=%d seqs=%d — prune must delete both sides", vecs, seqs)
	}
}

// Reporting "0% hit rate" for a memo nobody has consulted states a measured
// failure where no measurement exists. nil is the honest answer.
func TestHitRateIsNilUntilSomethingIsLookedUp(t *testing.T) {
	m := openTemp(t, 0)
	if st, err := m.Stats(); err != nil { t.Fatal(err) } else if st.HitRate != nil {
		t.Fatalf("HitRate = %v, want nil before any lookup", *st.HitRate)
	}
	wrapped := m.Wrap(func(string) ([]float64, error) { return []float64{1}, nil })
	if _, err := wrapped("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped("a"); err != nil {
		t.Fatal(err)
	}
	st, serr := m.Stats()
	if serr != nil {
		t.Fatal(serr)
	}
	if st.HitRate == nil {
		t.Fatal("HitRate is still nil after two lookups")
	}
	if *st.HitRate != 0.5 {
		t.Errorf("HitRate = %v, want 0.5 (one miss, one hit)", *st.HitRate)
	}
}

// A nil Memo is a valid, documented state (disabled, or the file is held by
// another process). It must behave as a transparent pass-through, not panic.
func TestNilMemoIsATransparentPassThrough(t *testing.T) {
	var m *Memo
	calls := 0
	fn := m.Wrap(func(string) ([]float64, error) { calls++; return []float64{1}, nil })
	for i := 0; i < 3; i++ {
		if _, err := fn("x"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatalf("calls=%d, want 3 — a nil memo must not memoize", calls)
	}
	if st, err := m.Stats(); err != nil { t.Fatalf("Stats on nil memo: %v", err) } else if st.Distinct != 0 || st.HitRate != nil {
		t.Errorf("nil memo stats should be empty, got %+v", st)
	}
	if err := m.Flush(); err != nil {
		t.Errorf("Flush on nil memo: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close on nil memo: %v", err)
	}
}

func TestOpenWithEmptyPathIsDisabledNotAnOutage(t *testing.T) {
	m, err := Open("", "e", "", 0)
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
	if m != nil {
		t.Fatal("a disabled memo must be nil")
	}
}

// Flush moves session counters into the persisted totals exactly once — double
// counting would inflate the very hit rate Phase 0.4 reads.
func TestFlushPersistsCountersExactlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memo.db")
	m, _ := Open(path, "e", "", 0)
	wrapped := m.Wrap(func(string) ([]float64, error) { return []float64{1}, nil })
	for i := 0; i < 3; i++ {
		if _, err := wrapped("a"); err != nil { // 1 miss + 2 hits
			t.Fatal(err)
		}
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(); err != nil { // second flush must be a no-op
		t.Fatal(err)
	}
	m.Close()

	m2, _ := Open(path, "e", "", 0)
	defer m2.Close()
	st, serr := m2.Stats()
	if serr != nil {
		t.Fatal(serr)
	}
	if st.LifetimeHits != 2 || st.LifetimeMisses != 1 {
		t.Fatalf("lifetime hits/misses = %d/%d, want 2/1", st.LifetimeHits, st.LifetimeMisses)
	}
}

// Shared must hand every in-process caller the SAME handle. Independent Opens
// would make all but the first lose the bbolt lock race against their own
// process and silently degrade to pass-through, after paying a full timeout each.
func TestSharedReturnsOneHandlePerPath(t *testing.T) {
	// Order matters: t.Cleanup is LIFO, so TempDir's removal (registered by the
	// call below) must be registered BEFORE CloseShared — otherwise the temp dir
	// is deleted while bolt still holds the file open and cleanup fails on
	// Windows, where an open handle blocks unlink.
	dir := t.TempDir()
	t.Cleanup(func() { _ = CloseShared() })
	path := filepath.Join(dir, "memo.db")
	a, err := Shared(path, "e", "", 0)
	if err != nil {
		t.Fatalf("Shared: %v", err)
	}
	b, err := Shared(path, "e", "", 0)
	if err != nil {
		t.Fatalf("Shared (second): %v", err)
	}
	if a != b {
		t.Fatal("Shared returned two different handles for one path")
	}
}
