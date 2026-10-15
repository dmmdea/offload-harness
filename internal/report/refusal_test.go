package report

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/ledger"
)

// `local-offload stats`: a refused placement did no work (ledger.Entry.Refusal), so it is not a job. Counted, it was
// a deferred job of its task: N one too high and a defer rate that read queue pressure as the seats declining work.
func TestStatsDoNotCountARefusalRowAsAJob(t *testing.T) {
	rows := []ledger.Entry{
		{Task: "agent_delegate", JobID: "agd-1", TokensOut: 40},
		{Task: "agent_delegate", JobID: "agd-2", Deferred: true, Reason: "stalled while decoding"},
		{Task: "agent_delegate", JobID: "agd-3", Deferred: true, Reason: "dispatch: status 503: queue full", Refusal: true},
	}
	got := Summarize(rows, 0.5)["agent_delegate"]
	if got.N != 2 || got.Deferred != 1 || got.DeferRate != 0.5 {
		t.Fatalf("agent_delegate = %+v, want n 2, deferred 1, defer rate 0.5 (the refusal row is not a job)", got)
	}
}
