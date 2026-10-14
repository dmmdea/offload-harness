package pipeline

import (
	"github.com/dmmdea/offload-harness/internal/agent"
	"github.com/dmmdea/offload-harness/internal/core"
)

// agentPagerWire maps the loop's pager report onto the wire object (RSI-OH-2). It returns nil
// when no instrument recorded the run (CompactionPasses nil), so that case stays an absent key
// on the wire instead of becoming an object of zeros.
func agentPagerWire(r agent.PagerReport) *core.AgentPager {
	if r.CompactionPasses == nil {
		return nil
	}
	return &core.AgentPager{
		Basis:            r.Basis,
		Evictions:        r.Evictions,
		EvictedBytes:     r.EvictedBytes,
		DistinctEvicted:  r.DistinctEvicted,
		Refetched:        r.Refetched,
		CompactionPasses: *r.CompactionPasses,
	}
}
