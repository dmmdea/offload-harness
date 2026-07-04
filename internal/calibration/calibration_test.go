package calibration

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmmdea/local-offload/internal/ledger"
)

// ---- helpers ----------------------------------------------------------------

func boolPtr(b bool) *bool { return &b }

// writeLedger serialises entries to a temporary JSONL file and returns its path.
func writeLedger(t *testing.T, entries []ledger.Entry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// ---- tests ------------------------------------------------------------------

// TestConformalThresholdKnown builds a synthetic dataset where all calls with
// margin >= 0.6 are correct and all below are wrong. With alpha=0.05 the
// algorithm must select a threshold that separates them.
func TestConformalThresholdKnown(t *testing.T) {
	// 150 correct calls at margin 0.8, 50 wrong calls at margin 0.3.
	pts := make([]point, 0, 200)
	for i := 0; i < 150; i++ {
		pts = append(pts, point{margin: 0.8, correct: true})
	}
	for i := 0; i < 50; i++ {
		pts = append(pts, point{margin: 0.3, correct: false})
	}

	thr, _ := conformalThreshold(pts, 0.05)

	// The threshold must exclude the 0.3 group (wrong) and accept the 0.8 group.
	// Any value in (0.3, 0.8] achieves 0 error, so the algo should pick 0.8
	// (largest qualifying t whose adjusted rate <= 0.05 — which 0.8 satisfies).
	if thr < 0.5 {
		t.Fatalf("expected threshold >= 0.5 to exclude wrong calls, got %.4f", thr)
	}

	// Achieved error at chosen threshold must be 0 (only correct calls accepted).
	_, empErr := adjustedRate(pts, thr)
	if empErr > 1e-9 {
		t.Fatalf("expected 0 empirical error at threshold %.4f, got %.6f", thr, empErr)
	}
}

// TestConformalThresholdFallback: when every threshold produces error > alpha,
// the function must fall back to the maximum margin (most conservative).
func TestConformalThresholdFallback(t *testing.T) {
	// All calls are wrong, so no threshold can achieve low error.
	pts := make([]point, 120)
	for i := range pts {
		pts[i] = point{margin: 0.5 + float64(i)*0.003, correct: false}
	}

	thr, _ := conformalThreshold(pts, 0.05)

	maxM := 0.0
	for _, p := range pts {
		if p.margin > maxM {
			maxM = p.margin
		}
	}
	if math.Abs(thr-maxM) > 1e-9 {
		t.Fatalf("fallback: expected max margin %.4f, got %.4f", maxM, thr)
	}
}

// TestRunFullPipeline exercises Run end-to-end with a synthetic ledger.
// We generate 200 classify entries: margins in (0,1], Grounded label.
// Correct when margin >= 0.5, wrong otherwise.
func TestRunFullPipeline(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	entries := make([]ledger.Entry, 200)
	for i := range entries {
		m := 0.1 + rng.Float64()*0.9 // in (0.1, 1.0)
		correct := m >= 0.5
		entries[i] = ledger.Entry{
			Task:     "classify",
			Margin:   m,
			Grounded: boolPtr(correct),
		}
	}

	ledgerPath := writeLedger(t, entries)
	outPath := filepath.Join(t.TempDir(), "thresholds.json")

	thresholds, report, err := Run(ledgerPath, 0.10, nil, outPath)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if _, ok := thresholds["classify"]; !ok {
		t.Fatalf("expected 'classify' in thresholds, got %v\nReport:\n%s", thresholds, report)
	}

	// Threshold must be in a reasonable range.
	thr := thresholds["classify"]
	if thr < 0 || thr > 1 {
		t.Fatalf("threshold out of [0,1]: %.4f", thr)
	}

	// Output file must be valid JSON.
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read outPath: %v", err)
	}
	var parsed map[string]float64
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("output JSON invalid: %v\ncontent: %s", err, raw)
	}
	if parsed["classify"] != thr {
		t.Fatalf("JSON file threshold %.4f != returned %.4f", parsed["classify"], thr)
	}

	t.Logf("report:\n%s", report)
}

