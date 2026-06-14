package report

import (
	"testing"

	"github.com/dmmdea/local-offload-pp-cli/internal/ledger"
)

func b(v bool) *bool { return &v }

func TestSummarize(t *testing.T) {
	es := []ledger.Entry{
		{Task: "extract", Grounded: b(true), Escalations: 0, Margin: 0.9, TokensOut: 100},
		{Task: "extract", Grounded: b(false), Escalations: 0, Margin: 0.4, TokensOut: 100},
		{Task: "extract", Grounded: b(true), Escalations: 1, Margin: 0.5, TokensOut: 200}, // escalation-resolved
		{Task: "extract", Deferred: true, TokensOut: 0},
	}
	rep := Summarize(es, 0.35)["extract"]
	if rep.N != 4 || rep.Deferred != 1 || rep.EscalationResolved != 1 {
		t.Fatalf("bad counts: %+v", rep)
	}
	if rep.GroundedRate < 0.66 || rep.GroundedRate > 0.67 { // 2 grounded of 3 labeled
		t.Fatalf("grounded rate: %+v", rep)
	}
}
