package calibration

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/core"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Conformal calibration derives a margin cutoff from stored margins. The two
// decision-margin scales (register D-130) are ~10x apart, so a ledger spanning
// a flag flip would produce a cutoff that belongs to neither scale — a number
// that then gates production. Run therefore calibrates on ONE scale and reports
// what it left out.

func scaleEntries(n int, scale string, margin float64, correct bool) []ledger.Entry {
	c := correct
	out := make([]ledger.Entry, 0, n)
	for i := 0; i < n; i++ {
		// A little spread so the candidate set is not a single point.
		m := margin + float64(i%5)*0.001
		out = append(out, ledger.Entry{
			TS: int64(i), Task: "classify", Margin: m, MarginScale: scale, Grounded: &c,
		})
	}
	return out
}

func TestCalibrationFiltersToOneScale(t *testing.T) {
	rows := append(scaleEntries(80, core.MarginScaleMatched, 0.90, true), scaleEntries(80, core.MarginScaleFull, 0.09, true)...)
	p := writeLedger(t, rows)

	thr, report, err := Run(p, 0.10, nil, filepath.Join(t.TempDir(), "t.json"), core.MarginScaleFull)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := thr["classify"]
	if !ok {
		t.Fatalf("classify missing from thresholds; report:\n%s", report)
	}
	if got > 0.2 {
		t.Errorf("threshold %.4f is a matched-scale value — matched rows leaked into a full-scale calibration", got)
	}
	if !strings.Contains(report, "margin_scale") {
		t.Errorf("the report must say how many rows were excluded by scale:\n%s", report)
	}
	if !strings.Contains(report, "80") {
		t.Errorf("the report must name the excluded count (80):\n%s", report)
	}
}

func TestCalibrationOnTheMatchedScaleTreatsEmptyAsMatched(t *testing.T) {
	rows := append(scaleEntries(80, "", 0.90, true), scaleEntries(80, core.MarginScaleFull, 0.09, true)...)
	p := writeLedger(t, rows)

	thr, report, err := Run(p, 0.10, nil, filepath.Join(t.TempDir(), "t.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := thr["classify"]; got < 0.5 {
		t.Errorf("threshold %.4f — a full-scale row leaked into the matched calibration; report:\n%s", got, report)
	}
}

// A matched-only ledger (every box today) must calibrate exactly as before and
// say nothing about scale.
func TestCalibrationSaysNothingWhenNothingIsExcluded(t *testing.T) {
	p := writeLedger(t, scaleEntries(80, "", 0.90, true))
	_, report, err := Run(p, 0.10, nil, filepath.Join(t.TempDir(), "t.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(report, "margin_scale") {
		t.Errorf("no rows were excluded, so the report must not mention scale:\n%s", report)
	}
}