// TestRunSkipsSmallTasks: tasks with fewer than 60 labeled rows must be omitted.
func TestRunSkipsSmallTasks(t *testing.T) {
	// 50 summarize entries (< 60) and 150 triage entries (>= 60).
	entries := make([]ledger.Entry, 0, 200)
	for i := 0; i < 50; i++ {
		entries = append(entries, ledger.Entry{
			Task:     "summarize",
			Margin:   0.7,
			Grounded: boolPtr(true),
		})
	}
	for i := 0; i < 150; i++ {
		entries = append(entries, ledger.Entry{
			Task:     "triage",
			Margin:   0.6,
			Grounded: boolPtr(true),
		})
	}

	ledgerPath := writeLedger(t, entries)
	outPath := filepath.Join(t.TempDir(), "thresholds.json")

	thresholds, report, err := Run(ledgerPath, 0.10, nil, outPath)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if _, ok := thresholds["summarize"]; ok {
		t.Fatalf("summarize should be omitted (n<60), report:\n%s", report)
	}
	if _, ok := thresholds["triage"]; !ok {
		t.Fatalf("triage should be present (n>=60), report:\n%s", report)
	}
}

// TestCalibrationEmissionBoundary60 proves the calibration emission gate is
// exactly 60 rows: a task with 60 rows is calibrated; one with 59 is skipped.
func TestCalibrationEmissionBoundary60(t *testing.T) {
	makeEntries := func(task string, n int) []ledger.Entry {
		es := make([]ledger.Entry, n)
		for i := range es {
			es[i] = ledger.Entry{
				Task:     task,
				Margin:   0.1 + float64(i%9)*0.1, // varied margins so calibration has candidates
				Grounded: boolPtr(i%2 == 0),
			}
		}
		return es
	}

	// 60 rows: at the floor — must appear in thresholds.
	entries60 := makeEntries("classify", 60)
	// 59 rows under a different task — must be omitted.
	entries59 := makeEntries("extract", 59)

	ledgerPath := writeLedger(t, append(entries60, entries59...))
	outPath := filepath.Join(t.TempDir(), "thresholds.json")

	thresholds, report, err := Run(ledgerPath, 0.10, nil, outPath)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if _, ok := thresholds["classify"]; !ok {
		t.Fatalf("classify (n=60) should be calibrated, report:\n%s", report)
	}
	if _, ok := thresholds["extract"]; ok {
		t.Fatalf("extract (n=59) should be omitted (below 60-row gate), report:\n%s", report)
	}
}

// TestRunSkipsCacheHitAndZeroMargin verifies filtering behaviour.
func TestRunSkipsCacheHitAndZeroMargin(t *testing.T) {
	entries := make([]ledger.Entry, 0, 300)
	// 150 valid classify entries.
	for i := 0; i < 150; i++ {
		entries = append(entries, ledger.Entry{
			Task:     "classify",
			Margin:   0.7,
			Grounded: boolPtr(true),
		})
	}
	// 100 cache hit entries — must be skipped.
	for i := 0; i < 100; i++ {
		entries = append(entries, ledger.Entry{
			Task:     "classify",
			Margin:   0.9,
			CacheHit: true,
			Grounded: boolPtr(true),
		})
	}
	// 100 zero-margin entries — must be skipped.
	for i := 0; i < 100; i++ {
		entries = append(entries, ledger.Entry{
			Task:     "classify",
			Margin:   0,
			Grounded: boolPtr(true),
		})
	}

	ledgerPath := writeLedger(t, entries)
	outPath := filepath.Join(t.TempDir(), "thresholds.json")

	thresholds, _, err := Run(ledgerPath, 0.10, nil, outPath)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	// Only the 150 valid entries count; classify should still appear.
	if _, ok := thresholds["classify"]; !ok {
		t.Fatal("classify should be present")
	}
}

// TestRunNoLabel: entries with no Grounded/EscalatedAgreed are skipped.
func TestRunNoLabel(t *testing.T) {
	entries := make([]ledger.Entry, 200)
	for i := range entries {
		entries[i] = ledger.Entry{Task: "classify", Margin: 0.7}
		// Grounded and EscalatedAgreed intentionally nil
	}

	ledgerPath := writeLedger(t, entries)
	outPath := filepath.Join(t.TempDir(), "thresholds.json")

	thresholds, _, err := Run(ledgerPath, 0.10, nil, outPath)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if _, ok := thresholds["classify"]; ok {
		t.Fatal("classify should be omitted — no usable labels")
	}
}

