package delegate

import (
	"testing"

	"github.com/dmmdea/offload-harness/internal/gpuprobe"
)

// A remote node that reports display_attached (the monitor is plugged into this card) is not
// a free card, even when its display_active reads Disabled because the screen is asleep. A
// node that predates the field omits it and reads exactly as before.
func TestCardIdleSkipsAnAttachedDisplayCard(t *testing.T) {
	idle := gpuprobe.Device{Index: 1, UUID: "GPU-bbbb2222-cccc-dddd-eeee-ffffffffffff", TotalGiB: 16, FreeGiB: 15, UtilPct: 0, UtilKnown: true}
	if !cardIdle(idle) {
		t.Fatal("an idle card with no display must be free")
	}
	attached := idle
	attached.DisplayAttached = true
	if cardIdle(attached) {
		t.Fatal("a card the monitor is attached to must not read as free")
	}
	if cardFree(attached, 1) {
		t.Fatal("cardFree must agree with cardIdle")
	}
	active := idle
	active.DisplayActive = true
	if cardIdle(active) {
		t.Fatal("display_active must still exclude the card")
	}
}
