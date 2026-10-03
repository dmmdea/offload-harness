package mcpserver

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/config"
	"github.com/dmmdea/offload-harness/internal/gpuactivity"
	"github.com/dmmdea/offload-harness/internal/gpucards"
)

// offload_status builds its per-card table from the same nvidia-smi sample the lease verdict
// took. A card the monitor is attached to is the display card there too, even while the
// driver reads display_active Disabled (the screen asleep): the table the operator reads
// must agree with the allocator.
func TestStatusCardTableMarksAnAttachedCardAsTheDisplayCard(t *testing.T) {
	act := gpuactivity.View{GPUs: []gpuactivity.GPU{
		{Index: 0, UUID: "GPU-aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee", Name: "RTX 5060 Ti", MemTotalMiB: 16311, UtilKnown: true},
		{Index: 1, UUID: "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff", Name: "RTX 5070 Ti", MemTotalMiB: 16303, UtilKnown: true, DisplayAttached: true},
		{Index: 2, UUID: "GPU-cccc3333-dddd-eeee-ffff-000000000000", Name: "RTX 5060 Ti", MemTotalMiB: 16311, UtilKnown: true},
	}}
	sec := gpuCardsSection(config.Config{}, act)
	rows, ok := sec["cards"].([]gpucards.Row)
	if !ok || len(rows) != 3 {
		t.Fatalf("cards = %#v, want three rows", sec["cards"])
	}
	for i, want := range []bool{false, true, false} {
		if rows[i].Display != want {
			t.Errorf("card %d Display = %v, want %v", i, rows[i].Display, want)
		}
	}
}
