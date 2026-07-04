package confhead

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/local-offload/internal/ledger"
)

func bptr(v bool) *bool { return &v }

func TestFeatureRowAndLabel(t *testing.T) {
	e := ledger.Entry{Task: "classify", Margin: 0.8, Retries: 1, InputChars: 100, Truncated: true,
		Feat: map[string]float64{"len_chars": 100, "n_words": 18}, Grounded: bptr(true)}
	fr := FeatureRow(e)
	if fr["margin"] != 0.8 || fr["retries"] != 1 || fr["truncated"] != 1 || fr["len_chars"] != 100 {
		t.Fatalf("bad feature row: %v", fr)
	}
	if y, ok := Label(e); !ok || y != 1 {
		t.Fatalf("expected labeled correct, got y=%v ok=%v", y, ok)
	}
	if _, ok := Label(ledger.Entry{Task: "classify"}); ok {
		t.Fatal("entry with no Grounded/EscalatedAgreed should be unlabeled")
	}
}

func TestFitPredictRanks(t *testing.T) {
	// Synthetic: high-margin/low-retry rows are correct; low-margin/high-retry are wrong.
	var es []ledger.Entry
	for i := 0; i < 120; i++ {
		good := i%2 == 0
		m := 0.9
		r := 0
		g := true
		if !good {
			m = 0.1
			r = 2
			g = false
		}
		es = append(es, ledger.Entry{Task: "classify", Margin: m, Retries: r, InputChars: 50,
			Feat: map[string]float64{"len_chars": 50, "n_words": 9, "n_numbers": 0, "n_caps": 1, "has_code": 0, "has_url": 0},
			Grounded: bptr(g)})
	}
	m := Fit(es)
	pGood := m.Predict("classify", FeatureRow(ledger.Entry{Task: "classify", Margin: 0.9, Retries: 0, InputChars: 50,
		Feat: map[string]float64{"len_chars": 50, "n_words": 9}}))
	pBad := m.Predict("classify", FeatureRow(ledger.Entry{Task: "classify", Margin: 0.1, Retries: 2, InputChars: 50,
		Feat: map[string]float64{"len_chars": 50, "n_words": 9}}))
	if !(pGood > pBad) {
		t.Fatalf("head should rank good>bad, got pGood=%v pBad=%v", pGood, pBad)
	}
	if pGood < 0 || pGood > 1 {
		t.Fatalf("p out of range: %v", pGood)
	}
}

// TestTrainMergesSidecar writes a labels sidecar with >=100 classify rows
// (labeled via EscalatedAgreed) and an EMPTY main ledger, then asserts Train
// trains classify entirely from the sidecar and the report says so.
func TestTrainMergesSidecar(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.jsonl")  // empty/missing — no rows
	labelsPath := filepath.Join(dir, "labels.jsonl")  // 120 classify rows
	outPath := filepath.Join(dir, "confhead-weights.json")

	for i := 0; i < 120; i++ {
		agreed := i%2 == 0
		e := ledger.Entry{Task: "classify", Margin: 0.5, Retries: 0, InputChars: 50,
			Feat:            map[string]float64{"len_chars": 50, "n_words": 9, "n_numbers": 0, "n_caps": 1, "has_code": 0, "has_url": 0},
			EscalatedAgreed: &agreed}
		if err := ledger.AppendLabel(labelsPath, e); err != nil {
			t.Fatalf("AppendLabel: %v", err)
		}
	}

	rep, err := Train(ledgerPath, labelsPath, outPath)
	if err != nil {
		t.Fatalf("Train: %v", err)
	}
	if !strings.Contains(rep, "task=classify") || !strings.Contains(rep, "trained OK") {
		t.Fatalf("expected classify trained from sidecar, report:\n%s", rep)
	}
	// 120 rows all from the sidecar — the report should attribute them.
	if !strings.Contains(rep, "120 rows") {
		t.Fatalf("expected 120 classify rows in report, got:\n%s", rep)
	}
}

// TestTrainEmptyLabelsPathOK confirms an empty labelsPath behaves like "no sidecar".
func TestTrainEmptyLabelsPathOK(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	outPath := filepath.Join(dir, "confhead-weights.json")
	if _, err := Train(ledgerPath, "", outPath); err != nil {
		t.Fatalf("empty labelsPath should not error: %v", err)
	}
}

func TestPredictNilSafe(t *testing.T) {
	var m *Model
	if m.Predict("classify", map[string]float64{}) != -1 {
		t.Fatal("nil model should return -1")
	}
}

// TestEmissionBoundary60 proves the emission gate is exactly 60 rows:
// a task with 60 labeled rows TRAINS (Fit returns a valid head) and a task
// with 59 rows is SKIPPED (Predict returns the sentinel -1).
// The OOF paired-bootstrap CI is the real adoption guard; this floor only
// governs whether a weights file entry is emitted at all.
func TestEmissionBoundary60(t *testing.T) {
	makeEntries := func(task string, n int) []ledger.Entry {
		es := make([]ledger.Entry, n)
		for i := range es {
			good := i%2 == 0
			es[i] = ledger.Entry{Task: task, Margin: 0, Retries: 0, InputChars: 50,
				Feat:     map[string]float64{"len_chars": 50, "n_words": 9, "n_numbers": 0, "n_caps": 1, "has_code": 0, "has_url": 0},
				Grounded: bptr(good)}
		}
		return es
	}

	// Exactly 60 rows: Fit (emission gate = 60) should TRAIN the head.
	es60 := makeEntries("summarize", 60)
	p60 := Fit(es60).Predict("summarize", FeatureRow(es60[0]))
	if p60 < 0 || p60 > 1 {
		t.Fatalf("Fit should train at exactly 60 rows (emission gate); got p=%v (sentinel=-1 means skipped)", p60)
	}

	// 59 rows: one below the floor — must be skipped (sentinel -1).
	es59 := makeEntries("summarize", 59)
	p59 := Fit(es59).Predict("summarize", FeatureRow(es59[0]))
	if p59 != -1 {
		t.Fatalf("Fit should skip a 59-row task (below emission gate of 60); got p=%v", p59)
	}
}

// TestFitWithMinRows confirms FitWithMinRows is parameterized independently of
// the production emission gate: with minRowsArg=40 a 60-row task trains, and
// with minRowsArg=61 the same 60-row task is skipped.
func TestFitWithMinRows(t *testing.T) {
	var es []ledger.Entry
	for i := 0; i < 60; i++ {
		good := i%2 == 0
		es = append(es, ledger.Entry{Task: "summarize", Margin: 0, Retries: 0, InputChars: 50,
			Feat:     map[string]float64{"len_chars": 50, "n_words": 9, "n_numbers": 0, "n_caps": 1, "has_code": 0, "has_url": 0},
			Grounded: bptr(good)})
	}
	// minRowsArg=40: 60 rows >= 40 → trains.
	p := FitWithMinRows(es, 40).Predict("summarize", FeatureRow(es[0]))
	if p < 0 || p > 1 {
		t.Fatalf("FitWithMinRows(40) should train the 60-row task, got p=%v", p)
	}
	// minRowsArg=61: 60 rows < 61 → skipped.
	p61 := FitWithMinRows(es, 61).Predict("summarize", FeatureRow(es[0]))
	if p61 != -1 {
		t.Fatalf("FitWithMinRows(61) should skip the 60-row task, got p=%v", p61)
	}
}
