package core

import "testing"

// CardHeld decides, by class, which deferrals are a place in line and not a failure. The literals are
// pinned because the ledger, the wire and the PAIR card's close all key on them.
func TestCardHeldIsExactlyTheHeldClasses(t *testing.T) {
	if ErrClassGPUBusy != "gpu_busy" || ErrClassGPUQueued != "gpu_queued" || ErrClassComposeBusy != "compose_busy" {
		t.Fatalf("the held classes are on the wire and in the ledger: %q %q %q", ErrClassGPUBusy, ErrClassGPUQueued, ErrClassComposeBusy)
	}
	for class, want := range map[string]bool{
		"gpu_busy":              true,
		"gpu_queued":            true,
		"compose_busy":          true, // the compose slot is held by another composition
		"":                      false,
		"gpu_lease_unavailable": false, // a configuration fault: the lease location cannot be used
		"timeout":               false,
		"oom":                   false,
		"http_5xx":              false,
		"conn_refused":          false,
		"GPU_BUSY":              false, // classes are exact
		"gpu_busy ":             false,
	} {
		if got := CardHeld(class); got != want {
			t.Errorf("CardHeld(%q) = %v, want %v", class, got, want)
		}
	}
}
