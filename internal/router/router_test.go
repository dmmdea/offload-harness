package router

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// ---- helpers ------------------------------------------------------------

func boolPtr(b bool) *bool { return &b }

// writeLedger serialises entries to a temp JSONL file and returns its path.
func writeLedger(t *testing.T, entries []ledger.Entry) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "ledger-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, e := range entries {
		line, _ := json.Marshal(e)
		f.Write(line)
		f.Write([]byte("\n"))
	}
	return f.Name()
}

// makeFeat builds a feature map with the given len_chars value; all others
// are set to 0.
func makeFeat(lenChars float64) map[string]float64 {
	return map[string]float64{
		"len_chars": lenChars,
		"n_words":   lenChars / 6,
		"n_numbers": 0,
		"n_caps":    0,
		"has_code":  0,
		"has_url":   0,
	}
}

// ---- synthetic data builder ---------------------------------------------

// syntheticEntries creates n entries for the given task.
// Short inputs (len_chars < splitAt) are accepted at E2B (y=1).
// Long inputs (len_chars >= splitAt) escalate (y=0).
func syntheticEntries(task string, n int, splitAt float64, rng *rand.Rand) []ledger.Entry {
	entries := make([]ledger.Entry, n)
	for i := 0; i < n; i++ {
		lc := rng.Float64() * 2000 // 0..2000
		tier := "gemma4-e2b"
		accepted := lc < splitAt
		esc := 0
		if !accepted {
			esc = 1
		}
		entries[i] = ledger.Entry{
			TS:          int64(i + 1),
			Task:        task,
			ModelTier:   tier,
			Escalations: esc,
			Deferred:    false,
			Grounded:    nil,
			Feat:        makeFeat(lc),
		}
	}
	return entries
}

// ---- tests --------------------------------------------------------------

// TestTrainAndLoad verifies the full Train → Load → PreferLargerEntry cycle.
func TestTrainAndLoad(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	// 300 classify rows: short (<800) succeed at E2B, long (>=800) escalate.
	const splitAt = 800.0
	entries := syntheticEntries("classify", 300, splitAt, rng)
	// Also add some "summarize" rows which should be ignored (not trained).
	entries = append(entries, syntheticEntries("summarize", 300, splitAt, rng)...)

	dir := t.TempDir()
	ledgerPath := writeLedger(t, entries)
	outPath := filepath.Join(dir, "router-weights.json")

	report, err := Train(ledgerPath, "", outPath)
	if err != nil {
		t.Fatalf("Train failed: %v\nreport:\n%s", err, report)
	}
	t.Logf("Train report:\n%s", report)

	m := Load(outPath)
	if m == nil {
		t.Fatal("Load returned nil")
	}

	// Short input → should be accepted at E2B → PreferLargerEntry=false
	shortFeat := makeFeat(100)
	if m.PreferLargerEntry("classify", shortFeat) {
		t.Error("short input: expected PreferLargerEntry=false (E2B should handle it)")
	}

	// Long input → should escalate → PreferLargerEntry=true
	longFeat := makeFeat(1900)
	if !m.PreferLargerEntry("classify", longFeat) {
		t.Error("long input: expected PreferLargerEntry=true (skip E2B)")
	}

	// Summarize is not trained → PreferLargerEntry must return false (default)
	if m.PreferLargerEntry("summarize", longFeat) {
		t.Error("summarize: no model trained, expected PreferLargerEntry=false")
	}
}

// TestTriageTask mirrors TestTrainAndLoad for "triage".
func TestTriageTask(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	const splitAt = 500.0
	entries := syntheticEntries("triage", 250, splitAt, rng)

	dir := t.TempDir()
	lp := writeLedger(t, entries)
	op := filepath.Join(dir, "w.json")

	report, err := Train(lp, "", op)
	if err != nil {
		t.Fatalf("Train: %v\n%s", err, report)
	}
	m := Load(op)
	if m == nil {
		t.Fatal("Load nil")
	}

	if m.PreferLargerEntry("triage", makeFeat(50)) {
		t.Error("short triage: should stay at E2B")
	}
	if !m.PreferLargerEntry("triage", makeFeat(1950)) {
		t.Error("long triage: should prefer larger")
	}
}

// TestInsufficientRows ensures tasks with <200 rows are skipped.
func TestInsufficientRows(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	entries := syntheticEntries("classify", 100, 500, rng) // only 100

	dir := t.TempDir()
	lp := writeLedger(t, entries)
	op := filepath.Join(dir, "w.json")

	report, err := Train(lp, "", op)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Logf("report: %s", report)

	m := Load(op)
	if m == nil {
		t.Fatal("Load nil")
	}
	// classify was skipped → must default false
	if m.PreferLargerEntry("classify", makeFeat(1900)) {
		t.Error("expected false for untrained task")
	}
}

// TestLoadMissing verifies Load returns nil for a missing file.
func TestLoadMissing(t *testing.T) {
	m := Load(filepath.Join(t.TempDir(), "nonexistent.json"))
	if m != nil {
		t.Error("expected nil from Load on missing file")
	}
}

// TestNilModel verifies a nil *Model is safe to call.
func TestNilModel(t *testing.T) {
	var m *Model
	if m.PreferLargerEntry("classify", makeFeat(1000)) {
		t.Error("nil model must return false")
	}
}

