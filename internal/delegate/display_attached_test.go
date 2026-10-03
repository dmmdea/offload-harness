package delegate

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// The delegator's free-card count reads display_active alone, as it did before the node
// published display_attached: an attached monitor is a fact about the box's lifetime, not a
// load on the card, and counting it would change what every host with card-scoped leases OFF
// advertises. (The allocator on the node itself does read it: gpuprobe.ScreenCardUUIDs.)
func TestCardIdleReadsDisplayActiveNotDisplayAttached(t *testing.T) {
	idle := gpuprobe.Device{Index: 1, UUID: "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff", TotalGiB: 16, FreeGiB: 15, UtilPct: 0, UtilKnown: true}
	if !cardIdle(idle) {
		t.Fatal("an idle card with no display must be free")
	}
	attached := idle
	attached.DisplayAttached = true
	if !cardIdle(attached) {
		t.Fatal("an attached monitor alone must not change the free-card count a node advertises")
	}
	if !cardFree(attached, 1) {
		t.Fatal("cardFree must agree with cardIdle")
	}
	active := idle
	active.DisplayActive = true
	if cardIdle(active) {
		t.Fatal("display_active must still exclude the card")
	}
}
