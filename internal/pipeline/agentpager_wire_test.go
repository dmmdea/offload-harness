package pipeline

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
)

// agentPagerWire is the unrecorded-versus-zero boundary on the delegation-log wire.
// The loop's CompactionPasses is a pointer, nil meaning no instrument recorded the run, and the
// wire object must stay ABSENT for that case: an object of zeros would read as a measured run
// that never compacted and never evicted, and the readout would count it as a real zero.

// TestAgentPagerWireAbsentStaysAbsent: no recorded pass count means no wire object, whatever else
// the report holds.
func TestAgentPagerWireAbsentStaysAbsent(t *testing.T) {
	if got := agentPagerWire(agent.PagerReport{}); got != nil {
		t.Fatalf("an empty report must stay absent on the wire, got %+v", got)
	}
	rate := 0.5
	partial := agent.PagerReport{Evictions: 3, DistinctEvicted: 2, Refetched: 1, RefetchRate: &rate, Basis: "measured"}
	if got := agentPagerWire(partial); got != nil {
		t.Fatalf("a report with counts but no recorded pass count must stay absent (the instrument did not record the run), got %+v", got)
	}
}

// TestAgentPagerWireMapsEveryField: a recorded report maps field for field (distinct values, so a
// swapped pair shows), and a recorded zero is an object, not an absence.
func TestAgentPagerWireMapsEveryField(t *testing.T) {
	passes := 3
	got := agentPagerWire(agent.PagerReport{Evictions: 7, EvictedBytes: 4096, DistinctEvicted: 5, Refetched: 2, Basis: "measured", CompactionPasses: &passes})
	want := core.AgentPager{Basis: "measured", Evictions: 7, EvictedBytes: 4096, DistinctEvicted: 5, Refetched: 2, CompactionPasses: 3}
	if got == nil || *got != want {
		t.Fatalf("wire object = %+v, want %+v", got, want)
	}
	zero := 0
	got = agentPagerWire(agent.PagerReport{Basis: "insufficient_data", CompactionPasses: &zero})
	if got == nil || *got != (core.AgentPager{Basis: "insufficient_data"}) {
		t.Fatalf("a recorded zero-pass run must be an object of its recorded values, got %+v", got)
	}
}