// TestRunPerTaskAlpha: per-task alpha override is respected.
// In Conformal Risk Control the algorithm picks the LARGEST t where the
// adjusted error rate <= alpha. With a relaxed alpha (0.30), very high t values
// (accepting few, very-confident calls) can still satisfy the bound and the
// algorithm picks them. With a strict alpha (0.01), the +1 Laplace correction
// in (n*err+1)/(n+1) means we need many accepted calls to dilute it below 0.01,
// so the chosen threshold is lower (accepts more calls).
// Monotonicity: stricter alpha → lower or equal threshold.
func TestRunPerTaskAlpha(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	makeEntries := func(n int) []ledger.Entry {
		out := make([]ledger.Entry, n)
		for i := range out {
			m := 0.1 + rng.Float64()*0.89
			out[i] = ledger.Entry{
				Task:     "classify",
				Margin:   m,
				Grounded: boolPtr(m >= 0.5),
			}
		}
		return out
	}

	ledgerPath := writeLedger(t, makeEntries(200))
	outStrict := filepath.Join(t.TempDir(), "strict.json")
	outRelax := filepath.Join(t.TempDir(), "relax.json")

	thrStrict, _, err := Run(ledgerPath, 0.01, map[string]float64{"classify": 0.01}, outStrict)
	if err != nil {
		t.Fatal(err)
	}
	thrRelax, _, err := Run(ledgerPath, 0.30, map[string]float64{"classify": 0.30}, outRelax)
	if err != nil {
		t.Fatal(err)
	}

	ts := thrStrict["classify"]
	tr := thrRelax["classify"]
	// Stricter alpha forces a lower threshold (must accept more calls to dilute the
	// Laplace correction term), so ts <= tr.
	if ts > tr {
		t.Fatalf("strict alpha=0.01 threshold %.4f should be <= relaxed alpha=0.30 threshold %.4f", ts, tr)
	}
}

// TestRunMissingLedger: a missing ledger file returns empty thresholds, not an error.
func TestRunMissingLedger(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "thresholds.json")
	thresholds, report, err := Run(filepath.Join(t.TempDir(), "nope.jsonl"), 0.10, nil, outPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(thresholds) != 0 {
		t.Fatalf("expected empty thresholds, got %v", thresholds)
	}
	_ = report
}

// TestRunMalformedLinesSkipped: malformed/partial lines in the ledger are silently skipped.
func TestRunMalformedLinesSkipped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	f, _ := os.Create(p)

	enc := json.NewEncoder(f)
	// Write 150 valid entries.
	for i := 0; i < 150; i++ {
		_ = enc.Encode(ledger.Entry{Task: "extract", Margin: 0.7, Grounded: boolPtr(true)})
	}
	// Append a partial / malformed line.
	_, _ = fmt.Fprint(f, `{"task":"extract","margin":0.5`)
	f.Close()

	outPath := filepath.Join(t.TempDir(), "thresholds.json")
	thresholds, _, err := Run(p, 0.10, nil, outPath)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if _, ok := thresholds["extract"]; !ok {
		t.Fatal("extract should be present despite trailing malformed line")
	}
}

// TestEscalatedAgreedLabel: EscalatedAgreed is used as the label when Grounded is nil.
func TestEscalatedAgreedLabel(t *testing.T) {
	entries := make([]ledger.Entry, 150)
	for i := range entries {
		agreed := true
		entries[i] = ledger.Entry{
			Task:            "triage",
			Margin:          0.65,
			EscalatedAgreed: &agreed,
		}
	}

	ledgerPath := writeLedger(t, entries)
	outPath := filepath.Join(t.TempDir(), "thresholds.json")

	thresholds, _, err := Run(ledgerPath, 0.10, nil, outPath)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if _, ok := thresholds["triage"]; !ok {
		t.Fatal("triage should be present — EscalatedAgreed provides labels")
	}
}

// TestAdjustedRate validates the CRC adjusted-rate formula directly.
func TestAdjustedRate(t *testing.T) {
	// 90 correct, 10 wrong, all at margin 0.7.
	pts := make([]point, 100)
	for i := range pts {
		pts[i] = point{margin: 0.7, correct: i < 90}
	}
	adj, emp := adjustedRate(pts, 0.7)
	if math.Abs(emp-0.10) > 1e-9 {
		t.Fatalf("empirical error: want 0.10, got %.6f", emp)
	}
	// (100*0.10 + 1) / (100+1) = 11/101
	want := 11.0 / 101.0
	if math.Abs(adj-want) > 1e-9 {
		t.Fatalf("adjusted rate: want %.6f, got %.6f", want, adj)
	}
}

// TestAdjustedRateNoAccepted: threshold above all margins → 1.0 by convention.
func TestAdjustedRateNoAccepted(t *testing.T) {
	pts := []point{{margin: 0.3, correct: true}}
	adj, emp := adjustedRate(pts, 0.9)
	if adj != 1.0 || emp != 1.0 {
		t.Fatalf("no accepted calls: want adj=1 emp=1, got adj=%.2f emp=%.2f", adj, emp)
	}
}
