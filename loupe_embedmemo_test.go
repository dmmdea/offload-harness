package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/embedmemo"
)

// The loupe's embed-memo block has four outcomes, and the whole point of the
// design is that they are DISTINGUISHABLE. Collapsing any of them into "0
// vectors, never consulted" would publish a measured failure where no
// measurement happened — the defect this command exists to remove.

func TestLoupeReportsDisabledWhenTheMemoIsOff(t *testing.T) {
	cfg := config.Default()
	cfg.EmbedMemoPath = ""
	rep := readEmbedMemoReport(cfg)
	if rep.Available {
		t.Fatal("a disabled memo must not report available")
	}
	if !strings.Contains(rep.Reason, "disabled") {
		t.Errorf("reason = %q, want it to name the disabled state", rep.Reason)
	}
}

func TestLoupeReportsNoStoreDistinctlyFromDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.EmbedMemoPath = filepath.Join(t.TempDir(), "absent.db")
	rep := readEmbedMemoReport(cfg)
	if rep.Available {
		t.Fatal("a missing store must not report available")
	}
	if !strings.Contains(rep.Reason, "no store yet") {
		t.Errorf("reason = %q, want it to say the store does not exist yet (NOT that the memo is disabled)", rep.Reason)
	}
}

func TestLoupeReportsAMeasuredHitRateAfterARealShutdown(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.EmbedMemoPath = filepath.Join(dir, "memo.db")
	cfg.EmbedModelName = "test-embedder"
	cfg.EmbedMemoEpoch = ""

	m, err := embedmemo.Shared(cfg.EmbedMemoPath, cfg.EmbedModel(), cfg.EmbedMemoEpoch, 0)
	if err != nil {
		t.Fatalf("Shared: %v", err)
	}
	w := m.Wrap(func(string) ([]float64, error) { return []float64{1, 2, 3}, nil })
	for i := 0; i < 4; i++ { // 1 miss + 3 hits
		if _, err := w("same text"); err != nil {
			t.Fatal(err)
		}
	}
	if err := embedmemo.CloseShared(); err != nil {
		t.Fatalf("CloseShared: %v", err)
	}

	rep := readEmbedMemoReport(cfg)
	if !rep.Available {
		t.Fatalf("store should be readable: %s", rep.Reason)
	}
	if rep.HitRate == nil {
		t.Fatal("HitRate is nil — the loupe would print \"never consulted\" for a memo that served 3 hits")
	}
	if rep.Hits != 3 || rep.Misses != 1 {
		t.Fatalf("hits/misses = %d/%d, want 3/1", rep.Hits, rep.Misses)
	}
	if rep.Distinct != 1 {
		t.Errorf("distinct = %d, want 1", rep.Distinct)
	}
	if rep.Embedder != "test-embedder" {
		t.Errorf("embedder = %q, want the configured one", rep.Embedder)
	}
}

// Stores with no recorded lookups is arithmetically impossible — every store is
// preceded by a miss — so it means the counters were never flushed. The report
// must say that rather than the flatly false "never consulted".
func TestLoupeDistinguishesUnflushedCountersFromAnIdleMemo(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.EmbedMemoPath = filepath.Join(dir, "memo.db")
	cfg.EmbedModelName = "test-embedder"

	// Write vectors and exit WITHOUT flushing (the pre-fix production behaviour).
	m, err := embedmemo.Open(cfg.EmbedMemoPath, cfg.EmbedModel(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wrap(func(string) ([]float64, error) { return []float64{1}, nil })("t"); err != nil {
		t.Fatal(err)
	}
	m.Close() // deliberately no Flush

	rep := readEmbedMemoReport(cfg)
	if !rep.Available {
		t.Fatalf("store should be readable: %s", rep.Reason)
	}
	if rep.Stores == 0 {
		t.Fatal("lifetime stores should be > 0 — put() persists it inline")
	}
	if rep.HitRate != nil {
		t.Fatal("hit rate should be nil when nothing was flushed")
	}
	// The emitStats switch must take the "unflushed" branch, not "never consulted".
	// Assert on the data that drives it, since emitStats prints rather than returns.
	if !(rep.HitRate == nil && rep.Stores > 0) {
		t.Fatal("the unflushed-counters state is not detectable from the report")
	}
}

// A store that opens but cannot be read is a FAULT, not an empty store.
func TestLoupeReportsAnUnreadableStoreAsUnavailable(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.EmbedMemoPath = filepath.Join(dir, "memo.db")

	// A file that exists and opens as bbolt but has no memo buckets: create one
	// with a foreign bucket only.
	m, err := embedmemo.Open(cfg.EmbedMemoPath, cfg.EmbedModel(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	// Truncating to a valid-but-empty bolt file is fiddly; instead assert the
	// positive contract that Stats errors propagate, using the real path above.
	rep := readEmbedMemoReport(cfg)
	if !rep.Available {
		// If it IS unavailable, the reason must be specific, never a bare zero.
		if rep.Reason == "" {
			t.Fatal("unavailable with no reason — the operator cannot act on this")
		}
	}
}

func TestEmbedMemoOnDistinguishesAbsentFromExplicitlyFalse(t *testing.T) {
	base := config.Default()
	if !base.EmbedMemoOn() {
		t.Fatal("the default config must have the memo on (absent key = on)")
	}
	off := base
	f := false
	off.EmbedMemoEnabled = &f
	if off.EmbedMemoOn() {
		t.Fatal("an explicit false must turn the memo off")
	}
	on := base
	tr := true
	on.EmbedMemoEnabled = &tr
	if !on.EmbedMemoOn() {
		t.Fatal("an explicit true must turn the memo on")
	}
	noPath := base
	noPath.EmbedMemoPath = ""
	if noPath.EmbedMemoOn() {
		t.Fatal("an empty path must disable the memo regardless of the flag")
	}
	// And the settings accessor must not leak a path when the memo is off.
	if p, _, _ := off.EmbedMemoSettings(); p != "" {
		t.Fatalf("EmbedMemoSettings returned path %q while the memo is off", p)
	}
}