// TestMalformedLedgerLines ensures malformed lines are skipped without error.
func TestMalformedLedgerLines(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	entries := syntheticEntries("classify", 250, 600, rng)

	dir := t.TempDir()
	f, _ := os.CreateTemp(dir, "ledger-*.jsonl")
	// Write some good lines, some garbage, then more good lines.
	for i, e := range entries[:125] {
		line, _ := json.Marshal(e)
		f.Write(line)
		f.Write([]byte("\n"))
		if i == 60 {
			f.Write([]byte("{bad json\n"))
			f.Write([]byte("\n")) // blank
		}
	}
	for _, e := range entries[125:] {
		line, _ := json.Marshal(e)
		f.Write(line)
		f.Write([]byte("\n"))
	}
	f.Close()

	op := filepath.Join(dir, "w.json")
	_, err := Train(f.Name(), "", op)
	if err != nil {
		t.Fatalf("Train with malformed lines: %v", err)
	}
}

// TestEmptyLedger handles an empty ledger file.
func TestEmptyLedger(t *testing.T) {
	dir := t.TempDir()
	lp := filepath.Join(dir, "empty.jsonl")
	os.WriteFile(lp, []byte{}, 0o600)

	op := filepath.Join(dir, "w.json")
	report, err := Train(lp, "", op)
	if err != nil {
		t.Fatalf("Train on empty ledger: %v\n%s", err, report)
	}
}

// TestPreferLargerEntryUnknownTask is a no-model path guard.
func TestPreferLargerEntryUnknownTask(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	entries := syntheticEntries("classify", 250, 600, rng)

	dir := t.TempDir()
	lp := writeLedger(t, entries)
	op := filepath.Join(dir, "w.json")
	Train(lp, "", op)

	m := Load(op)
	if m.PreferLargerEntry("extract", makeFeat(2000)) {
		t.Error("extract is not trained; must return false")
	}
}

// TestFeatOrderStability confirms featOrder always returns the same order.
func TestFeatOrderStability(t *testing.T) {
	feat := map[string]float64{
		"has_url":   1,
		"len_chars": 100,
		"n_words":   20,
		"n_numbers": 3,
		"n_caps":    5,
		"has_code":  0,
	}
	order1 := featOrder(feat)
	order2 := featOrder(feat)
	if fmt.Sprint(order1) != fmt.Sprint(order2) {
		t.Errorf("featOrder not stable: %v vs %v", order1, order2)
	}
	// Must start with len_chars (first canonical key present)
	if len(order1) == 0 || order1[0] != "len_chars" {
		t.Errorf("expected len_chars first, got %v", order1)
	}
}

// TestTrain_MergesRouterLabelSidecar verifies that synthesized E2B rows in a
// sidecar file are merged into the training set, allowing a task to reach
// minRows even when the main ledger is empty.
func TestTrain_MergesRouterLabelSidecar(t *testing.T) {
	dir := t.TempDir()
	led := filepath.Join(dir, "ledger.jsonl")
	side := filepath.Join(dir, "router-labels.jsonl")
	out := filepath.Join(dir, "router-weights.json")

	// main ledger: 0 usable E2B classify rows
	os.WriteFile(led, []byte(""), 0o644)

	// sidecar: 250 synthesized gemma4-e2b classify rows (alternating accept/skip)
	f, err := os.Create(side)
	if err != nil {
		t.Fatal(err)
	}
	tr := true
	fa := false
	for i := 0; i < 250; i++ {
		g := &tr
		if i%2 == 0 {
			g = &fa
		}
		e := ledger.Entry{
			Task:      "classify",
			ModelTier: "gemma4-e2b",
			Grounded:  g,
			Feat:      map[string]float64{"len_chars": float64(10 + i%5), "n_words": float64(2 + i%3)},
		}
		b, _ := json.Marshal(e)
		f.Write(append(b, '\n'))
	}
	f.Close()

	report, err := Train(led, side, out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "classify") || !strings.Contains(report, "trained OK") {
		t.Fatalf("expected classify trained via sidecar, got: %s", report)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("weights not written: %v", err)
	}
}

// TestTrain_MissingSidecarIgnored verifies that a missing sidecar path does not
// cause an error — Train proceeds with main ledger rows only.
func TestTrain_MissingSidecarIgnored(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	entries := syntheticEntries("classify", 250, 800, rng)
	lp := writeLedger(t, entries)
	op := filepath.Join(t.TempDir(), "w.json")

	// nonexistent sidecar path
	_, err := Train(lp, filepath.Join(t.TempDir(), "does-not-exist.jsonl"), op)
	if err != nil {
		t.Fatalf("missing sidecar should be ignored, got error: %v", err)
	}
}

func TestHasTask(t *testing.T) {
	var nilM *Model
	if nilM.HasTask("classify") {
		t.Fatal("nil receiver: want false")
	}
	m := &Model{tasks: map[string]taskWeights{"classify": {}}}
	if !m.HasTask("classify") {
		t.Fatal("known task: want true")
	}
	if m.HasTask("triage") {
		t.Fatal("unknown task: want false")
	}
}
