package pipeline

import (
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/ledger"
)

// E-04: an accelerator call is one ledger row — task = the sidecar tool,
// model_tier = the device (or "<node>:<device>" when a fleet node ran it),
// latency and the defer verdict carried like every other task's row.
func TestRecordAccelWritesOneLedgerRowPerCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	led, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()
	p := New(config.Config{}, nil, nil, led)

	p.RecordAccel("classify_image", "coral-edgetpu", 612, false, "")
	p.RecordAccel("object_detect", "lenovo-ampere16:coral-edgetpu", 2048, true, "coral-edgetpu (fleet): sidecar busy")

	rows, err := ledger.ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].Task != "classify_image" || rows[0].ModelTier != "coral-edgetpu" || rows[0].LatencyMs != 612 || rows[0].Deferred {
		t.Errorf("local row: %+v", rows[0])
	}
	if rows[1].Task != "object_detect" || rows[1].ModelTier != "lenovo-ampere16:coral-edgetpu" || !rows[1].Deferred || rows[1].Reason == "" {
		t.Errorf("fleet defer row: %+v", rows[1])
	}
}

// A box whose ledger is held by another process (led == nil) records nothing
// and never panics — the same contract every other task honours.
func TestRecordAccelWithoutALedgerIsANoOp(t *testing.T) {
	p := New(config.Config{}, nil, nil, nil)
	p.RecordAccel("classify_image", "coral-edgetpu", 1, false, "")
	var nilPipeline *Pipeline
	nilPipeline.RecordAccel("classify_image", "coral-edgetpu", 1, false, "")
}
