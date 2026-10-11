package health

import (
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// Register F48: a bake's rows carry a fixture seat's margins and latencies. The health report is the box's, so it
// reads production rows only. The control shows the same rows ARE read when nothing labels them.
func TestHealthReadsProductionRowsOnly(t *testing.T) {
	var real, bake []ledger.Entry
	for i := 0; i < 30; i++ {
		real = append(real, makeEntry("stable-tier", 0.8, 50.0, 200, false))
		bake = append(bake, makeEntry("bake-tier", 0.1, 3.0, 9000, true))
	}
	labelled := append([]ledger.Entry(nil), real...)
	for _, e := range bake {
		e.RunClass = "bake"
		labelled = append(labelled, e)
	}
	rpt, err := Run(writeLedger(t, labelled), filepath.Join(t.TempDir(), "out.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rpt.Tiers["bake-tier"]; ok {
		t.Fatalf("a labelled bake tier reached the health report: %v", rpt.Tiers)
	}
	if th := rpt.Tiers["stable-tier"]; th.N != 30 {
		t.Fatalf("the real tier must keep all its rows: %+v", th)
	}

	rpt, err = Run(writeLedger(t, append(append([]ledger.Entry(nil), real...), bake...)), filepath.Join(t.TempDir(), "out2.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rpt.Tiers["bake-tier"]; !ok {
		t.Fatalf("control: unlabelled rows of the same tier must be read: %v", rpt.Tiers)
	}
}
