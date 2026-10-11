package calibration

import (
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Register F48: calibrating the live confidence thresholds on a fixture's margins would move them for traffic
// nobody sent. The control shows the task IS calibrated when its rows are not labelled.
func TestCalibrationReadsProductionRowsOnly(t *testing.T) {
	rows := func(task, class string) []ledger.Entry {
		var es []ledger.Entry
		for i := 0; i < 150; i++ {
			m := 0.1 + 0.9*float64(i)/150
			es = append(es, ledger.Entry{Task: task, Margin: m, Grounded: boolPtr(m >= 0.5), RunClass: class})
		}
		return es
	}
	run := func(es []ledger.Entry) map[string]float64 {
		th, _, err := Run(writeLedger(t, es), 0.10, nil, filepath.Join(t.TempDir(), "thresholds.json"), "")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return th
	}

	th := run(append(rows("classify", ""), rows("summarize", "bake")...))
	if _, ok := th["summarize"]; ok {
		t.Fatalf("a bake's task was calibrated: %v", th)
	}
	if _, ok := th["classify"]; !ok {
		t.Fatalf("the real task must still be calibrated: %v", th)
	}
	th = run(append(rows("classify", ""), rows("summarize", "")...))
	if _, ok := th["summarize"]; !ok {
		t.Fatalf("control: the same rows unlabelled must be calibrated: %v", th)
	}
}
