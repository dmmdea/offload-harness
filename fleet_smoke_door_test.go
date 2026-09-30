package main

import "testing"

// TestSmokeContractStampsItsDoor: fleet-smoke is the one in-repository caller of the
// delegation engine that built its contract by hand, and it stamped no door, so its
// rows read as `delegate` — the engine's own name, the value a row carries when a
// caller stamped none (docs/FLEET-NODE.md, "The delegation ledger row"). Measurement
// traffic that cannot be told from an unstamped entry point is also traffic a share
// figure cannot exclude (register C-63).
func TestSmokeContractStampsItsDoor(t *testing.T) {
	if got := smokeContract("node-a").Door; got != "cli:fleet-smoke" {
		t.Fatalf("smokeContract door = %q, want %q", got, "cli:fleet-smoke")
	}
}
