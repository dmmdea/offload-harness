package health

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// A local job its caller pinned for measurement writes two rows: the outer row, which carries the pin, and the
// pipeline's inner `agent` row, which names its parent job and carries nothing of the pin. ledger.ProductionRows
// skips the inner row with its parent; a reader that judges each row alone counted the inner row as real work
// (the operator guide says the inner row of a labelled job is left out with its parent).
func TestHealthLeavesOutTheInnerRowOfALabelledJob(t *testing.T) {
	rows := func(pin string) []ledger.Entry {
		es := stableEntries()
		for i := 0; i < 30; i++ {
			id := fmt.Sprintf("job-%d", i)
			outer := makeEntry("outer-tier", 0.8, 50.0, 200, false)
			outer.JobID, outer.PinReason = id, pin
			inner := makeEntry("inner-tier", 0.1, 3.0, 9000, true)
			inner.ParentJobID = id
			es = append(es, outer, inner)
		}
		return es
	}
	run := func(es []ledger.Entry) Report {
		rpt, err := Run(writeLedger(t, es), filepath.Join(t.TempDir(), "out.json"), "")
		if err != nil {
			t.Fatal(err)
		}
		return rpt
	}

	rpt := run(rows("measurement"))
	for _, tier := range []string{"inner-tier", "outer-tier"} {
		if _, ok := rpt.Tiers[tier]; ok {
			t.Fatalf("the %s of a measurement-pinned job reached the health report: %v", tier, rpt.Tiers)
		}
	}
	if th := rpt.Tiers["stable-tier"]; th.N != 30 {
		t.Fatalf("the real tier must keep all its rows: %+v", th)
	}

	if _, ok := run(rows("")).Tiers["inner-tier"]; !ok {
		t.Fatal("control: the same inner rows of an unpinned job are real work and must be read")
	}
}
