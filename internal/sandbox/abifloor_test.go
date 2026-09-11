package sandbox

import "testing"

// TestEffectiveABIFloorCoversTheNetworkGuarantee (0.115.22, register H-27):
// the cage promises "all TCP bind+connect denied", which the V4 rule set
// delivers only on Landlock ABI >= 4. A requested floor of 1 (what both
// callers asked for until 0.115.22) let a kernel with ABI 1-3 pass the
// fail-closed gate and run the cage with the network half silently missing.
func TestEffectiveABIFloorCoversTheNetworkGuarantee(t *testing.T) {
	for _, tc := range []struct{ in, want int }{{0, NetABIFloor}, {1, NetABIFloor}, {3, NetABIFloor}, {4, 4}, {7, 7}, {99, 99}} {
		if got := EffectiveABIFloor(tc.in); got != tc.want {
			t.Errorf("EffectiveABIFloor(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
	if NetABIFloor != 4 {
		t.Fatalf("NetABIFloor = %d, want 4 — landlock.V4 is the rule set the cage applies; the floor and the rule set must move together", NetABIFloor)
	}
}
