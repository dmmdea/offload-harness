package calibration

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Register D-126: the only classify/triage label writer appends to the
// confhead labels SIDECAR (cfg.ConfHeadLabelsPath), while calibrate read the
// ledger alone — so 0 of 9,217 live ledger rows ever passed the filter and no
// threshold was ever fitted. Calibration reads every source it is given.
func TestRunSourcesFitsFromTheLabelsSidecar(t *testing.T) {
	var labels []ledger.Entry
	for i := 0; i < 80; i++ {
		m := 0.2 + float64(i)*0.01
		labels = append(labels, ledger.Entry{Task: "classify", Margin: m, EscalatedAgreed: boolPtr(m > 0.5)})
	}
	labelsPath := writeLedger(t, labels)
	ledgerPath := writeLedger(t, []ledger.Entry{{Task: "classify", Margin: 0.9}}) // unlabeled, as the live ledger is
	outPath := filepath.Join(t.TempDir(), "thresholds.json")

	// The ledger alone still fits nothing (the live shape).
	thr, _, err := Run(ledgerPath, 0.10, nil, outPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := thr["classify"]; ok {
		t.Fatalf("the unlabeled ledger alone must not fit a threshold: %v", thr)
	}

	thr, report, err := RunSources([]string{ledgerPath, labelsPath}, 0.10, nil, outPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := thr["classify"]; !ok {
		t.Fatalf("labels sidecar with 80 labeled rows must fit a classify threshold, got %v\n%s", thr, report)
	}
	if !strings.Contains(report, filepath.Base(labelsPath)) || !strings.Contains(report, "80") {
		t.Fatalf("the report must name each source and its usable rows:\n%s", report)
	}
}

func TestRunSourcesSkipsAMissingSource(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "thresholds.json")
	_, report, err := RunSources([]string{filepath.Join(t.TempDir(), "absent.jsonl")}, 0.10, nil, outPath, "")
	if err != nil {
		t.Fatalf("a missing source is a 0-row source, not an error: %v", err)
	}
	if !strings.Contains(report, "absent.jsonl") {
		t.Fatalf("the report must still name the source:\n%s", report)
	}
}
