package hwdetect

import "testing"

// TestRAMTierMatchesTheInstallerTable asserts the SAME values detect.ps1's own
// self-test asserts (setup/detect.ps1: Assert-RamTier 128 high / 64 mid / 56 mid /
// 32 low / 16 min). Two implementations of a gate are only safe while something
// holds them to the same table, and the boundaries are load-bearing: 56 GB is what
// unlocks the 26B via --cpu-moe, and one GB below it must not.
func TestRAMTierMatchesTheInstallerTable(t *testing.T) {
	for _, tc := range []struct {
		ramGb int
		want  string
	}{
		{128, "high"}, {120, "high"}, // boundary
		{119, "mid"}, {64, "mid"}, {56, "mid"}, // 56 = the 26B unlock
		{55, "low"}, {32, "low"}, {28, "low"}, // boundary
		{27, "min"}, {16, "min"}, {0, "min"},
	} {
		if got := RAMTier(tc.ramGb); got != tc.want {
			t.Errorf("RAMTier(%d) = %q, want %q", tc.ramGb, got, tc.want)
		}
	}
}

// TestClassifyAlwaysStampsARAMTier: an empty ram_tier means "do not gate", so a
// verdict that forgot to set it would silently disable the 26B RAM gate AND the
// RAM-gated media seed rather than failing. Every profile path must stamp one.
func TestClassifyAlwaysStampsARAMTier(t *testing.T) {
	for _, f := range []Facts{
		{Vendor: "nvidia", Arch: "blackwell", VRAMGb: 16, GPUCount: 1, RAMGb: 128},
		{Vendor: "nvidia", Arch: "ampere", VRAMGb: 8, GPUCount: 1, RAMGb: 64},
		{Vendor: "nvidia", Arch: "volta", VRAMGb: 32, GPUCount: 2, RAMGb: 128}, // dual-gpu path
		{Vendor: "amd", Arch: "rdna3", VRAMGb: 0.5, GPUCount: 1, RAMGb: 32},
		{Vendor: "none", Arch: "none", RAMGb: 16},
		{}, // zero facts: still a decision, still gated
	} {
		v := Classify(f)
		if v.RAMTier == "" {
			t.Errorf("Classify(%+v) returned profile %q with an EMPTY ram_tier", f, v.Profile)
		}
		if v.RAMTier != RAMTier(f.RAMGb) {
			t.Errorf("Classify(%+v) ram_tier = %q, want %q", f, v.RAMTier, RAMTier(f.RAMGb))
		}
	}
}
