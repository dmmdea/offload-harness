package calibration

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// The inner `agent` row of a local job its caller pinned for measurement names its parent and carries nothing of
// the pin (see ledger.ProductionRows). Calibrating the live thresholds on a measurement's margins would move them
// for traffic nobody sent; a reader that judges each row alone read the inner rows as real.
func TestCalibrationLeavesOutTheInnerRowsOfALabelledJob(t *testing.T) {
	rows := func(task, pin string) []ledger.Entry {
		var es []ledger.Entry
		for i := 0; i < 150; i++ {
			id := fmt.Sprintf("%s-%d", task, i)
			m := 0.1 + 0.9*float64(i)/150
			es = append(es,
				ledger.Entry{JobID: id, Task: task, PinReason: pin},
				ledger.Entry{ParentJobID: id, Task: task, Margin: m, Grounded: boolPtr(m >= 0.5)})
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

	th := run(append(rows("classify", ""), rows("summarize", "measurement")...))
	if _, ok := th["summarize"]; ok {
		t.Fatalf("the inner rows of measurement-pinned jobs were calibrated: %v", th)
	}
	if _, ok := th["classify"]; !ok {
		t.Fatalf("the real task must still be calibrated: %v", th)
	}
	th = run(append(rows("classify", ""), rows("summarize", "")...))
	if _, ok := th["summarize"]; !ok {
		t.Fatalf("control: the same rows of unpinned jobs must be calibrated: %v", th)
	}
}
