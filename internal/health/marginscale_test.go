package health

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// health baselines a tier's margin against its OWN early history and
// route-skips on a collapse. The two decision-margin scales (register D-130)
// differ by ~10x, so a ledger that spans a flag flip would read the flip itself
// as a quality collapse and route around a healthy tier. Run therefore keeps
// only the rows on the scale the box currently emits, and says how many it
// dropped.

func scaleRows(n int, scale string, margin float64) []ledger.Entry {
	out := make([]ledger.Entry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ledger.Entry{
			TS: int64(i), Task: "triage", ModelTier: "e4b",
			Margin: margin, MarginScale: scale,
			LatencyMs: 100, TokPerSec: 40,
		})
	}
	return out
}

func TestFullScaleBaselinesOnFullRowsOnly(t *testing.T) {
	rows := append(scaleRows(20, core.MarginScaleMatched, 0.90), scaleRows(20, core.MarginScaleFull, 0.09)...)
	p := writeLedger(t, rows)
	out := filepath.Join(t.TempDir(), "tier_overrides.json")

	rpt, err := Run(p, out, core.MarginScaleFull)
	if err != nil {
		t.Fatal(err)
	}
	th, ok := rpt.Tiers["e4b"]
	if !ok {
		t.Fatal("tier e4b missing from report")
	}
	if th.N != 20 {
		t.Fatalf("n = %d, want 20 — only the full-scale rows may be analysed", th.N)
	}
	if th.EWMAMargin > 0.2 {
		t.Errorf("ewma_margin = %.4f — a matched-scale row leaked into the full-scale baseline", th.EWMAMargin)
	}
	if th.RouteSkip {
		t.Error("route-skipped a healthy tier: the scale change was read as a quality collapse")
	}
	if rpt.ExcludedByScale != 20 {
		t.Errorf("excluded_by_scale = %d, want 20", rpt.ExcludedByScale)
	}
	joined := strings.Join(rpt.Notes, "\n")
	if !strings.Contains(joined, "margin_scale") {
		t.Errorf("the report must say rows were excluded by scale; notes = %q", joined)
	}
}

func TestMatchedScaleKeepsOldRowsWithNoScaleField(t *testing.T) {
	rows := append(scaleRows(10, "", 0.90), scaleRows(10, core.MarginScaleFull, 0.09)...)
	p := writeLedger(t, rows)
	rpt, err := Run(p, filepath.Join(t.TempDir(), "o.json"), core.MarginScaleMatched)
	if err != nil {
		t.Fatal(err)
	}
	if n := rpt.Tiers["e4b"].N; n != 10 {
		t.Fatalf("n = %d, want 10 — an empty scale reads as matched, a full row does not", n)
	}
	if rpt.ExcludedByScale != 10 {
		t.Errorf("excluded_by_scale = %d, want 10", rpt.ExcludedByScale)
	}
}

// The default box emits the matched scale and every historic row is matched:
// nothing is excluded, and the report says nothing about scale.
func TestMatchedOnlyLedgerExcludesNothing(t *testing.T) {
	p := writeLedger(t, scaleRows(12, "", 0.90))
	rpt, err := Run(p, filepath.Join(t.TempDir(), "o.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	if rpt.ExcludedByScale != 0 {
		t.Fatalf("excluded_by_scale = %d on a matched-only ledger, want 0", rpt.ExcludedByScale)
	}
	if strings.Contains(strings.Join(rpt.Notes, "\n"), "margin_scale") {
		t.Error("a matched-only ledger must produce no scale note")
	}
}
